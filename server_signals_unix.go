//go:build unix

package weaver

import (
	"os"
	"syscall"
)

func shutdownSignals() []os.Signal {
	return []os.Signal{
		os.Interrupt,
		syscall.SIGTERM,
		syscall.SIGHUP,
		syscall.SIGQUIT,
	}
}
