package weaver

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"connectrpc.com/connect"
)

// Resolver 把 unit target 解析为 Connect 可使用的目标。
type Resolver interface {
	Resolve(context.Context, string) (ResolvedTarget, error)
}

// ResolvedTarget 同时携带基础 URL 和负责发现、负载均衡的 HTTPClient。
type ResolvedTarget struct {
	BaseURL    string
	HTTPClient connect.HTTPClient
}

type staticResolver struct {
	client connect.HTTPClient
}

func (r staticResolver) Resolve(_ context.Context, target string) (ResolvedTarget, error) {
	parsed, err := url.Parse(target)
	if err != nil {
		return ResolvedTarget{}, fmt.Errorf("weaver: 无效静态目标 %q: %w", target, err)
	}
	if (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return ResolvedTarget{}, fmt.Errorf("weaver: 静态目标必须是有效的 http/https URL: %q", target)
	}
	return ResolvedTarget{
		BaseURL:    strings.TrimRight(target, "/"),
		HTTPClient: r.client,
	}, nil
}

func defaultHTTPClient() connect.HTTPClient {
	protocols := new(http.Protocols)
	protocols.SetHTTP2(true)
	protocols.SetUnencryptedHTTP2(true)

	// 跨 unit 调用默认只使用 HTTP/2。明文目标使用 h2c prior knowledge，
	// 不启用 HTTP/1，避免失败后重发可能非幂等的 RPC。
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Protocols = protocols
	return &http.Client{Transport: transport}
}
