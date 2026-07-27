package invalid_config_multiple

import (
	"context"

	"github.com/project-kgo/weaver"
	"github.com/project-kgo/weaver/internal/generate/testdata/components"
)

type firstSettings struct {
	Value string
}

type secondSettings struct {
	Value string
}

type firstConfig = weaver.WithConfig[firstSettings]
type secondConfig = weaver.WithConfig[secondSettings]

type upper struct {
	weaver.Implements[components.UpperComponent]
	firstConfig
	secondConfig
}

func (*upper) Upper(context.Context, string) (string, error) { return "", nil }
