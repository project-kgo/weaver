package components

import (
	"context"
	"net/http"

	"connectrpc.com/connect"
	"github.com/project-kgo/weaver"
)

type UpperComponent interface {
	Upper(context.Context, string) (string, error)
}

type EchoComponent interface {
	Echo(context.Context, string) (string, error)
}

var Upper = weaver.Service[UpperComponent]{
	Name:       "test.v1.Upper",
	NewLocal:   func(value UpperComponent) UpperComponent { return value },
	NewRemote:  func(connect.HTTPClient, string, ...connect.ClientOption) UpperComponent { return nil },
	NewHandler: func(UpperComponent, ...connect.HandlerOption) (string, http.Handler) { return "", nil },
}

var Echo = weaver.Service[EchoComponent]{
	Name:       "test.v1.Echo",
	NewLocal:   func(value EchoComponent) EchoComponent { return value },
	NewRemote:  func(connect.HTTPClient, string, ...connect.ClientOption) EchoComponent { return nil },
	NewHandler: func(EchoComponent, ...connect.HandlerOption) (string, http.Handler) { return "", nil },
}
