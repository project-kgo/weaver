package invalid_config_named

import (
	"context"

	"github.com/project-kgo/weaver"
	"github.com/project-kgo/weaver/internal/generate/testdata/components"
)

type settings struct {
	Prefix string
}

type upper struct {
	weaver.Implements[components.UpperComponent]
	Settings weaver.WithConfig[settings]
}

func (*upper) Upper(context.Context, string) (string, error) { return "", nil }
