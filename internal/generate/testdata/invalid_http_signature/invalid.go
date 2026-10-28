package invalid

import "net/http"

//weaver:http test.Bad
type bad struct{}

//weaver:route POST /bad
func (*bad) Handle(*http.Request) {}
