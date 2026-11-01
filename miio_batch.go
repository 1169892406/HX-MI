package main

import (
	"fmt"
	"homex.dev/homex/sdk/go/plugin"
)

// property get/set 按协议上限15项分块；action 明确是单动作合同，批量输入逐项
// 执行并按输入顺序合并结果，任何块失败立即返回带序号错误。
func miioBatchProperties(ctx *plugin.Context, d miioDevice, token []byte, method string, params []map[string]any) ([]map[string]any, error) {
	out := []map[string]any{}
	chunks := chunk15(params)
	if method == "action" {
		chunks = make([][]map[string]any, len(params))
		for i := range params {
			chunks[i] = []map[string]any{params[i]}
		}
	}
	for i, chunk := range chunks {
		env, err := miioExchange(ctx, d, token, method, chunk)
		if err != nil {
			return nil, fmt.Errorf("%s batch %d/%d: %w", method, i+1, len(chunks), err)
		}
		if list, ok := env["result"].([]any); ok {
			for _, x := range list {
				if m, ok := x.(map[string]any); ok {
					out = append(out, m)
				}
			}
		} else if m, ok := env["result"].(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out, nil
}
