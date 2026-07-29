package weaver

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	"go.opentelemetry.io/otel/trace"
)

func TestNewLoggerUsesSlogHandler(t *testing.T) {
	if _, ok := NewLogger().Handler().(*slogHandler); !ok {
		t.Fatalf("NewLogger Handler 类型为 %T，期望 *slogHandler", NewLogger().Handler())
	}
}

func TestSlogHandlerAddsTraceID(t *testing.T) {
	var output bytes.Buffer
	handler := &slogHandler{Handler: slog.NewJSONHandler(&output, nil)}
	logger := slog.New(handler).With("component", "test")
	traceID := trace.TraceID{1, 2, 3}
	spanContext := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: traceID,
		SpanID:  trace.SpanID{4, 5, 6},
	})
	ctx := trace.ContextWithSpanContext(context.Background(), spanContext)

	logger.InfoContext(ctx, "test message")

	var record map[string]any
	if err := json.Unmarshal(output.Bytes(), &record); err != nil {
		t.Fatalf("解析日志失败: %v", err)
	}
	if got := record["trace_id"]; got != traceID.String() {
		t.Fatalf("trace_id = %v，期望 %s", got, traceID)
	}
}

func TestSlogHandlerOmitsInvalidTraceID(t *testing.T) {
	var output bytes.Buffer
	handler := &slogHandler{Handler: slog.NewJSONHandler(&output, nil)}
	logger := slog.New(handler)

	logger.InfoContext(context.Background(), "test message")

	var record map[string]any
	if err := json.Unmarshal(output.Bytes(), &record); err != nil {
		t.Fatalf("解析日志失败: %v", err)
	}
	if _, ok := record["trace_id"]; ok {
		t.Fatal("无有效 trace 时不应记录 trace_id")
	}
}
