// genmiotidx 从 instances.snapshot.json 生成命令路径专用的紧凑查找表。
//
// 背景（t234）：WASM 内解析 5.9MB / 48k 条 JSON 会吃满 60s 执行预算。
// 命令路径只需要 model→type 一次解析，用编译期生成的定长索引 + 二分查找，
// 避免每条命令重建整个 ModelIndex。
//
// 文件格式（models.bin）：
//
//	magic  "MXMI1\n"          6 字节
//	count  uint32 BE          4 字节
//	offs   count * uint32 BE  偏移表（指向 blob 起点）
//	blob   [key '\0' type '\0'] * count   （key 升序）
package main

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
)

type inst struct {
	Model   string `json:"model"`
	Type    string `json:"type"`
	Version int    `json:"version"`
	Status  string `json:"status"`
}

func rank(s string) int {
	switch s {
	case "released":
		return 3
	case "preview":
		return 2
	case "debug":
		return 1
	}
	return 0
}

func alias(s string) string {
	p := strings.Split(s, ".")
	if len(p) > 2 {
		return strings.Join(p[len(p)-2:], ".")
	}
	return s
}

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "用法: genmiotidx <snapshot.json> <models.bin>")
		os.Exit(2)
	}
	raw, err := os.ReadFile(os.Args[1])
	if err != nil {
		panic(err)
	}
	var doc struct {
		Instances []inst `json:"instances"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		panic(err)
	}
	sorted := append([]inst(nil), doc.Instances...)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].Model != sorted[j].Model {
			return sorted[i].Model < sorted[j].Model
		}
		if sorted[i].Version != sorted[j].Version {
			return sorted[i].Version > sorted[j].Version
		}
		return rank(sorted[i].Status) > rank(sorted[j].Status)
	})
	exact := map[string]string{}
	aliasOwner := map[string]string{}
	aliasConflict := map[string]bool{}
	for _, x := range sorted {
		if strings.TrimSpace(x.Model) == "" || strings.TrimSpace(x.Type) == "" {
			continue
		}
		if _, ok := exact[x.Model]; ok {
			continue
		}
		exact[x.Model] = x.Type
		a := alias(x.Model)
		if a == x.Model {
			continue
		}
		if prev, ok := aliasOwner[a]; ok && prev != x.Model {
			aliasConflict[a] = true
			continue
		}
		aliasOwner[a] = x.Model
	}
	keys := make([]string, 0, len(exact)+len(aliasOwner))
	for m := range exact {
		keys = append(keys, m)
	}
	for a := range aliasOwner {
		if aliasConflict[a] {
			continue
		}
		if _, isExact := exact[a]; isExact {
			continue
		}
		keys = append(keys, a)
	}
	sort.Strings(keys)
	var blob []byte
	offs := make([]uint32, 0, len(keys))
	for _, k := range keys {
		typ := exact[k]
		if t, ok := exact[k]; ok {
			typ = t
		} else if full, ok := aliasOwner[k]; ok {
			typ = exact[full]
		}
		offs = append(offs, uint32(len(blob)))
		blob = append(blob, k...)
		blob = append(blob, 0)
		blob = append(blob, typ...)
		blob = append(blob, 0)
	}
	out := make([]byte, 0, 10+4*len(offs)+len(blob))
	out = append(out, []byte("MXMI1\n")...)
	var n [4]byte
	binary.BigEndian.PutUint32(n[:], uint32(len(offs)))
	out = append(out, n[:]...)
	for _, o := range offs {
		binary.BigEndian.PutUint32(n[:], o)
		out = append(out, n[:]...)
	}
	out = append(out, blob...)
	if err := os.WriteFile(os.Args[2], out, 0o644); err != nil {
		panic(err)
	}
	fmt.Printf("wrote %s: keys=%d blob=%d total=%d\n", os.Args[2], len(keys), len(blob), len(out))
}
