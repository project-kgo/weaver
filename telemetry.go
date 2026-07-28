package weaver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

const telemetryInstrumentationName = "github.com/project-kgo/weaver"

var recoveredPanicError = errors.New("weaver: internal error")

var localCallMetrics struct {
	sync.Once
	duration metric.Float64Histogram
}

// LocalCall 保存一次同 unit 组件调用的埋点状态，仅供生成代码使用。
type LocalCall struct {
	ctx     context.Context
	span    trace.Span
	started time.Time
	service string
	method  string
}

// BeginLocalCall 开始一次同 unit 组件调用，仅供生成代码使用。
func BeginLocalCall(ctx context.Context, procedure string) (context.Context, *LocalCall) {
	name, service, method := splitProcedure(procedure)
	attributes := []attribute.KeyValue{
		attribute.String("rpc.system", "weaver"),
		attribute.String("rpc.service", service),
		attribute.String("rpc.method", method),
		attribute.Bool("weaver.local", true),
	}
	ctx, span := otel.Tracer(telemetryInstrumentationName).Start(
		ctx,
		name,
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(attributes...),
	)
	return ctx, &LocalCall{
		ctx:     ctx,
		span:    span,
		started: time.Now(),
		service: service,
		method:  method,
	}
}

// End 结束同 unit 调用并记录结果，仅供生成代码使用。
func (call *LocalCall) End(err error) {
	if call == nil {
		return
	}
	result := "ok"
	if err != nil {
		result = connect.CodeOf(err).String()
		if !errors.Is(err, recoveredPanicError) {
			call.span.RecordError(err)
		}
		call.span.SetStatus(codes.Error, result)
	}
	call.span.SetAttributes(attribute.String("weaver.status_code", result))

	metricAttributes := []attribute.KeyValue{
		attribute.String("rpc.service", call.service),
		attribute.String("rpc.method", call.method),
		attribute.String("weaver.status_code", result),
	}
	if duration := localCallDuration(); duration != nil {
		duration.Record(
			context.WithoutCancel(call.ctx),
			time.Since(call.started).Seconds(),
			metric.WithAttributes(metricAttributes...),
		)
	}
	call.span.End()
}

// RecoverPanic 记录组件调用 panic 并返回不会泄漏 panic 内容的错误，仅供 Runtime 和生成代码使用。
func RecoverPanic(ctx context.Context, procedure string, recovered any) error {
	stack := debug.Stack()
	slog.ErrorContext(
		ctx,
		"[panic]",
		"procedure", strings.TrimLeft(procedure, "/"),
		"panic", recovered,
		"stack", string(stack),
	)
	trace.SpanFromContext(ctx).AddEvent(
		"exception",
		trace.WithAttributes(
			attribute.String("exception.type", fmt.Sprintf("%T", recovered)),
			attribute.String("exception.message", fmt.Sprint(recovered)),
			attribute.String("exception.stacktrace", string(stack)),
		),
	)
	return connect.NewError(connect.CodeInternal, recoveredPanicError)
}

func localCallDuration() metric.Float64Histogram {
	localCallMetrics.Do(func() {
		duration, err := otel.Meter(telemetryInstrumentationName).Float64Histogram(
			"weaver.local.call.duration",
			metric.WithUnit("s"),
			metric.WithDescription("同 unit 组件调用耗时"),
		)
		if err != nil {
			slog.Error("weaver: 创建本地调用指标失败", "error", err)
			return
		}
		localCallMetrics.duration = duration
	})
	return localCallMetrics.duration
}

func splitProcedure(procedure string) (name, service, method string) {
	name = strings.TrimLeft(strings.TrimSpace(procedure), "/")
	service, method, _ = strings.Cut(name, "/")
	if method == "" {
		method = service
		service = ""
	}
	return name, service, method
}
