package weaver

import (
	"context"
	"fmt"
	"reflect"
)

// CodegenVersion 用于让过期的生成代码在编译期失败。
const CodegenVersion = 2

// Implements 声明一个结构体实现了组件接口，仅供代码生成器识别。
type Implements[T any] struct{}

// Ref 保存另一个组件的代理。代理由 Runtime 在启动时注入。
type Ref[T any] struct {
	value T
	set   bool
}

// Get 返回已注入的组件代理。
func (r Ref[T]) Get() T {
	if !r.set {
		panic("weaver: Ref 尚未注入")
	}
	return r.value
}

// Resource 保存普通 Go 资源，例如数据库连接或配置对象。
type Resource[T any] struct {
	value T
	set   bool
}

// Get 返回已注入的资源。
func (r Resource[T]) Get() T {
	if !r.set {
		panic("weaver: Resource 尚未注入")
	}
	return r.value
}

// WithConfig 把当前组件的 YAML 配置绑定到类型 T。
// T 必须是结构体，并由组件实现匿名嵌入此类型。
type WithConfig[T any] struct {
	value T
	set   bool
}

// Config 返回当前组件的配置。配置在 Init 执行前由 Runtime 注入。
func (c *WithConfig[T]) Config() *T {
	if !c.set {
		panic("weaver: Config 尚未注入")
	}
	return &c.value
}

// Initializer 在所有字段注入完成后执行。
type Initializer interface {
	Init(context.Context) error
}

// Shutdowner 在 Runtime 关闭时逆依赖顺序执行。
type Shutdowner interface {
	Shutdown(context.Context) error
}

// Injector 是生成代码与 Runtime 之间的最小注入协议。
// 未导出的方法防止业务代码自行实现并绕过 Runtime 校验。
type Injector interface {
	resolveComponent(string) (any, error)
	resolveResource(reflect.Type) (any, error)
	resolveConfig(reflect.Type) (any, error)
}

// ResolveComponent 仅供生成代码使用。
func ResolveComponent[T any](injector Injector, name string) (T, error) {
	var zero T
	value, err := injector.resolveComponent(name)
	if err != nil {
		return zero, err
	}
	typed, ok := value.(T)
	if !ok {
		return zero, fmt.Errorf("weaver: 组件 %q 的代理类型为 %T，无法赋给 %v", name, value, reflect.TypeFor[T]())
	}
	return typed, nil
}

// ResolveResource 仅供生成代码使用，资源按精确 Go 类型匹配。
func ResolveResource[T any](injector Injector) (T, error) {
	var zero T
	typeOfT := reflect.TypeFor[T]()
	value, err := injector.resolveResource(typeOfT)
	if err != nil {
		return zero, err
	}
	typed, ok := value.(T)
	if !ok {
		return zero, fmt.Errorf("weaver: 资源 %v 的实际类型为 %T", typeOfT, value)
	}
	return typed, nil
}

// ResolveConfig 仅供生成代码使用，配置类型按当前组件精确匹配。
func ResolveConfig[T any](injector Injector) (T, error) {
	var zero T
	typeOfT := reflect.TypeFor[T]()
	value, err := injector.resolveConfig(typeOfT)
	if err != nil {
		return zero, err
	}
	typed, ok := value.(T)
	if !ok {
		return zero, fmt.Errorf("weaver: 配置 %v 的实际类型为 %T", typeOfT, value)
	}
	return typed, nil
}

// SetRef 仅供生成代码使用。
func SetRef[T any](target *Ref[T], value T) {
	target.value = value
	target.set = true
}

// SetResource 仅供生成代码使用。
func SetResource[T any](target *Resource[T], value T) {
	target.value = value
	target.set = true
}

// SetConfig 仅供生成代码使用。
func SetConfig[T any](target *WithConfig[T], value T) {
	target.value = value
	target.set = true
}
