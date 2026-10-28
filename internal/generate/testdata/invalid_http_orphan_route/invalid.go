package invalid

import "net/http"

type bad struct{}

//weaver:route POST /bad
func (*bad) Handle(http.ResponseWriter, *http.Request) {}
