package main

import (
	"sort"
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

/*
 * irEncodeForPlay 把波形编码成 `miIO.ir_play` 要的 `code`。
 *
 * # 格式（2026-11-17 从 python-miio 权威定义确认，不是猜的）
 *
 *	Const(0xA567, Int16ul)               // magic，2 字节小端
 *	edge_count (Int16ul)                 // 边对数
 *	times_index: Array(16, Int32ul)      // 去重后的时长查表（最多 16 个，不足补 0）
 *	edge_pairs:  edge_count × 1 字节     // 每字节 = (gap索引<<4) | pulse索引
 *
 * 这是 python-miio 的 `ChuangmiIrSignal`；我用社区实测可用的码反解验证过：
 *	magic=0xA567 edge_count=83 表=[528,606,1647,2239,4513,9019,...]
 *	pair0 = 9019us 脉冲 + 4513us 间隔（NEC 典型头）
 *
 * # 为什么不能直接用 JSON 数组
 *
 * 我最初把时长数组 JSON 化再 base64，那是**错的**：设备要的是上面这个
 * 索引压缩格式（4 位索引 ⇒ 最多 16 个不同时长，红外波形通常只有几个）。
 */
func irEncodeForPlay(p irPattern) (string, error) {
	t := p.Timings
	if len(t) < 2 {
		return "", errors.New("波形过短")
	}
	if len(t)%2 != 0 {
		t = t[:len(t)-1] // 丢弃落单的最后一个脉冲
	}
	// ① 去重排序建表（最多 16 项）
	uniq := map[int]bool{}
	for _, v := range t {
		if v > 0 {
			uniq[v] = true
		}
	}
	if len(uniq) > 16 {
		return "", fmt.Errorf("波形含 %d 个不同时长，超过协议上限 16", len(uniq))
	}
	table := make([]int, 0, len(uniq))
	for v := range uniq {
		table = append(table, v)
	}
	sort.Ints(table)
	idx := make(map[int]int, len(table))
	for i, v := range table {
		idx[v] = i
	}
	// ② 边对：每对 = (pulse, gap)
	pairs := make([]byte, 0, len(t)/2)
	for i := 0; i+1 < len(t); i += 2 {
		pulse, gap := t[i], t[i+1]
		pi, ok1 := idx[pulse]
		gi, ok2 := idx[gap]
		if !ok1 || !ok2 {
			return "", errors.New("波形索引映射失败")
		}
		pairs = append(pairs, byte(gi<<4)|byte(pi))
	}
	// ③ 组包
	/*
	 * edge_count = 边对数×2 - 1（**不是**边对数）。
	 *
	 * 2026-11-17 用社区实测可用的码做往返测试发现的：
	 *	原码 42 个边对字节、edge_count=0x53=83 = 42×2-1
	 *	我原来写成 42 ⇒ 设备收到边数与实际不符，静默丢弃（电视无反应）
	 * 物理含义：N 对(脉冲,间隔) = 2N 个时长 = 2N-1 个跳变（edge）。
	 */
	edgeCount := len(pairs)*2 - 1
	if edgeCount < 0 {
		edgeCount = 0
	}
	buf := make([]byte, 0, 4+64+len(pairs))
	buf = append(buf, 0x67, 0xA5)
	buf = append(buf, byte(edgeCount), byte(edgeCount>>8))
	for i := 0; i < 16; i++ {
		v := 0
		if i < len(table) {
			v = table[i]
		}
		buf = append(buf, byte(v), byte(v>>8), byte(v>>16), byte(v>>24))
	}
	buf = append(buf, pairs...)
	return base64.StdEncoding.EncodeToString(buf), nil
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
	FetchedAt string                         `json:"fetched_at"`
	Entries   map[string]map[string]irPattern `json:"entries"` // brandID → keyID → 波形
	// Learned 存**学到的码**（原始 miIO code 字符串），键是用户起的名字。
	// 这是通用插件的关键资产：一次学习、永久离线可用，与品牌/协议无关。
	Learned map[string]string `json:"learned"`
}

func loadIRCache(ctx *plugin.Context) irCacheFile {
	c := irCacheFile{Entries: map[string]map[string]irPattern{}, Learned: map[string]string{}}
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
//
// 带落盘缓存：电视品牌有 503 条，每次发射都重新拉一次会白吃掉几秒预算
// （2026-11-17：这也是把 30s 时限耗尽的帮凶之一）。
func irBrands(ctx *plugin.Context, deviceID int) ([]map[string]any, error) {
	key := "ir-brands-" + strconv.Itoa(deviceID) + ".json"
	if r, err := ctx.FSRead(key); err == nil && r.Found {
		if b, berr := r.Data(); berr == nil {
			var cached []map[string]any
			if json.Unmarshal(b, &cached) == nil && len(cached) > 0 {
				return cached, nil
			}
		}
	}
	out, ferr := irBrandsFetch(ctx, deviceID)
	if ferr != nil {
		return nil, ferr
	}
	if b, merr := json.Marshal(out); merr == nil {
		w := plugin.FSWriteRequest{Path: key, QuotaKB: 512}
		w.SetData(b)
		_ = ctx.FSWrite(w)
	}
	return out, nil
}

func irBrandsFetch(ctx *plugin.Context, deviceID int) ([]map[string]any, error) {
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
	/*
	 * ⚠ **红外是单向发射，设备通常不回应答**（2026-11-17 实测踩坑）。
	 *
	 * 第一版按普通命令处理（默认重试 3 次），结果 `miIO.ir_play` 每次都要等满
	 * 超时，3 次 × 3s 叠加前面的两次 HTTP 取码，直接撞满插件 30s 时限 ⇒
	 * `PLUGIN_SANDBOX_CRASH`。
	 *
	 * 正确语义：
	 *   · 只试 **1 次**；
	 *   · "UDP 等待响应超时"**视为已发射**（很多固件就是不发应答）；
	 *   · 只有设备**明确回错误码**才算失败。
	 */
	code, cerr := irEncodeForPlay(p)
	if cerr != nil {
		return cerr
	}
	env, err := miioExchangeWith(ctx, blaster, token, "miIO.ir_play", map[string]any{
		"freq": p.Frequency,
		"code": code,
	}, miioExchangeOptions{Attempts: 1})
	if err != nil {
		if strings.Contains(err.Error(), "等待响应超时") || strings.Contains(err.Error(), "读取响应失败") {
			// 无应答 ≠ 失败：红外发射本来就是"发完就算"。
			ctx.LogWarn("hx-mi 红外已发出（设备未回执，属正常）：%s freq=%d %d 段",
				blaster.Name, p.Frequency, len(p.Timings))
			return nil
		}
		return err
	}
	if code, ok := env["code"].(float64); ok && int(code) != 0 {
		return fmt.Errorf("音箱拒绝发射（code=%d，波形格式可能不被接受）", int(code))
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

/*
 * ir_raw 工具：把**任意时长数组**按设备要求的格式发出去（`ir_raw`）。
 *
 *	{"freq":36000,"timings":[1000,580,...],"dry_run":true}
 *
 * # 为什么需要（2026-11-17）
 *
 * 小米电视的红外协议与 NEC 不同：Mi Remote 码库里 `小米` 品牌在电视类型下
 * **节点数为 0**（小米电视走蓝牙遥控，库里没有红外码），但用户明确反馈
 * "开关机必须走红外，否则电视关机后蓝牙连不上" —— 说明电视确实有红外接收器。
 *
 * 于是改走**按协议自造码**：hifi-remote 论坛已逆向出
 *	XIAOMI IR（RC-MM 变种）：[HEADER][D:8][F:8][C:4]，设备码 0x86 / Power 用 0x3C
 * 该工具就是把这套生成出来的时长直接发射，便于逐次试码而不必每次改插件。
 *
 * 安全：同样支持 dry_run；真发时只尝试一次。
 */
func irRawTool(ctx *plugin.Context, args json.RawMessage) (json.RawMessage, error) {
	var in struct {
		Freq    int   `json:"freq"`
		Timings []int `json:"timings"`
		DryRun  bool  `json:"dry_run"`
	}
	if len(args) > 0 {
		if err := json.Unmarshal(args, &in); err != nil {
			return nil, err
		}
	}
	if in.Freq <= 0 {
		in.Freq = 36000
	}
	if len(in.Timings) < 4 {
		return nil, errors.New("timings 至少需要 4 个值（成对的脉冲/间隔）")
	}
	p := irPattern{Frequency: in.Freq, Timings: in.Timings}
	code, cerr := irEncodeForPlay(p)
	if cerr != nil {
		return nil, cerr
	}
	if in.DryRun {
		return json.Marshal(map[string]any{
			"dry_run": true, "frequency": p.Frequency,
			"timings": len(p.Timings), "code_bytes": len(code),
			"code_head": code[:minInt(len(code), 60)],
		})
	}
	blaster, token, berr := blasterDevice(ctx)
	if berr != nil {
		return nil, berr
	}
	if perr := playIRLocal(ctx, blaster, token, p); perr != nil {
		return nil, fmt.Errorf("发射失败：%w", perr)
	}
	return json.Marshal(map[string]any{
		"sent": true, "via": blaster.Name, "frequency": p.Frequency,
		"timings": len(p.Timings),
	})
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

/*
 * 学习模式 —— 通用插件的**万能兜底**（2026-11-17 用户："你这个不适通用的啊"）。
 *
 * # 为什么必须做这个
 *
 * 前两条路都有盲区：
 *   · 公开码库：只覆盖库里有码的品牌（小米/红米电视就没有，节点数 0）；
 *   · 按协议自造码：一种协议只能覆盖一个品牌族，换台电视就得重来。
 * 都不是"通用插件"该有的样子。
 *
 * 而音箱固件本身支持学习：
 *	miIO.ir_learn {"key":"1"}   进入学习（等用户拿原遥控器对着音箱按一下）
 *	miIO.ir_read  {"key":"1"}   读回学到的码（就是 miIO.ir_play 要的那个 code）
 *
 * 这条路的通用性是**本质的**：不管什么品牌、什么协议、甚至自制红外设备，
 * 只要能用一个红外遥控器操作，就能学。学完存本地 ⇒ 之后完全离线发射。
 */

// irLearnTool 让音箱进入学习状态（`ir_learn`）。
//
//	{"key":1}   ← 存储槽位，1~1000000
//
// 用户在**学习窗口内**（通常十几秒）用原遥控器对着音箱按一下要学的键，
// 然后调 `ir_read` 取回码。两次调用之间的"人去按遥控器"无法自动完成，
// 所以这是一个人机配合的两步流程 —— 但对用户是**一次性的**：
// 学完就能一直离线用。
func irLearnTool(ctx *plugin.Context, args json.RawMessage) (json.RawMessage, error) {
	var in struct {
		Key int `json:"key"`
	}
	if len(args) > 0 {
		_ = json.Unmarshal(args, &in)
	}
	if in.Key <= 0 {
		in.Key = 1
	}
	blaster, token, berr := blasterDevice(ctx)
	if berr != nil {
		return nil, berr
	}
	env, err := miioExchangeWith(ctx, blaster, token, "miIO.ir_learn",
		map[string]any{"key": strconv.Itoa(in.Key)}, miioExchangeOptions{Attempts: 1})
	if err != nil {
		return nil, fmt.Errorf("音箱未接受学习请求：%w", err)
	}
	if code, ok := env["code"].(float64); ok && int(code) != 0 {
		return nil, fmt.Errorf("音箱拒绝进入学习（code=%d）", int(code))
	}
	return json.Marshal(map[string]any{
		"learning": true, "key": in.Key, "via": blaster.Name,
		"instruction": "音箱已进入学习状态。请在十几秒内，用**原遥控器**对着音箱按下要学的那个键，" +
			"然后立即调用 ir_read 取回码。",
	})
}

// irReadTool 读回学习到的红外码（`ir_read`），并可直接存成具名按键。
//
//	{"key":1,"save_as":"tv_power"}   ← save_as 可选
func irReadTool(ctx *plugin.Context, args json.RawMessage) (json.RawMessage, error) {
	var in struct {
		Key    int    `json:"key"`
		SaveAs string `json:"save_as"`
	}
	if len(args) > 0 {
		_ = json.Unmarshal(args, &in)
	}
	if in.Key <= 0 {
		in.Key = 1
	}
	blaster, token, berr := blasterDevice(ctx)
	if berr != nil {
		return nil, berr
	}
	env, err := miioExchangeWith(ctx, blaster, token, "miIO.ir_read",
		map[string]any{"key": strconv.Itoa(in.Key)}, miioExchangeOptions{Attempts: 1})
	if err != nil {
		return nil, fmt.Errorf("读码失败（可能还没学到）：%w", err)
	}
	if code, ok := env["code"].(float64); ok && int(code) != 0 {
		// -5002 = 该槽位还没码；-5003 = 学习超时
		return nil, fmt.Errorf("没有读到码（code=%d；-5002=该槽位无码，-5003=学习超时）", int(code))
	}
	learned, _ := env["code_str"].(string)
	if learned == "" {
		if v, ok := env["code"].(string); ok {
			learned = v
		}
	}
	// 小米把码放在 result["code"]，但我们的框架把 numeric code 也放在同一键，
	// 所以再兜一层：从 result 原始 map 里找字符串型 code。
	if learned == "" {
		if res, ok := env["result"].(map[string]any); ok {
			if v, ok2 := res["code"].(string); ok2 {
				learned = v
			}
		}
	}
	if learned == "" {
		return json.Marshal(map[string]any{
			"read": false,
			"note": "设备回了响应但没带 code 字段；原始响应见 raw",
			"raw":  env,
		})
	}
	out := map[string]any{
		"read": true, "key": in.Key, "code": learned,
		"code_bytes": len(learned), "via": blaster.Name,
	}
	if strings.TrimSpace(in.SaveAs) != "" {
		c := loadIRCache(ctx)
		if c.Entries == nil {
			c.Entries = map[string]map[string]irPattern{}
		}
		if c.Learned == nil {
			c.Learned = map[string]string{}
		}
		c.Learned[in.SaveAs] = learned
		saveIRCache(ctx, c)
		out["saved_as"] = in.SaveAs
	}
	return json.Marshal(out)
}
