package weaver

import (
	"log/slog"
)

func init() {
	sl := NewLogger()
	slog.SetDefault(sl)

	NewTracerProvider()
}
