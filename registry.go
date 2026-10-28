package weaver

import (
	"fmt"
	"reflect"
	"sort"
	"sync"
)

// Registration 描述一个组件实现及其生成的注入逻辑。
// Service 与 HTTP 二选一：前者是 protobuf 组件，后者是原生 HTTP 组件。
type Registration struct {
	Service      ServiceDescriptor
	HTTP         HTTPDescriptor
	New          func() any
	Inject       func(any, Injector) error
	ConfigType   reflect.Type
	Dependencies []string
}

// Registry 保存进程内可用的组件定义，不保存组件实例。
type Registry struct {
	mu            sync.RWMutex
	registrations map[string]Registration
}

// NewRegistry 创建隔离注册表，主要用于测试或同进程多应用场景。
func NewRegistry() *Registry {
	return &Registry{registrations: make(map[string]Registration)}
}

// Register 注册一个组件实现。
func (r *Registry) Register(registration Registration) error {
	if r == nil {
		return fmt.Errorf("weaver: Registry 不能为空")
	}
	name := registration.name()
	if name == "" {
		return fmt.Errorf("weaver: 组件名称不能为空")
	}
	if registration.Service.name != "" && registration.HTTP.Name != "" {
		return fmt.Errorf("weaver: 组件 %q 不能同时是 protobuf 组件和 HTTP 组件", name)
	}
	if registration.New == nil || registration.Inject == nil {
		return fmt.Errorf("weaver: 组件 %q 缺少工厂或注入函数", name)
	}
	if registration.isHTTP() {
		if err := registration.HTTP.validate(); err != nil {
			return err
		}
		registration.HTTP.Routes = append([]Route(nil), registration.HTTP.Routes...)
	} else if registration.Service.componentType == nil || registration.Service.newLocal == nil || registration.Service.newRemote == nil || registration.Service.newHandler == nil {
		return fmt.Errorf("weaver: 组件 %q 的服务描述不完整", name)
	}
	if registration.ConfigType != nil && registration.ConfigType.Kind() != reflect.Struct {
		return fmt.Errorf("weaver: 组件 %q 的配置类型必须是结构体，得到 %v", name, registration.ConfigType)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.registrations[name]; exists {
		return fmt.Errorf("weaver: 组件 %q 被重复注册", name)
	}
	registration.Dependencies = append([]string(nil), registration.Dependencies...)
	sort.Strings(registration.Dependencies)
	r.registrations[name] = registration
	return nil
}

func (r Registration) isHTTP() bool {
	return r.HTTP.Name != ""
}

// name 返回 placements 与配置段使用的组件名称。
func (r Registration) name() string {
	if r.isHTTP() {
		return r.HTTP.Name
	}
	return r.Service.name
}

func (r *Registry) snapshot() map[string]Registration {
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make(map[string]Registration, len(r.registrations))
	for name, registration := range r.registrations {
		result[name] = registration
	}
	return result
}

var defaultRegistry = NewRegistry()

// Register 把生成的组件注册到默认注册表。
func Register(registration Registration) error {
	return defaultRegistry.Register(registration)
}

// MustRegister 供生成代码在 init 中使用，错误会立即暴露。
func MustRegister(registration Registration) {
	if err := Register(registration); err != nil {
		panic(err)
	}
}
