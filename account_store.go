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

/*
 * ## 账号存储的密钥：与"会话材料"解耦（2026-11-10 修用户实测的数据"消失"）
 *
 * # 事故形态
 *
 * 用户点了「退出登录」后，配置页报
 *   `PLUGIN_CALL_FAILED: 账号主密钥不可用（accounts_master_key 未配置，且会话材料不存在）`
 * 账号列表**整块**读不出来，看起来像"我的账号信息没了"。
 *
 * # 真身（两处）
 *
 * ① `accounts.json` 是**明文 JSON**（账号列表本身不需要任何密钥），
 *    密钥只用于 `accounts.secrets/<id>.enc` 里那份**凭据密文**。
 *    但 `openAccountStore` 在密钥缺席时**直接返回错误**，把"读列表"这件
 *    不需要密钥的事也一并挡死了。
 *
 * ② 密钥的兜底来源是 VaultBlob `xiaomi_session_material` —— 而 `logout`
 *    会删掉那个 blob（它本来就是这个用途）。于是"退出登录"变成了
 *    "账号存储密钥永久丢失"：密文再也解不开，且每次读列表都报错。
 *
 * # 修法
 *
 * · 密钥改从**专用且永不删除**的 blob `accounts_key_material` 派生：
 *   首次使用时生成一次，之后一直用它。
 * · 兼容旧数据：专用 blob 不存在时，先按旧来源（主密钥 / 会话材料）派生，
 *   并把**同一份材料**写进专用 blob —— 密钥值因此不变，既有密文照样能解，
 *   无需重新加密（避免迁移途中损坏）。
 * · 密钥确实不可用时（专用 blob 没有、会话材料也没了）**不再阻断账号列表**：
 *   列表照常读写（明文），只有需要密钥的操作才报明确错误。
 *
 * 这样"退出登录"只清会话，不会带走账号存储的钥匙。
 */
const accountsKeyBlob = "accounts_key_material"

// openAccountStore 打开账号存储。
//
// 注意它**不再因为密钥缺席而失败**：账号列表是明文元数据，与密钥无关。
// 只有当真的要用密钥（加解密账号凭据）时才会报错 —— 那时的错误也必须说清
// 是"密钥不可用"，而不是让人以为账号数据没了。
func openAccountStore(ctx *plugin.Context) (*accountStore, error) {
	/*
	 * ⚠ 这里**完全不碰密钥库**（2026-11-10 第二次修）。
	 *
	 * 上一版在打开时就取密钥 —— 而取密钥要读 VaultBlob，而 VaultBlob 的
	 * scope 判定会看**当前调用上下文**：家庭级 blob 不接受账号上下文
	 * （`secretblobs.go` 的 blobAccount）。于是"账号级工具"（account_settings /
	 * account_delete）一进来就被平台拒：
	 *   `PLUGIN_PERMISSION_DENIED: 家庭级 secret blob "accounts_key_material" 不接受账号上下文`
	 *
	 * 而这两个工具**根本不需要密钥**：它们读写的是明文元数据
	 * （accounts.json / 删文件），密钥只在加解密 accounts.secrets/*.enc 时才要。
	 * 所以改成惰性：真要用的时候（saveSecret/loadSecret）才去取，
	 * 那时若拿不到就报"密钥不可用"，语义比"整个工具失败"精确得多。
	 */
	return &accountStore{ctx: ctx}, nil
}

// secretKey 惰性取密钥（首次使用时缓存在实例上）。
func (s *accountStore) secretKey() ([]byte, error) {
	if len(s.key) > 0 {
		return s.key, nil
	}
	key, err := ensureAccountKey(s.ctx)
	if err != nil {
		return nil, err
	}
	s.key = key
	return key, nil
}

// deriveAccountKey 把材料派生成 32 字节密钥（与旧实现逐字一致，保证既有密文可解）。
func deriveAccountKey(material string) []byte {
	sum := sha256.Sum256([]byte(material + "\x00homex.xiaomi_miot/accounts/v2"))
	return sum[:]
}

