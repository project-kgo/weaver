package main

import (
	"github.com/project-kgo/weaver/internal/protocgen"
	"google.golang.org/protobuf/compiler/protogen"
)

func main() {
	protogen.Options{}.Run(protocgen.Generate)
}
