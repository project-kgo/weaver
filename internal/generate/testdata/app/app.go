package app

import (
	"context"
	"database/sql"
	"strings"

	"github.com/project-kgo/weaver"
	"github.com/project-kgo/weaver/internal/generate/testdata/components"
)

type upperConfig struct {
	Prefix string `yaml:"prefix"`
}

type upper struct {
	weaver.Implements[components.UpperComponent]
	weaver.WithConfig[upperConfig]
	Database weaver.Resource[*sql.DB]
}

func (*upper) Upper(_ context.Context, value string) (string, error) {
	return strings.ToUpper(value), nil
}

type echo struct {
	weaver.Implements[components.EchoComponent]
	Upper weaver.Ref[components.UpperComponent]
}

func (e *echo) Echo(ctx context.Context, value string) (string, error) {
	return e.Upper.Get().Upper(ctx, value)
}
