package invalid

import "net/http"

//weaver:http test.First
type first struct{}

//weaver:route POST /callbacks/{id}
func (*first) Handle(http.ResponseWriter, *http.Request) {}

//weaver:http test.Second
type second struct{}

//weaver:route POST /callbacks/{name}
func (*second) Handle(http.ResponseWriter, *http.Request) {}
