package weaver

import (
	"fmt"
	"reflect"
	"strings"

	"connectrpc.com/connect"
)

// Option 配置 Runtime。
type Option interface {
	apply(*runtimeOptions) error
}

type optionFunc func(*runtimeOptions) error

func (f optionFunc) apply(options *runtimeOptions) error {
	return f(options)
}

type runtimeOptions struct {
	registry       *Registry
	resources      map[reflect.Type]any
	resolvers      map[string]Resolver
	httpClient     connect.HTTPClient
	clientOptions  []connect.ClientOption
	handlerOptions []connect.HandlerOption
}

func newRuntimeOptions() runtimeOptions {
	return runtimeOptions{
		registry:   defaultRegistry,
		resources:  make(map[reflect.Type]any),
		resolvers:  make(map[string]Resolver),
		httpClient: defaultHTTPClient(),
	}
}

// WithRegistry 使用指定注册表代替默认注册表。
func WithRegistry(registry *Registry) Option {
	return optionFunc(func(options *runtimeOptions) error {
		if registry == nil {
			return fmt.Errorf("weaver: Registry 不能为空")
		}
		options.registry = registry
		return nil
	})
}

// WithResource 注册一个按精确 Go 类型匹配的资源。
func WithResource[T any](value T) Option {
	return optionFunc(func(options *runtimeOptions) error {
		typeOfT := reflect.TypeFor[T]()
		if isNil(value) {
			return fmt.Errorf("weaver: 资源 %v 不能为 nil", typeOfT)
		}
		if _, exists := options.resources[typeOfT]; exists {
			return fmt.Errorf("weaver: 资源 %v 被重复注册", typeOfT)
		}
		options.resources[typeOfT] = value
		return nil
	})
}

// WithResolver 注册自定义 URL scheme 的 Resolver。
func WithResolver(scheme string, resolver Resolver) Option {
	return optionFunc(func(options *runtimeOptions) error {
		scheme = strings.ToLower(strings.TrimSpace(scheme))
		if scheme == "" || strings.Contains(scheme, ":") {
			return fmt.Errorf("weaver: Resolver scheme %q 无效", scheme)
		}
		if scheme == "http" || scheme == "https" {
			return fmt.Errorf("weaver: %s 使用内置静态 Resolver，请通过 WithHTTPClient 自定义传输", scheme)
		}
		if resolver == nil || isNil(resolver) {
			return fmt.Errorf("weaver: Resolver %q 不能为空", scheme)
		}
		if _, exists := options.resolvers[scheme]; exists {
			return fmt.Errorf("weaver: Resolver %q 被重复注册", scheme)
		}
		options.resolvers[scheme] = resolver
		return nil
	})
}

// WithHTTPClient 设置 http/https 静态目标共用的客户端。
// 跨 unit 调用要求客户端支持 HTTP/2；明文 http 目标还必须支持 h2c。
func WithHTTPClient(client connect.HTTPClient) Option {
	return optionFunc(func(options *runtimeOptions) error {
		if client == nil || isNil(client) {
			return fmt.Errorf("weaver: HTTPClient 不能为空")
		}
		options.httpClient = client
		return nil
	})
}

// WithClientOptions 设置所有远程 Connect client 的传输层选项。
func WithClientOptions(values ...connect.ClientOption) Option {
	return optionFunc(func(options *runtimeOptions) error {
		options.clientOptions = append(options.clientOptions, values...)
		return nil
	})
}

// WithClientInterceptors 设置所有跨 unit Connect client 的中间件。
// 多次调用会按注册顺序追加；本地组件调用不会执行这些中间件。
func WithClientInterceptors(values ...connect.Interceptor) Option {
	return optionFunc(func(options *runtimeOptions) error {
		for _, value := range values {
			if value == nil || isNil(value) {
				return fmt.Errorf("weaver: Client Interceptor 不能为空")
			}
		}
		if len(values) != 0 {
			options.clientOptions = append(options.clientOptions, connect.WithInterceptors(values...))
		}
		return nil
	})
}

// WithHandlerOptions 设置当前 unit 所有 Connect handler 的传输层选项。
func WithHandlerOptions(values ...connect.HandlerOption) Option {
	return optionFunc(func(options *runtimeOptions) error {
		options.handlerOptions = append(options.handlerOptions, values...)
		return nil
	})
}

// WithHandlerInterceptors 设置当前 unit 所有 Connect handler 的中间件。
// 多次调用会按注册顺序追加；内置 recovery 会覆盖这些中间件中的 panic。
func WithHandlerInterceptors(values ...connect.Interceptor) Option {
	return optionFunc(func(options *runtimeOptions) error {
		for _, value := range values {
			if value == nil || isNil(value) {
				return fmt.Errorf("weaver: Handler Interceptor 不能为空")
			}
		}
		if len(values) != 0 {
			options.handlerOptions = append(options.handlerOptions, connect.WithInterceptors(values...))
		}
		return nil
	})
}

func isNil(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}
