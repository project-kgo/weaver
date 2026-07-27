package invalid_config_type

import (
	"context"

	"github.com/project-kgo/weaver"
	"github.com/project-kgo/weaver/internal/generate/testdata/components"
)

type upper struct {
	weaver.Implements[components.UpperComponent]
	weaver.WithConfig[string]
}

func (*upper) Upper(context.Context, string) (string, error) { return "", nil }
