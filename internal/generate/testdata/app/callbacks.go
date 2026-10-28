package app

import (
	"net/http"

	"github.com/project-kgo/weaver"
	"github.com/project-kgo/weaver/internal/generate/testdata/components"
)

type callbackConfig struct {
	Secret string `yaml:"secret"`
}

//weaver:http test.Callbacks
type callbacks struct {
	weaver.WithConfig[callbackConfig]
	Upper weaver.Ref[components.UpperComponent]
}

//weaver:route POST /callbacks/pay/{provider}
//weaver:route POST /callbacks/pay
func (c *callbacks) pay(w http.ResponseWriter, r *http.Request) {
	value, _ := c.Upper.Get().Upper(r.Context(), r.PathValue("provider"))
	_, _ = w.Write([]byte(value))
}

// Ping 用于验证值接收者方法同样可以声明路由。
//
//weaver:route GET /callbacks/ping
func (c callbacks) Ping(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusNoContent)
}
