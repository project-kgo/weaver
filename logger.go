package weaver

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"go.opentelemetry.io/otel/trace"
)

type slogHandler struct {
	slog.Handler
}

// Handle 将当前 trace ID 注入日志记录。
func (s *slogHandler) Handle(ctx context.Context, r slog.Record) error {
	spanContext := trace.SpanContextFromContext(ctx)
	if spanContext.IsValid() {
		r.AddAttrs(slog.String("trace_id", spanContext.TraceID().String()))
	}
	src := r.Source()
	r.AddAttrs(slog.String(slog.SourceKey, fmt.Sprintf("%s:%s", src.File, src.Line)))
	return s.Handler.Handle(ctx, r)
}

// WithAttrs 保证派生 Handler 仍会注入 trace ID。
func (s *slogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &slogHandler{Handler: s.Handler.WithAttrs(attrs)}
}

// WithGroup 保证派生 Handler 仍会注入 trace ID。
func (s *slogHandler) WithGroup(name string) slog.Handler {
	return &slogHandler{Handler: s.Handler.WithGroup(name)}
}

var _ slog.Handler = (*slogHandler)(nil)

// NewLogger 创建输出 JSON 的默认日志记录器。
func NewLogger() *slog.Logger {
	handler := &slogHandler{Handler: slog.NewJSONHandler(os.Stdout, nil)}
	sl := slog.New(handler)

	return sl
}
