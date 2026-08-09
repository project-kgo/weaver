package weaver

import (
	"context"
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
	handlerOptions []handlerOption
	shutdownHooks  []ShutdownHook
}

// handlerOption 延迟需要组件代理的 Handler 配置，确保它与普通配置保持注册顺序。
type handlerOption struct {
	value          connect.HandlerOption
	componentType  reflect.Type
	newInterceptor func(any) (connect.Interceptor, error)
}

// ShutdownHook 是 Runtime 关闭组件后执行的外部清理函数。
type ShutdownHook func(context.Context) error

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
		for _, value := range values {
			options.handlerOptions = append(options.handlerOptions, handlerOption{value: value})
		}
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
			options.handlerOptions = append(options.handlerOptions, handlerOption{value: connect.WithInterceptors(values...)})
		}
		return nil
	})
}

// WithHandlerInterceptor 创建一个可使用组件代理的 Handler 中间件。
// T 必须是已注册的组件接口；传给 factory 的值与 Ref[T] 使用相同的本地或远程代理。
// factory 在所有本地组件初始化完成后执行一次。
func WithHandlerInterceptor[T any](factory func(T) connect.Interceptor) Option {
	componentType := reflect.TypeFor[T]()
	return optionFunc(func(options *runtimeOptions) error {
		if factory == nil {
			return fmt.Errorf("weaver: Handler Interceptor 工厂不能为空")
		}
		options.handlerOptions = append(options.handlerOptions, handlerOption{
			componentType: componentType,
			newInterceptor: func(value any) (connect.Interceptor, error) {
				component, ok := value.(T)
				if !ok {
					return nil, fmt.Errorf("weaver: 组件代理类型 %T 无法赋给 %v", value, componentType)
				}
				interceptor := factory(component)
				if interceptor == nil || isNil(interceptor) {
					return nil, fmt.Errorf("weaver: Handler Interceptor 工厂返回 nil")
				}
				return interceptor, nil
			},
		})
		return nil
	})
}

// WithShutdownHook 注册外部关闭回调。
// 多次注册的回调会在组件全部关闭后，按注册顺序的逆序执行。
func WithShutdownHook(hook ShutdownHook) Option {
	return optionFunc(func(options *runtimeOptions) error {
		if hook == nil {
			return fmt.Errorf("weaver: ShutdownHook 不能为空")
		}
		options.shutdownHooks = append(options.shutdownHooks, hook)
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
