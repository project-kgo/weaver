package weaver

import (
	"fmt"
	"sort"
	"sync"
)

// Registration 描述一个组件实现及其生成的注入逻辑。
type Registration struct {
	Service      ServiceDescriptor
	New          func() any
	Inject       func(any, Injector) error
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
	if registration.Service.name == "" {
		return fmt.Errorf("weaver: 组件名称不能为空")
	}
	if registration.New == nil || registration.Inject == nil {
		return fmt.Errorf("weaver: 组件 %q 缺少工厂或注入函数", registration.Service.name)
	}
	if registration.Service.newLocal == nil || registration.Service.newRemote == nil || registration.Service.newHandler == nil {
		return fmt.Errorf("weaver: 组件 %q 的服务描述不完整", registration.Service.name)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.registrations[registration.Service.name]; exists {
		return fmt.Errorf("weaver: 组件 %q 被重复注册", registration.Service.name)
	}
	registration.Dependencies = append([]string(nil), registration.Dependencies...)
	sort.Strings(registration.Dependencies)
	r.registrations[registration.Service.name] = registration
	return nil
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
