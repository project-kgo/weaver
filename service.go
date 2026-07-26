package weaver

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"connectrpc.com/connect"
)

// Service 是 protoc-gen-weaver-go 生成的强类型服务描述。
type Service[T any] struct {
	Name       string
	NewLocal   func(T) T
	NewRemote  func(connect.HTTPClient, string, ...connect.ClientOption) T
	NewHandler func(T, ...connect.HandlerOption) (string, http.Handler)
}

// ServiceDescriptor 是 Runtime 使用的类型擦除描述。
type ServiceDescriptor struct {
	name       string
	newLocal   func(any) (any, error)
	newRemote  func(connect.HTTPClient, string, ...connect.ClientOption) (any, error)
	newHandler func(any, ...connect.HandlerOption) (string, http.Handler, error)
}

// Descriptor 将生成的强类型描述转换为 Runtime 描述。
func (s Service[T]) Descriptor() ServiceDescriptor {
	return ServiceDescriptor{
		name: s.Name,
		newLocal: func(implementation any) (any, error) {
			typed, ok := implementation.(T)
			if !ok {
				return nil, fmt.Errorf("weaver: 组件 %q 的实现类型 %T 不满足生成接口", s.Name, implementation)
			}
			if s.NewLocal == nil {
				return nil, fmt.Errorf("weaver: 组件 %q 缺少本地代理工厂", s.Name)
			}
			return s.NewLocal(typed), nil
		},
		newRemote: func(client connect.HTTPClient, baseURL string, options ...connect.ClientOption) (any, error) {
			if s.NewRemote == nil {
				return nil, fmt.Errorf("weaver: 组件 %q 缺少远程客户端工厂", s.Name)
			}
			return s.NewRemote(client, baseURL, options...), nil
		},
		newHandler: func(implementation any, options ...connect.HandlerOption) (string, http.Handler, error) {
			typed, ok := implementation.(T)
			if !ok {
				return "", nil, fmt.Errorf("weaver: 组件 %q 的实现类型 %T 不满足生成接口", s.Name, implementation)
			}
			if s.NewHandler == nil {
				return "", nil, fmt.Errorf("weaver: 组件 %q 缺少 Handler 工厂", s.Name)
			}
			path, handler := s.NewHandler(typed, options...)
			if path == "" || handler == nil {
				return "", nil, fmt.Errorf("weaver: 组件 %q 生成了无效 Handler", s.Name)
			}
			return path, handler, nil
		},
	}
}

// Name 返回 protobuf service 全名。
func (s Service[T]) ServiceName() string {
	return s.Name
}

// NormalizeError 对齐本地调用与 Connect 远程调用的未知错误语义。
func NormalizeError(err error) error {
	if err == nil {
		return nil
	}
	var connectErr *connect.Error
	if errors.As(err, &connectErr) {
		return err
	}
	if errors.Is(err, context.Canceled) {
		return connect.NewError(connect.CodeCanceled, err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return connect.NewError(connect.CodeDeadlineExceeded, err)
	}
	return connect.NewError(connect.CodeUnknown, err)
}
