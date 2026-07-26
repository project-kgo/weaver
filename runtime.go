package weaver

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"sort"
	"strings"
	"sync"
)

// Runtime 是嵌入业务进程的组件容器。
type Runtime struct {
	currentUnit string
	config      Config
	options     runtimeOptions
	handler     http.Handler
	startupCtx  context.Context

	registrations map[string]Registration
	instances     map[string]any
	localClients  map[string]any
	remoteClients map[string]any
	resolvedUnits map[string]ResolvedTarget
	shutdownOrder []any

	shutdownMu sync.Mutex
	shutdown   bool
}

// New 创建、注入并初始化当前 unit 的全部组件。
func New(ctx context.Context, currentUnit string, config Config, values ...Option) (*Runtime, error) {
	if ctx == nil {
		return nil, fmt.Errorf("weaver: context 不能为空")
	}
	currentUnit = strings.TrimSpace(currentUnit)
	if currentUnit == "" {
		return nil, fmt.Errorf("weaver: currentUnit 不能为空")
	}

	options := newRuntimeOptions()
	for _, value := range values {
		if value == nil {
			return nil, fmt.Errorf("weaver: Option 不能为空")
		}
		if err := value.apply(&options); err != nil {
			return nil, err
		}
	}
	options.resolvers["http"] = staticResolver{client: options.httpClient}
	options.resolvers["https"] = staticResolver{client: options.httpClient}

	runtime := &Runtime{
		currentUnit:   currentUnit,
		config:        cloneConfig(config),
		options:       options,
		startupCtx:    ctx,
		registrations: options.registry.snapshot(),
		instances:     make(map[string]any),
		localClients:  make(map[string]any),
		remoteClients: make(map[string]any),
		resolvedUnits: make(map[string]ResolvedTarget),
	}
	order, err := runtime.validateAndOrder()
	if err != nil {
		return nil, err
	}

	for _, name := range order {
		if runtime.config.Placements[name] != currentUnit {
			continue
		}
		instance := runtime.registrations[name].New()
		if instance == nil || isNil(instance) {
			return nil, fmt.Errorf("weaver: 组件 %q 的工厂返回 nil", name)
		}
		runtime.instances[name] = instance
	}

	injector := runtimeInjector{runtime: runtime}
	for _, name := range order {
		instance, local := runtime.instances[name]
		if !local {
			continue
		}
		if err := runtime.registrations[name].Inject(instance, injector); err != nil {
			return nil, fmt.Errorf("weaver: 注入组件 %q 失败: %w", name, err)
		}
	}

	for _, name := range order {
		instance, local := runtime.instances[name]
		if !local {
			continue
		}
		if initializer, ok := instance.(Initializer); ok {
			if err := initializer.Init(ctx); err != nil {
				cleanupErr := runtime.shutdownComponents(context.WithoutCancel(ctx))
				return nil, errors.Join(fmt.Errorf("weaver: 初始化组件 %q 失败: %w", name, err), cleanupErr)
			}
		}
		runtime.shutdownOrder = append(runtime.shutdownOrder, instance)
	}

	mux := http.NewServeMux()
	paths := make(map[string]string)
	for _, name := range order {
		instance, local := runtime.instances[name]
		if !local {
			continue
		}
		path, handler, err := runtime.registrations[name].Service.newHandler(instance, options.handlerOptions...)
		if err != nil {
			cleanupErr := runtime.shutdownComponents(context.WithoutCancel(ctx))
			return nil, errors.Join(err, cleanupErr)
		}
		if previous, exists := paths[path]; exists {
			cleanupErr := runtime.shutdownComponents(context.WithoutCancel(ctx))
			return nil, errors.Join(fmt.Errorf("weaver: 组件 %q 与 %q 使用了相同 Handler 路径 %q", previous, name, path), cleanupErr)
		}
		paths[path] = name
		mux.Handle(path, handler)
	}
	runtime.handler = mux
	return runtime, nil
}

// Handler 返回只包含当前 unit 组件的 HTTP Handler。
func (r *Runtime) Handler() http.Handler {
	return r.handler
}

// Shutdown 幂等关闭组件；普通资源的生命周期仍由调用方负责。
func (r *Runtime) Shutdown(ctx context.Context) error {
	if ctx == nil {
		return fmt.Errorf("weaver: context 不能为空")
	}
	r.shutdownMu.Lock()
	defer r.shutdownMu.Unlock()
	if r.shutdown {
		return nil
	}
	r.shutdown = true
	return r.shutdownComponents(ctx)
}

func (r *Runtime) shutdownComponents(ctx context.Context) error {
	var result error
	for index := len(r.shutdownOrder) - 1; index >= 0; index-- {
		if shutdowner, ok := r.shutdownOrder[index].(Shutdowner); ok {
			result = errors.Join(result, shutdowner.Shutdown(ctx))
		}
	}
	r.shutdownOrder = nil
	return result
}

