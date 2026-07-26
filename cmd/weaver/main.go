package main

import (
	"context"
	"fmt"
	"os"

	"github.com/project-kgo/weaver/internal/generate"
)

func main() {
	if len(os.Args) < 2 || os.Args[1] != "generate" {
		fmt.Fprintln(os.Stderr, "usage: weaver generate [package patterns...]")
		os.Exit(2)
	}
	patterns := os.Args[2:]
	outputs, err := generate.Generate(context.Background(), ".", patterns...)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := generate.Write(outputs); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	for _, output := range outputs {
		if output.Remove {
			fmt.Println("removed", output.Filename)
		} else {
			fmt.Println("generated", output.Filename)
		}
	}
}
