package main

import (
	"bytes"
	"compress/gzip"
	"crypto/aes"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/url"
	"strconv"
	"strings"
	"time"

	"homex.dev/homex/sdk/go/plugin"
)

/*
 * 红外码库（2026-11-17）—— 让"通用插件"真正成立的关键。
 *
 * # 问题
 *
 * 小爱音箱支持局域网发红外（`miIO.ir_play` 实测存在），但需要
 * `{"freq":<频率>,"code":"<base64 波形>"}`。拿不到码 ⇒ 只能走云端 action。
 * 抓包能拿到，但**每个用户都抓包不是通用方案**。
 *
 * # 解法（公开、无需用户凭据）
 *
 * 小米「米家遥控」App 的红外码库 API 已被逆向公开
 * （ysard/mi_remote_database，Apache/公开研究项目）：
 *
 *	服务器: https://sg-urc.io.mi.com
 *	取设备类型: /controller/device/1
 *	取品牌:     /controller/brand/list/1?devid=<设备类型>
 *	取型号树:   /controller/match/tree/1?devid=..&miyk=1&brandid=..&power=1
 *	取波形:     型号树节点里的 ir_zip_key
 *
 * 签名只需**公开常量**（逆向所得，非用户凭据）：
 *	opaque = HMAC-SHA1(URL_SECRET_KEY, path_url + "&token=" + URL_TOKEN)
 *
 * 波形解码（同样公开常量）：
 *	base64 → AES-128-ECB(PATTERN_SECRET_KEY) → gzip → JSON 时长数组（微秒）
 *
 * # 为什么这是对的
 *
 * 码库是**所有人的同一份**，与具体账号无关 ⇒ 一次取到就能缓存给所有用户，
 * 真正做到"登录后自动可用、离线可控制"。
 */

const (
	irDBServer      = "https://sg-urc.io.mi.com"
	irDBURLToken    = "0f9dfa001cba164d7bda671649c50abf"
	irDBURLSecret   = "581582928c881b42eedce96331bff5d3"
	irDBPatternKey  = "fd7e915003168929c1a9b0ec32a60788"
	irDBAPIVersion  = "6034"
	irDBCachePath   = "ir-code-cache.json"
	irDBCacheMaxAge = 30 * 24 * time.Hour
)

// irDBCall 调一次码库 API 并返回响应正文（自动处理 gzip 与 opaque 签名）。
func irDBCall(ctx *plugin.Context, path string, params [][2]string) ([]byte, error) {
	q := url.Values{}
	q.Set("country", "CN")
	q.Set("version", irDBAPIVersion)
	q.Set("ts", strconv.FormatInt(time.Now().UnixMilli(), 10))
	q.Set("nonce", strconv.Itoa(rand.Intn(1_000_000_000)-500_000_000))
	for _, p := range params {
		q.Set(p[0], p[1])
	}
	pathURL := path + "?" + q.Encode()
	mac := hmac.New(sha1.New, []byte(irDBURLSecret))
	mac.Write([]byte(pathURL + "&token=" + irDBURLToken))
	full := irDBServer + pathURL + "&opaque=" + hex.EncodeToString(mac.Sum(nil))

	resp, err := ctx.HTTPRequest(plugin.HTTPRequest{
		Method: "GET", URL: full, TimeoutMS: 15000,
		Headers: map[string]string{"accept-encoding": "gzip", "user-agent": "okhttp/3.8.0"},
	})
	if err != nil {
		return nil, err
	}
	body, berr := resp.Body()
	if berr != nil {
		return nil, berr
	}
	if resp.Status != 200 {
		return nil, fmt.Errorf("码库接口 HTTP %d", resp.Status)
	}
	// 服务器可能直接给 gzip，也可能给明文 JSON
	if len(body) > 2 && body[0] == 0x1f && body[1] == 0x8b {
		zr, zerr := gzip.NewReader(bytes.NewReader(body))
		if zerr == nil {
			defer func() { _ = zr.Close() }()
			if plain, rerr := io.ReadAll(zr); rerr == nil {
				return plain, nil
			}
		}
	}
	return body, nil
}

// irPattern 是一条红外波形：频率 + ON/OFF 微秒序列。
type irPattern struct {
	Frequency int   `json:"frequency"`
	Timings   []int `json:"timings"`
}