func (r *Runtime) validateAndOrder() ([]string, error) {
	if len(r.registrations) == 0 {
		return nil, fmt.Errorf("weaver: 没有注册任何组件，请先运行 weaver generate")
	}
	if _, exists := r.config.Units[r.currentUnit]; !exists {
		return nil, fmt.Errorf("weaver: currentUnit %q 未出现在 units 中", r.currentUnit)
	}
	for name, unit := range r.config.Placements {
		if _, exists := r.registrations[name]; !exists {
			return nil, fmt.Errorf("weaver: placements 包含未注册组件 %q", name)
		}
		if _, exists := r.config.Units[unit]; !exists {
			return nil, fmt.Errorf("weaver: 组件 %q 指向未知 unit %q", name, unit)
		}
	}
	localCount := 0
	for name := range r.registrations {
		unit, exists := r.config.Placements[name]
		if !exists || strings.TrimSpace(unit) == "" {
			return nil, fmt.Errorf("weaver: 组件 %q 缺少 placement", name)
		}
		if unit == r.currentUnit {
			localCount++
		}
	}
	if localCount == 0 {
		return nil, fmt.Errorf("weaver: unit %q 没有承载任何组件", r.currentUnit)
	}
	for unit, target := range r.config.Units {
		if unit != r.currentUnit && strings.TrimSpace(target) == "" {
			return nil, fmt.Errorf("weaver: 远程 unit %q 缺少 target", unit)
		}
	}
	return topologicalOrder(r.registrations)
}

type runtimeInjector struct {
	runtime *Runtime
}

func (i runtimeInjector) resolveComponent(name string) (any, error) {
	runtime := i.runtime
	registration, exists := runtime.registrations[name]
	if !exists {
		return nil, fmt.Errorf("weaver: 依赖了未注册组件 %q", name)
	}
	unit := runtime.config.Placements[name]
	if unit == runtime.currentUnit {
		if client, cached := runtime.localClients[name]; cached {
			return client, nil
		}
		implementation, exists := runtime.instances[name]
		if !exists {
			return nil, fmt.Errorf("weaver: 本地组件 %q 尚未创建", name)
		}
		client, err := registration.Service.newLocal(implementation)
		if err != nil {
			return nil, err
		}
		runtime.localClients[name] = client
		return client, nil
	}

	if client, cached := runtime.remoteClients[name]; cached {
		return client, nil
	}
	target, err := runtime.resolveUnit(unit)
	if err != nil {
		return nil, fmt.Errorf("weaver: 解析组件 %q 所在 unit %q 失败: %w", name, unit, err)
	}
	client, err := registration.Service.newRemote(target.HTTPClient, target.BaseURL, runtime.options.clientOptions...)
	if err != nil {
		return nil, err
	}
	runtime.remoteClients[name] = client
	return client, nil
}

func (i runtimeInjector) resolveResource(resourceType reflect.Type) (any, error) {
	value, exists := i.runtime.options.resources[resourceType]
	if !exists {
		return nil, fmt.Errorf("weaver: 缺少资源 %v", resourceType)
	}
	return value, nil
}

func (r *Runtime) resolveUnit(unit string) (ResolvedTarget, error) {
	if resolved, cached := r.resolvedUnits[unit]; cached {
		return resolved, nil
	}
	target := strings.TrimSpace(r.config.Units[unit])
	parsed, err := url.Parse(target)
	if err != nil || parsed.Scheme == "" {
		return ResolvedTarget{}, fmt.Errorf("无效 target %q", target)
	}
	resolver, exists := r.options.resolvers[strings.ToLower(parsed.Scheme)]
	if !exists {
		return ResolvedTarget{}, fmt.Errorf("没有为 scheme %q 注册 Resolver", parsed.Scheme)
	}
	resolved, err := resolver.Resolve(r.startupCtx, target)
	if err != nil {
		return ResolvedTarget{}, err
	}
	resolved.BaseURL = strings.TrimRight(strings.TrimSpace(resolved.BaseURL), "/")
	if resolved.BaseURL == "" || resolved.HTTPClient == nil {
		return ResolvedTarget{}, fmt.Errorf("Resolver %q 返回了无效目标", parsed.Scheme)
	}
	r.resolvedUnits[unit] = resolved
	return resolved, nil
}

func topologicalOrder(registrations map[string]Registration) ([]string, error) {
	names := make([]string, 0, len(registrations))
	for name := range registrations {
		names = append(names, name)
	}
	sort.Strings(names)

	const (
		unvisited = iota
		visiting
		visited
	)
	states := make(map[string]int, len(registrations))
	stack := make([]string, 0, len(registrations))
	order := make([]string, 0, len(registrations))
	var visit func(string) error
	visit = func(name string) error {
		switch states[name] {
		case visited:
			return nil
		case visiting:
			start := 0
			for index, value := range stack {
				if value == name {
					start = index
					break
				}
			}
			cycle := append(append([]string(nil), stack[start:]...), name)
			return fmt.Errorf("weaver: 组件依赖存在循环: %s", strings.Join(cycle, " -> "))
		}

		registration, exists := registrations[name]
		if !exists {
			return fmt.Errorf("weaver: 依赖了未注册组件 %q", name)
		}
		states[name] = visiting
		stack = append(stack, name)
		for _, dependency := range registration.Dependencies {
			if err := visit(dependency); err != nil {
				return err
			}
		}
		stack = stack[:len(stack)-1]
		states[name] = visited
		order = append(order, name)
		return nil
	}

	for _, name := range names {
		if err := visit(name); err != nil {
			return nil, err
		}
	}
	return order, nil
}

func cloneConfig(config Config) Config {
	result := Config{
		Units:      make(map[string]string, len(config.Units)),
		Placements: make(map[string]string, len(config.Placements)),
	}
	for name, target := range config.Units {
		result.Units[name] = target
	}
	for name, unit := range config.Placements {
		result.Placements[name] = unit
	}
	return result
}
