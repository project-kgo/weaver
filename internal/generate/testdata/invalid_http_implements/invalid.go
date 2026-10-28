package invalid

import (
	"context"
	"net/http"

	"github.com/project-kgo/weaver"
	"github.com/project-kgo/weaver/internal/generate/testdata/components"
)

//weaver:http test.Bad
type bad struct {
	weaver.Implements[components.StandaloneComponent]
}

func (*bad) Standalone(context.Context) error { return nil }

//weaver:route POST /bad
func (*bad) Handle(http.ResponseWriter, *http.Request) {}
