package weaver

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"connectrpc.com/connect"
	"github.com/project-kgo/weaver/internal/kube"
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

// builtinKubeResolver 延迟读取集群凭证，未使用 kube target 时不会访问 Kubernetes。
type builtinKubeResolver struct {
	once     sync.Once
	resolver *kube.Resolver
	err      error
}

// Prepare 在 Runtime 创建组件前注册全部实际依赖的 target，并启动按 namespace 合并的发现流。
func (r *builtinKubeResolver) Prepare(ctx context.Context, targets []string) error {
	if len(targets) == 0 {
		return nil
	}
	r.once.Do(func() {
		r.resolver, r.err = kube.NewInCluster()
		if r.err != nil {
			return
		}
		for _, target := range targets {
			if _, _, r.err = r.resolver.Resolve(ctx, target); r.err != nil {
				return
			}
		}
		r.err = r.resolver.Start(ctx)
	})
	return r.err
}

func (r *builtinKubeResolver) Resolve(ctx context.Context, target string) (ResolvedTarget, error) {
	if r.err != nil {
		return ResolvedTarget{}, r.err
	}
	if r.resolver == nil {
		return ResolvedTarget{}, fmt.Errorf("weaver: kube target %q 未在 Runtime 启动前准备", target)
	}
	baseURL, client, err := r.resolver.Resolve(ctx, target)
	if err != nil {
		return ResolvedTarget{}, err
	}
	return ResolvedTarget{BaseURL: baseURL, HTTPClient: client}, nil
}

func (r *builtinKubeResolver) Shutdown(ctx context.Context) error {
	if r.resolver == nil {
		return nil
	}
	return r.resolver.Shutdown(ctx)
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
