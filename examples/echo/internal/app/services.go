package app

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/project-kgo/weaver"
	examplev1 "github.com/project-kgo/weaver/examples/echo/gen/example/v1"
	examplev1weaver "github.com/project-kgo/weaver/examples/echo/gen/example/v1/examplev1weaver"
)

// Settings 演示普通类型资源注入。
type Settings struct {
	Prefix string
}

type upperService struct {
	weaver.Implements[examplev1weaver.UpperServiceComponent]
	Settings weaver.Resource[*Settings]
}

func (service *upperService) Init(context.Context) error {
	if service.Settings.Get().Prefix == "" {
		return fmt.Errorf("prefix 不能为空")
	}
	return nil
}

func (service *upperService) Upper(ctx context.Context, request *examplev1.UpperRequest) (*examplev1.UpperResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if request.GetValue() == "error" {
		return nil, errors.New("upper service failed")
	}
	return &examplev1.UpperResponse{
		Value: service.Settings.Get().Prefix + strings.ToUpper(request.GetValue()),
	}, nil
}

type echoService struct {
	weaver.Implements[examplev1weaver.EchoServiceComponent]
	Upper weaver.Ref[examplev1weaver.UpperServiceComponent]
}

func (service *echoService) Echo(ctx context.Context, request *examplev1.EchoRequest) (*examplev1.EchoResponse, error) {
	response, err := service.Upper.Get().Upper(ctx, &examplev1.UpperRequest{Value: request.GetValue()})
	if err != nil {
		return nil, err
	}
	return &examplev1.EchoResponse{Value: response.GetValue()}, nil
}