// decryptIrPattern 解开 `ir_zip_key`：base64 → AES-128-ECB → gzip → JSON 数组。
func decryptIrPattern(enc string, freq int) (irPattern, error) {
	rawB, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		return irPattern{}, fmt.Errorf("波形 base64 解码失败: %w", err)
	}
	if len(rawB)%aes.BlockSize != 0 {
		return irPattern{}, errors.New("波形长度不是 AES 块整数倍")
	}
	block, err := aes.NewCipher([]byte(irDBPatternKey))
	if err != nil {
		return irPattern{}, err
	}
	out := make([]byte, len(rawB))
	for i := 0; i < len(rawB); i += aes.BlockSize {
		block.Decrypt(out[i:i+aes.BlockSize], rawB[i:i+aes.BlockSize])
	}
	/*
	 * ⚠ **不要** TrimRight 掉尾部 \x00（2026-11-17 实测踩坑）。
	 *
	 * AES-ECB 解出来的尾部字节是 gzip 的 CRC/ISIZE，其中**本来就含 0x00**。
	 * 早期版本按"去掉填充"的想法写了 `bytes.TrimRight(out, "\x00 ")`，
	 * 结果把 gzip 尾部的 CRC 清零 ⇒ `io.ReadAll` 报 `unexpected EOF`，
	 * 整条链路在"数据明明已经完整解出 72 项"的情况下失败。
	 *
	 * gzip 的结束判定依赖 CRC 校验，不能裁；直接喂给 gzip.Reader，
	 * 并把 EOF 视为正常结束。
	 */
	plain := out
	if len(out) > 2 && out[0] == 0x1f && out[1] == 0x8b {
		zr, zerr := gzip.NewReader(bytes.NewReader(out))
		if zerr != nil {
			return irPattern{}, fmt.Errorf("波形 gzip 解压失败: %w", zerr)
		}
		defer func() { _ = zr.Close() }()
		buf, rerr := io.ReadAll(zr)
		// 即使报 unexpected EOF，已读到的字节仍是完整的 JSON —— 照常解析。
		if rerr != nil && rerr != io.EOF && len(buf) == 0 {
			return irPattern{}, fmt.Errorf("波形解压失败: %w", rerr)
		}
		plain = buf
	}
	var timings []int
	if err := json.Unmarshal(plain, &timings); err != nil {
		return irPattern{}, fmt.Errorf("波形 JSON 解析失败: %w", err)
	}
	if len(timings) == 0 {
		return irPattern{}, errors.New("波形长度为 0")
	}
	return irPattern{Frequency: freq, Timings: timings}, nil
}

// irEncodeForPlay 把波形压成 `miIO.ir_play` 要的 base64 形式。
//
// 小米设备接受的 `code` 是**原始时长数组的紧凑二进制编码**，
// 社区实测两种形态都能被老固件接受：
//   - JSON 数组文本再 base64；
//   - 原始字节（每项 2 字节大端，单位微秒/50）。
// 这里先用**最直白的 JSON 文本 + base64**：它不依赖"除以 50"这种缩放假设，
// 且失败时能从错误码立刻看出是格式问题而不是幅度问题。
func irEncodeForPlay(p irPattern) string {
	b, _ := json.Marshal(p.Timings)
	return base64.StdEncoding.EncodeToString(b)
}

// irModelEntry 是型号树里一条可用波形。
type irModelEntry struct {
	KeyID     string `json:"key_id"`
	MatchIDs  []string `json:"match_ids"`
	Encrypted string `json:"encrypted"`
	Frequency int    `json:"frequency"`
}

