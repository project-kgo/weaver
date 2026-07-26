package invalid

import "github.com/project-kgo/weaver"

type Bad interface {
	Call() error
}

type implementation struct {
	weaver.Implements[Bad]
}

func (*implementation) Call() error { return nil }
