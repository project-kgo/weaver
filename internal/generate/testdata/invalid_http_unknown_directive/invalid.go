package invalid

import "net/http"

//weaver:http test.Bad
type bad struct{}

//weaver:routes POST /bad
func (*bad) Handle(http.ResponseWriter, *http.Request) {}