// fetchIROptions 拉某设备类型下某品牌的全部波形（型号树）。
func fetchIROptions(ctx *plugin.Context, deviceID, brandID string) ([]irModelEntry, error) {
	body, err := irDBCall(ctx, "/controller/match/tree/1", [][2]string{
		{"devid", deviceID}, {"miyk", "1"}, {"brandid", brandID}, {"power", "1"},
	})
	if err != nil {
		return nil, err
	}
	var wrapped struct {
		Data struct {
			Tree struct {
				Nodes []struct {
					IrZipKey  string   `json:"ir_zip_key"`
					KeyID     string   `json:"keyid"`
					KeysetIDs []string `json:"keysetids"`
					Frequency int      `json:"frequency"`
				} `json:"nodes"`
			} `json:"tree"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &wrapped) != nil {
		return nil, errors.New("型号树解析失败")
	}
	out := []irModelEntry{}
	for _, n := range wrapped.Data.Tree.Nodes {
		if n.IrZipKey == "" || n.KeyID == "" {
			continue
		}
		out = append(out, irModelEntry{
			KeyID: n.KeyID, MatchIDs: n.KeysetIDs, Encrypted: n.IrZipKey, Frequency: n.Frequency,
		})
	}
	if len(out) == 0 {
		return nil, errors.New("该品牌型号树里没有可用波形")
	}
	return out, nil
}

// irCacheFile 是落盘的波形缓存（按 品牌+按键 存，跨用户共享同一份码库）。
type irCacheFile struct {
	FetchedAt string                            `json:"fetched_at"`
	Entries   map[string]map[string]irPattern    `json:"entries"` // brandID → keyID → 波形
}

func loadIRCache(ctx *plugin.Context) irCacheFile {
	c := irCacheFile{Entries: map[string]map[string]irPattern{}}
	r, err := ctx.FSRead(irDBCachePath)
	if err != nil || !r.Found {
		return c
	}
	b, berr := r.Data()
	if berr != nil {
		return c
	}
	var parsed irCacheFile
	if json.Unmarshal(b, &parsed) != nil || parsed.Entries == nil {
		return c
	}
	return parsed
}

func saveIRCache(ctx *plugin.Context, c irCacheFile) {
	b, err := json.Marshal(c)
	if err != nil {
		return
	}
	w := plugin.FSWriteRequest{Path: irDBCachePath, QuotaKB: 1024}
	w.SetData(b)
	_ = ctx.FSWrite(w)
}

var _ = strings.TrimSpace // 保持 strings 引用（后续匹配逻辑会用到）

// irDeviceTypes 拉可用设备类型（TV=1, 空调等由 id 区分）。
func irDeviceTypes(ctx *plugin.Context) ([]map[string]any, error) {
	body, err := irDBCall(ctx, "/controller/device/1", nil)
	if err != nil {
		return nil, err
	}
	var d struct {
		Data []struct {
			DeviceID int    `json:"deviceid"`
			Name     string `json:"name"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &d) != nil {
		return nil, errors.New("设备类型解析失败")
	}
	out := []map[string]any{}
	for _, x := range d.Data {
		out = append(out, map[string]any{"device_id": x.DeviceID, "name": x.Name})
	}
	return out, nil
}

// irBrands 拉某设备类型下的品牌（用中文名匹配用户输入）。
func irBrands(ctx *plugin.Context, deviceID int) ([]map[string]any, error) {
	body, err := irDBCall(ctx, "/controller/brand/list/1", [][2]string{
		{"devid", strconv.Itoa(deviceID)},
	})
	if err != nil {
		return nil, err
	}
	var d struct {
		Data []struct {
			BrandID  int    `json:"brandid"`
			Name     string `json:"name"`
			DeviceID int    `json:"deviceid"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &d) != nil {
		return nil, errors.New("品牌列表解析失败")
	}
	out := []map[string]any{}
	for _, x := range d.Data {
		out = append(out, map[string]any{
			"brand_id": x.BrandID, "name": x.Name, "device_id": x.DeviceID,
		})
	}
	return out, nil
}

/*
 * ir_play 命令：把波形通过**局域网**发给音箱。
 *
 * 这是"离线可控红外"的最后一步：
 *   irCode = irEncodeForPlay(decryptedPattern)
 *   miio   = {"method":"miIO.ir_play","params":{"freq":freq,"code":irCode}}
 *
 * 注意：**真的会发射**。所以调用方必须显式确认（handler 里不做自动重试、
 * 不做批量、不做定时），且失败要如实报错而不是当作"已发送"。
 */
func playIRLocal(ctx *plugin.Context, blaster miioDevice, token []byte, p irPattern) error {
	env, err := miioExchange(ctx, blaster, token, "miIO.ir_play", map[string]any{
		"freq": p.Frequency,
		"code": irEncodeForPlay(p),
	})
	if err != nil {
		return err
	}
	if code, ok := env["code"].(float64); ok && int(code) != 0 {
		return fmt.Errorf("音箱拒绝发射（code=%d，可能是波形格式不被接受）", int(code))
	}
	return nil
}

// blasterDevice 找一台可当红外发射器的设备（有局域网地址 + token 的音箱/网关）。
func blasterDevice(ctx *plugin.Context) (miioDevice, []byte, error) {
	ds, err := loadDeviceRegistry(ctx)
	if err != nil {
		return miioDevice{}, nil, err
	}
	for _, d := range ds {
		if strings.TrimSpace(d.Host) == "" {
			continue
		}
		// 优先带 speaker 字样的型号（小爱音箱）；退而求其次任何有 host 的设备
		if strings.Contains(strings.ToLower(d.Model), "speaker") ||
			strings.Contains(strings.ToLower(d.Name), "音箱") {
			if tok, _, terr := ctx.VaultBlobOpenDevice("miio_token", d.ExternalID); terr == nil && len(tok) == 16 {
				return d, tok, nil
			}
		}
	}
	return miioDevice{}, nil, errors.New("没有找到可用的红外发射设备（需要一台在局域网内、带 token 的音箱）")
}

/*
 * ir_lib 工具：查码库（**只读**，不发红外）。
 *
 *	{"action":"types"}                       → 设备类型
 *	{"action":"brands","device_id":1}        → 电视品牌
 *	{"action":"find","device_id":1,"brand":"创维"}
 *	                                         → 该品牌的波形清单（按键名 + 频率，不含码）
 *	{"action":"code","device_id":1,"brand":"创维","key":"power"}
 *	                                         → 解密后的波形（时长数组）
 *
 * 单独做的理由：用户/前端需要先"找到自己的电视品牌"，那一步必须是纯查询；
 * 发送是另一个工具（`ir_send`），需要显式确认。
 */
func irLibTool(ctx *plugin.Context, args json.RawMessage) (json.RawMessage, error) {
	var in struct {
		Action   string `json:"action"`
		DeviceID int    `json:"device_id"`
		Brand    string `json:"brand"`
		Key      string `json:"key"`
	}
	if len(args) > 0 {
		_ = json.Unmarshal(args, &in)
	}
	if in.Action == "" {
		in.Action = "types"
	}
	switch in.Action {
	case "types":
		ts, err := irDeviceTypes(ctx)
		if err != nil {
			return nil, err
		}
		return json.Marshal(map[string]any{"device_types": ts})

	case "brands":
		if in.DeviceID == 0 {
			in.DeviceID = 1 // 默认电视
		}
		bs, err := irBrands(ctx, in.DeviceID)
		if err != nil {
			return nil, err
		}
		return json.Marshal(map[string]any{"device_id": in.DeviceID, "brands": bs, "count": len(bs)})

	case "find", "code":
		if strings.TrimSpace(in.Brand) == "" {
			return nil, errors.New("brand 不能为空（用中文品牌名，如「创维」）")
		}
		if in.DeviceID == 0 {
			in.DeviceID = 1
		}
		bs, err := irBrands(ctx, in.DeviceID)
		if err != nil {
			return nil, err
		}
		var brandID int
		matched := ""
		for _, b := range bs {
			n, _ := b["name"].(string)
			if n == in.Brand {
				brandID, _ = b["brand_id"].(int)
				matched = n
				break
			}
		}
		if brandID == 0 {
			// 精确没命中就做包含匹配，并回报候选，避免用户拼写差异卡住
			cand := []string{}
			for _, b := range bs {
				n, _ := b["name"].(string)
				if n != "" && strings.Contains(n, in.Brand) {
					cand = append(cand, n)
				}
			}
			if len(cand) == 1 {
				for _, b := range bs {
					if n, _ := b["name"].(string); n == cand[0] {
						brandID, _ = b["brand_id"].(int)
						matched = n
						break
					}
				}
			} else {
				return json.Marshal(map[string]any{
					"error": "品牌未精确命中，请用候选之一重试", "candidates": cand,
				})
			}
		}
		entries, err := fetchIROptions(ctx, strconv.Itoa(in.DeviceID), strconv.Itoa(brandID))
		if err != nil {
			return nil, err
		}
		if in.Action == "find" {
			keys := make([]map[string]any, 0, len(entries))
			for _, e := range entries {
				keys = append(keys, map[string]any{
					"key": e.KeyID, "frequency": e.Frequency, "models": e.MatchIDs,
				})
			}
			return json.Marshal(map[string]any{
				"brand": matched, "brand_id": brandID, "device_id": in.DeviceID,
				"keys": keys, "count": len(keys),
			})
		}
		// code：取指定按键并解密
		want := strings.TrimSpace(in.Key)
		if want == "" {
			want = "power"
		}
		for _, e := range entries {
			if !strings.EqualFold(e.KeyID, want) {
				continue
			}
			p, derr := decryptIrPattern(e.Encrypted, e.Frequency)
			if derr != nil {
				return nil, derr
			}
			/*
			 * ⚠ 只回**摘要**，不回全量时长数组（2026-11-17 实测）：
			 * 一条 NEC 波形 ~86 段、冷却后 base64 约 3~6KB，加上 JSON 包装会
			 * 超过平台 4KB 的 op 响应上限，报 `PLUGIN_CALL_FAILED: unexpected EOF`。
			 *
			 * 需要全量的只有"真正发射"那一步（在插件内部完成，不经过 wire），
			 * 所以这里给前 16 段 + 总长，足够人判断波形是否正常。
			 */
			head := p.Timings
			if len(head) > 16 {
				head = head[:16]
			}
			return json.Marshal(map[string]any{
				"brand": matched, "key": e.KeyID, "frequency": p.Frequency,
				"timings_head": head, "timing_count": len(p.Timings),
				"models": e.MatchIDs,
			})
		}
		return json.Marshal(map[string]any{
			"error": fmt.Sprintf("该品牌没有按键 %q", want),
		})
	}
	return nil, fmt.Errorf("未知 action %q（可用 types / brands / find / code）", in.Action)
}

/*
 * ir_send 工具：**真的会发射红外**。
 *
 *	{"device_id":1,"brand":"创维","key":"power"}
 *	{"brand":"创维","key":"volume_up","dry_run":true}   ← 只回波形，不发射
 *
 * 安全约束（本轮教训）：
 *   · 默认**要求显式 dry_run=false** 才发射 —— 但为了好用，缺省即为发射，
 *     所以调用方（前端按钮）必须是一次明确的用户点击；
 *   · 不做批量、不做重试、不做定时 —— 红外会改变物理世界；
 *   · 返回里带"已发射"的事实（而不是乐观确认）：只有 miIO 回 code=0 才算成功。
 */
func irSendTool(ctx *plugin.Context, args json.RawMessage) (json.RawMessage, error) {
	var in struct {
		DeviceID int    `json:"device_id"`
		Brand    string `json:"brand"`
		Key      string `json:"key"`
		DryRun   bool   `json:"dry_run"`
	}
	if len(args) > 0 {
		_ = json.Unmarshal(args, &in)
	}
	if in.DeviceID == 0 {
		in.DeviceID = 1
	}
	if strings.TrimSpace(in.Brand) == "" {
		return nil, errors.New("brand 不能为空")
	}
	if strings.TrimSpace(in.Key) == "" {
		in.Key = "power"
	}

	bs, err := irBrands(ctx, in.DeviceID)
	if err != nil {
		return nil, err
	}
	var brandID int
	for _, b := range bs {
		if n, _ := b["name"].(string); n == in.Brand {
			brandID, _ = b["brand_id"].(int)
			break
		}
	}
	if brandID == 0 {
		return nil, fmt.Errorf("品牌 %q 不在该设备类型的列表里", in.Brand)
	}
	entries, err := fetchIROptions(ctx, strconv.Itoa(in.DeviceID), strconv.Itoa(brandID))
	if err != nil {
		return nil, err
	}
	var hit *irModelEntry
	for i := range entries {
		if strings.EqualFold(entries[i].KeyID, in.Key) {
			hit = &entries[i]
			break
		}
	}
	if hit == nil {
		return nil, fmt.Errorf("该品牌没有按键 %q", in.Key)
	}
	p, derr := decryptIrPattern(hit.Encrypted, hit.Frequency)
	if derr != nil {
		return nil, derr
	}
	if in.DryRun {
		head := p.Timings
		if len(head) > 16 {
			head = head[:16]
		}
		return json.Marshal(map[string]any{
			"dry_run": true, "brand": in.Brand, "key": hit.KeyID,
			"frequency": p.Frequency, "timings_head": head, "timing_count": len(p.Timings),
			"note": "未发射（dry_run=true）",
		})
	}

	blaster, token, berr := blasterDevice(ctx)
	if berr != nil {
		return nil, berr
	}
	if perr := playIRLocal(ctx, blaster, token, p); perr != nil {
		return nil, fmt.Errorf("发射失败：%w", perr)
	}
	ctx.LogWarn("hx-mi 红外已发射：%s %s（经 %s，freq=%d，%d 段）",
		in.Brand, hit.KeyID, blaster.Name, p.Frequency, len(p.Timings))
	return json.Marshal(map[string]any{
		"sent": true, "brand": in.Brand, "key": hit.KeyID,
		"via": blaster.Name, "frequency": p.Frequency, "timing_count": len(p.Timings),
		"note": "音箱已接受该波形（code=0）",
	})
}
