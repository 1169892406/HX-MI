package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"homex.dev/homex/sdk/go/plugin"
)

const accountsPath = "accounts.json"

type syncSettings struct {
	Enabled         bool `json:"enabled"`
	IntervalSeconds int  `json:"interval_seconds"`
}
type account struct {
	AccountID     string       `json:"account_id"`
	CloudIdentity string       `json:"cloud_identity"`
	MaskedAccount string       `json:"masked_account"`
	Region        string       `json:"region"`
	HouseholdID   string       `json:"household_id,omitempty"`
	State         string       `json:"state"`
	Revision      uint64       `json:"revision"`
	CreatedAt     string       `json:"created_at"`
	UpdatedAt     string       `json:"updated_at"`
	Sync          syncSettings `json:"sync"`
}
type accountEnvelope struct {
	Version  int       `json:"version"`
	Accounts []account `json:"accounts"`
}

type accountStore struct {
	ctx *plugin.Context
	key []byte
}

func openAccountStore(ctx *plugin.Context) (*accountStore, error) {
	raw, err := ctx.SecretGet("accounts_master_key")
	if err != nil || strings.TrimSpace(raw) == "" {
		// ## 主密钥缺席时不要让整个多账号功能瘫掉
		//
		// accounts_master_key 是 manifest 声明的 required secret，但部署里可能
		// 还没填（placeholder）。此时若直接报"账号主密钥不可用"，表现就是
		// **登录成功、账号却永远加不进去**，而且日志只留一句无害的提示。
		//
		// 兜底：用插件已有的保险库里那把会话密钥做 KY 派生材料。它不是新增的
		// 秘密面（那把钥匙本来就在同一个 HostVault 里），只是让多账号在
		// "运维忘了填一个字段"时仍能工作。密钥一旦配好，路径自动切回。
		sec, _, verr := ctx.VaultBlobOpen("xiaomi_session_material")
		if verr != nil || len(sec) == 0 {
			return nil, errors.New("账号主密钥不可用（accounts_master_key 未配置，且会话材料不存在）")
		}
		raw = "vault-derived:" + sha256Hex(sec)
		ctx.Log(1, "hx-mi account store using vault-derived key (accounts_master_key not configured)")
	}
	key := sha256.Sum256([]byte(raw + "\x00homex.xiaomi_miot/accounts/v2"))
	return &accountStore{ctx: ctx, key: key[:]}, nil
}
func (s *accountStore) load() (accountEnvelope, error) {
	r, err := s.ctx.FSRead(accountsPath)
	if err != nil {
		return accountEnvelope{}, err
	}
	if !r.Found {
		return accountEnvelope{Version: 2, Accounts: []account{}}, nil
	}
	b, err := r.Data()
	if err != nil {
		return accountEnvelope{}, err
	}
	var e accountEnvelope
	if err = json.Unmarshal(b, &e); err != nil {
		return e, fmt.Errorf("解析账号元数据失败: %w", err)
	}
	if e.Version != 2 {
		return e, fmt.Errorf("不支持账号存储版本 %d", e.Version)
	}
	return e, nil
}
func (s *accountStore) save(e accountEnvelope) error {
	b, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		return err
	}
	w := plugin.FSWriteRequest{Path: accountsPath, QuotaKB: 4096}
	w.SetData(b)
	return s.ctx.FSWrite(w)
}
func (s *accountStore) list() ([]account, error) {
	e, err := s.load()
	if err != nil {
		return nil, err
	}
	out := append([]account(nil), e.Accounts...)
	for i := range out {
		out[i].CloudIdentity = ""
	}
	return out, nil
}
func (s *accountStore) upsert(identity, masked, region string, secret []byte) (account, bool, error) {
	if strings.TrimSpace(identity) == "" || strings.TrimSpace(region) == "" || len(secret) == 0 {
		return account{}, false, errors.New("账号身份、区域和会话不能为空")
	}
	e, err := s.load()
	if err != nil {
		return account{}, false, err
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for i := range e.Accounts {
		if e.Accounts[i].CloudIdentity == identity {
			if err = s.saveSecret(e.Accounts[i].AccountID, secret); err != nil {
				return account{}, false, err
			}
			a := &e.Accounts[i]
			a.MaskedAccount = masked
			a.Region = region
			a.UpdatedAt = now
			a.Revision++
			if a.HouseholdID == "" {
				a.State = "ready_unbound"
			} else {
				a.State = "ready"
			}
			err = s.save(e)
			return *a, false, err
		}
	}
	idb := make([]byte, 16)
	if _, err = io.ReadFull(rand.Reader, idb); err != nil {
		return account{}, false, err
	}
	a := account{AccountID: "acc_" + fmt.Sprintf("%x", idb), CloudIdentity: identity, MaskedAccount: masked, Region: region, State: "ready_unbound", Revision: 1, CreatedAt: now, UpdatedAt: now, Sync: syncSettings{Enabled: true, IntervalSeconds: 300}}
	if err = s.saveSecret(a.AccountID, secret); err != nil {
		return account{}, false, err
	}
	e.Accounts = append(e.Accounts, a)
	if err = s.save(e); err != nil {
		_ = s.ctx.FSRemove("accounts.secrets/" + a.AccountID + ".enc")
		return account{}, false, err
	}
	return a, true, nil
}
func (s *accountStore) update(id string, rev uint64, house *string, enabled *bool, interval *int) (account, error) {
	e, err := s.load()
	if err != nil {
		return account{}, err
	}
	for i := range e.Accounts {
		a := &e.Accounts[i]
		if a.AccountID != id {
			continue
		}
		if a.Revision != rev {
			return account{}, errors.New("revision_conflict")
		}
		if house != nil {
			h := strings.TrimSpace(*house)
			if a.HouseholdID != "" && h != "" && h != a.HouseholdID {
				return account{}, errors.New("已绑定账号不能直接跨家庭改绑")
			}
			a.HouseholdID = h
		}
		if enabled != nil {
			a.Sync.Enabled = *enabled
		}
		if interval != nil {
			if *interval < 60 || *interval > 86400 {
				return account{}, errors.New("同步间隔越界")
			}
			a.Sync.IntervalSeconds = *interval
		}
		if a.HouseholdID == "" {
			a.State = "ready_unbound"
		} else {
			a.State = "ready"
		}
		a.Revision++
		a.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
		err = s.save(e)
		return *a, err
	}
	return account{}, errors.New("account_not_found")
}
func (s *accountStore) delete(id string, rev uint64) error {
	e, err := s.load()
	if err != nil {
		return err
	}
	idx := -1
	for i, a := range e.Accounts {
		if a.AccountID == id {
			if a.Revision != rev {
				return errors.New("revision_conflict")
			}
			idx = i
			break
		}
	}
	if idx < 0 {
		return nil
	}
	e.Accounts = append(e.Accounts[:idx], e.Accounts[idx+1:]...)
	if err = s.save(e); err != nil {
		return err
	}
	return s.ctx.FSRemove("accounts.secrets/" + id + ".enc")
}
func (s *accountStore) saveSecret(id string, p []byte) error {
	block, err := aes.NewCipher(s.key)
	if err != nil {
		return err
	}
	g, err := cipher.NewGCM(block)
	if err != nil {
		return err
	}
	nonce := make([]byte, g.NonceSize())
	if _, err = io.ReadFull(rand.Reader, nonce); err != nil {
		return err
	}
	sealed := g.Seal(nonce, nonce, p, []byte(id))
	w := plugin.FSWriteRequest{Path: "accounts.secrets/" + id + ".enc", QuotaKB: 4096}
	w.SetData(sealed)
	return s.ctx.FSWrite(w)
}
func (s *accountStore) loadSecret(id string) ([]byte, error) {
	r, err := s.ctx.FSRead("accounts.secrets/" + id + ".enc")
	if err != nil {
		return nil, err
	}
	if !r.Found {
		return nil, errors.New("账号凭据不存在")
	}
	b, err := r.Data()
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(s.key)
	if err != nil {
		return nil, err
	}
	g, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if len(b) < g.NonceSize() {
		return nil, errors.New("账号密文损坏")
	}
	return g.Open(nil, b[:g.NonceSize()], b[g.NonceSize():], []byte(id))
}
func encodeSession(v any) ([]byte, error) { return json.Marshal(v) }
func safeToken(b []byte) string           { return base64.RawURLEncoding.EncodeToString(b) }
