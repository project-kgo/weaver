package app

import (
	"context"
	"errors"

	"github.com/project-kgo/weaver"
	examplev1 "github.com/project-kgo/weaver/examples/echo/gen/example/v1"
	examplev1weaver "github.com/project-kgo/weaver/examples/echo/gen/example/v1/examplev1weaver"
)

// Settings 演示按组件注入的 YAML 配置。

type userService struct {
	weaver.Implements[examplev1weaver.LoginServiceComponent]
}

func (service *userService) Init(context.Context) error {
	return nil
}

func (service *userService) Login(ctx context.Context, request *examplev1.LoginRequest) (*examplev1.LoginResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if request.GetValue() == "error" {
		return nil, errors.New("login service failed")
	}
	return &examplev1.LoginResponse{
		Token: request.GetValue(),
	}, nil
}
