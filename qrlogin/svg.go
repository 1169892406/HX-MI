package qrlogin

// 登录二维码渲染：后端出 SVG（前端 <img> 直接用，不引任何 JS 库）。

import (
	"net/http"
	"rsc.io/qr"
	"strconv"
	"strings"
)

// QRSVG 把登录链接渲染成黑块白底的内联 SVG（尺寸交给 CSS 控制）。
func QRSVG(loginURL string) (string, error) {
	code, err := qr.Encode(loginURL, qr.M)
	if err != nil {
		return "", err
	}
	return qrSVG(code), nil
}

// ServeQRSVG http.Handler：GET 时确保有等待中的二维码并输出 SVG。
// 用法：mux.Handle("/api/qr/svg", c.QRSVGHandler())
func (c *Client) QRSVGHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		st := c.Status()
		if st.State != "waiting" || st.LoginURL == "" {
			st2, err := c.Start()
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			st = st2
		}
		svg, err := QRSVG(st.LoginURL)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "image/svg+xml")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write([]byte(svg))
	}
}

func qrSVG(code *qr.Code) string {
	w := code.Size
	var b strings.Builder
	b.WriteString(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 `)
	b.WriteString(strconv.Itoa(w + 8))
	b.WriteString(" ")
	b.WriteString(strconv.Itoa(w + 8))
	b.WriteString(`" shape-rendering="crispEdges" class="qr-svg">`)
	b.WriteString(`<rect width="100%" height="100%" fill="#fff"/>`)
	for y := 0; y < w; y++ {
		runStart := -1
		for x := 0; x <= w; x++ {
			black := x < w && code.Black(x, y)
			if black && runStart < 0 {
				runStart = x
			} else if !black && runStart >= 0 {
				b.WriteString(`<rect x="`)
				b.WriteString(strconv.Itoa(runStart + 4))
				b.WriteString(`" y="`)
				b.WriteString(strconv.Itoa(y + 4))
				b.WriteString(`" width="`)
				b.WriteString(strconv.Itoa(x - runStart))
				b.WriteString(`" height="1" fill="#1b1814"/>`)
				runStart = -1
			}
		}
	}
	b.WriteString(`</svg>`)
	return b.String()
}
