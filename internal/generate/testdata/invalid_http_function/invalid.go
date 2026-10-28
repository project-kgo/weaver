package invalid

import "net/http"

//weaver:route POST /bad
func handle(http.ResponseWriter, *http.Request) {}
