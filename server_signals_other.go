//go:build !unix

package weaver

import "os"

func shutdownSignals() []os.Signal {
	return []os.Signal{os.Interrupt}
}