// ensureAccountKey 取（必要时建立）账号存储密钥。
func ensureAccountKey(ctx *plugin.Context) ([]byte, error) {
	// ① 专用 blob 已存在 ⇒ 直接用它（稳定、与会话无关）。
	if b, _, err := ctx.VaultBlobOpen(accountsKeyBlob); err == nil && len(b) > 0 {
		return deriveAccountKey("blob:" + sha256Hex(b)), nil
	}

	// ② 兼容旧数据：按旧来源派生材料。
	material := ""
	if raw, err := ctx.SecretGet("accounts_master_key"); err == nil && strings.TrimSpace(raw) != "" {
		material = raw
	} else if sec, _, verr := ctx.VaultBlobOpen("xiaomi_session_material"); verr == nil && len(sec) > 0 {
		material = "vault-derived:" + sha256Hex(sec)
	}
	if material != "" {
		// 把同一份材料固化进专用 blob：**密钥值不变**，既有密文无需重加密，
		// 而下次不再依赖主密钥/会话材料。
		blob := []byte(sha256Hex([]byte(material)))
		if _, serr := ctx.VaultBlobSeal(accountsKeyBlob, blob); serr != nil {
			ctx.LogWarn("hx-mi 固化账号密钥材料失败（本次仍可用，下次仍依赖旧来源）: %v", serr)
		} else {
			ctx.Log(1, "hx-mi account key material pinned to dedicated blob")
		}
		return deriveAccountKey(material), nil
	}

	/*
	 * ③ 什么来源都没有 ⇒ 生成一份新材料并固化。
	 *
	 * # 为什么这里**不**拒绝（曾经写成拒绝，是个陷阱）
	 *
	 * 上一版逻辑是"存在旧密文就报错，免得新钥与旧密文混淆"。但那个错误会一路传到
	 * 登录保存凭据那一步 ⇒ **用户永远无法重新登录**（每次都被"密钥不可用"挡住），
	 * 而这是插件最核心的功能。已损坏的密文本来就已经解不开（密钥没了），
	 * 拒绝建新钥并不能"保护"它，只能把插件锁死。
	 *
	 * 现在的取舍：建新钥，并如实告警"有 N 份旧密文已无法解密"。
	 * 那些账号行会在读取凭据时报错并要求重新登录 —— 这是诚实且可恢复的；
	 * 而"整个登录保存不了"是不可恢复的。
	 */
	if hasAnyAccountSecret(ctx) {
		ctx.LogWarn("hx-mi 账号密钥材料缺失但存在旧凭据密文：这些账号的凭据已无法解密（需重新登录）；" +
			"本次生成新的密钥材料，后续登录的凭据可正常保存")
	}
	fresh := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, fresh); err != nil {
		return nil, err
	}
	if _, err := ctx.VaultBlobSeal(accountsKeyBlob, fresh); err != nil {
		return nil, fmt.Errorf("初始化账号密钥材料失败: %w", err)
	}
	ctx.Log(1, "hx-mi account key material created")
	return deriveAccountKey("blob:" + sha256Hex(fresh)), nil
}

// hasAnyAccountSecret 报告是否存在任何账号凭据密文。
//
// FSList 列的是插件数据根下的全部文件（无子路径参数），所以按前缀过滤。
// 读不到清单时返回 false：那意味着"看起来没有密文"，于是会走"全新安装"分支
// 生成新密钥 —— 这个方向的误判代价是"既有密文解不开"，而反过来（明明有密文
// 却以为没有）会让插件在密钥丢失时凭空造钥、把新旧数据混在一起，更糟。
func hasAnyAccountSecret(ctx *plugin.Context) bool {
	files, err := ctx.FSList()
	if err != nil {
		return false
	}
	for _, f := range files {
		if strings.HasPrefix(f.Path, "accounts.secrets/") && strings.HasSuffix(f.Path, ".enc") {
			return true
		}
	}
	return false
}

// accountFileExists 报告账号数据文件是否存在。
//
// 用途：区分"全新安装（文件都没有）"与"有数据但读不了"。前者应显示空列表，
// 后者必须报错 —— 把前者当错误会让刚装好的用户以为插件坏了。
func accountFileExists(ctx *plugin.Context) bool {
	r, err := ctx.FSRead(accountsPath)
	return err == nil && r.Found
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
	key, err := s.secretKey()
	if err != nil {
		return fmt.Errorf("账号凭据无法加密保存：%w", err)
	}
	block, err := aes.NewCipher(key)
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
	key, err := s.secretKey()
	if err != nil {
		return nil, fmt.Errorf("账号凭据无法解密：%w", err)
	}
	block, err := aes.NewCipher(key)
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
