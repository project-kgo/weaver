package invalid

import "net/http"

//weaver:http test/bad
type bad struct{}

//weaver:route POST /bad
func (*bad) Handle(http.ResponseWriter, *http.Request) {}
