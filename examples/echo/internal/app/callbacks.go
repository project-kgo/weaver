package app

import (
	"io"
	"net/http"

	"github.com/project-kgo/weaver"
	examplev1 "github.com/project-kgo/weaver/examples/echo/gen/example/v1"
	examplev1weaver "github.com/project-kgo/weaver/examples/echo/gen/example/v1/examplev1weaver"
)

// CallbackSettings 演示 HTTP 组件同样可以注入 YAML 配置。
type CallbackSettings struct {
	Token string `yaml:"token"`
}

// callbacks 演示接收第三方 HTTP 回调：只做鉴权和报文转换，业务逻辑通过 Ref 交给 Service。
//
//weaver:http weaver.example.Callbacks
type callbacks struct {
	weaver.WithConfig[CallbackSettings]
	Upper weaver.Ref[examplev1weaver.UpperServiceComponent]
}

//weaver:route POST /callbacks/upper/{value}
func (c *callbacks) upper(writer http.ResponseWriter, request *http.Request) {
	if request.Header.Get("X-Callback-Token") != c.Config().Token {
		http.Error(writer, "invalid token", http.StatusUnauthorized)
		return
	}
	response, err := c.Upper.Get().Upper(request.Context(), &examplev1.UpperRequest{Value: request.PathValue("value")})
	if err != nil {
		http.Error(writer, err.Error(), http.StatusBadGateway)
		return
	}
	_, _ = io.WriteString(writer, response.GetValue())
}
