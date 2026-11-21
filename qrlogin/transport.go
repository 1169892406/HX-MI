package qrlogin

// 传输层与存储层抽象：让本包同时跑在普通 Go 程序（net/http + 文件）和
// 受限运行时（如 wasm 插件沙箱：无 socket、无文件系统，只有 Host API）里。
//
// 默认实现是 NetHTTPTransport + FileStore；受限环境在 Options 里注入自己的实现。

import (
	"bytes"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Response 一次 HTTP 交互的结果。Headers 里的 Set-Cookie 必须保留：
// serviceLogin 回调响应里带轮换后的 passToken/cUserId。
type Response struct {
	Status  int
	Headers map[string]string
	Body    []byte
}

// Transport 抽象 HTTP 传输。timeout 是单次请求上限（长轮询按需放宽，
// wasm 环境受 execution_timeout_ms 约束，一般传 25s 并靠重发轮询兜底）。
type Transport interface {
	Do(method, url string, headers map[string]string, timeout time.Duration) (Response, error)
}

// NetHTTPTransport 默认实现：标准库。
type NetHTTPTransport struct {
	Client *http.Client
}

// Do 实现 Transport。
func (t *NetHTTPTransport) Do(method, u string, headers map[string]string, timeout time.Duration) (Response, error) {
	client := t.Client
	if client == nil {
		client = &http.Client{Timeout: timeout}
	}
	if client.Timeout == 0 && timeout > 0 {
		client = &http.Client{Timeout: timeout}
	}
	req, err := http.NewRequest(method, u, nil)
	if err != nil {
		return Response{}, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return Response{}, err
	}
	defer resp.Body.Close()
	body := &bytes.Buffer{}
	_, _ = body.ReadFrom(resp.Body)
	hdrs := map[string]string{}
	for k, vs := range resp.Header {
		if len(vs) > 0 {
			hdrs[k] = strings.Join(vs, "\n")
		}
	}
	return Response{Status: resp.StatusCode, Headers: hdrs, Body: body.Bytes()}, nil
}

// Store 抽象持久化（Get 返回 found=false 表示键不存在，不是错误）。
type Store interface {
	Get(key string) (string, bool, error)
	Put(key, value string) error
}

// FileStore 默认实现：直接写文件（原子替换）。
type FileStore struct{}

// Get 实现 Store。
func (FileStore) Get(key string) (string, bool, error) {
	data, err := os.ReadFile(key)
	if os.IsNotExist(err) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return string(data), true, nil
}

// Put 实现 Store。
func (FileStore) Put(key, value string) error {
	if err := os.MkdirAll(filepath.Dir(key), 0o755); err != nil {
		return err
	}
	tmp := key + ".tmp"
	if err := os.WriteFile(tmp, []byte(value), 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, key); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "[qrlogin] 已写入 %s（%d 字节）\n", key, len(value))
	return nil
}

// storeBucket 带互斥的 Store 包装（Client 内部并发访问）。
type storeBucket struct {
	mu    sync.Mutex
	store Store
}

func (b *storeBucket) get(key string) (string, bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.store == nil {
		return "", false, nil
	}
	return b.store.Get(key)
}

func (b *storeBucket) put(key, value string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.store == nil {
		return fmt.Errorf("qrlogin: Store 未配置，无法落盘")
	}
	return b.store.Put(key, value)
}
