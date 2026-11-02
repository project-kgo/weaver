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
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
)

const (
	telemetryInstrumentationName = "github.com/project-kgo/weaver"
	statusCodeOK                 = "OK"
)

var recoveredPanicError = errors.New("weaver: internal error")

// durationBuckets 是 OpenTelemetry 语义约定为秒级耗时直方图推荐的分桶边界。
// SDK 默认分桶面向毫秒，秒级数据几乎全部落入第一个桶，因此创建直方图时必须显式指定。
var durationBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.075, 0.1, 0.25, 0.5, 0.75, 1, 2.5, 5, 7.5, 10}

var localCallMetrics struct {
	sync.Once
	duration metric.Float64Histogram
}

// LocalCall 保存一次同 unit 组件调用的埋点状态，仅供生成代码使用。
type LocalCall struct {
	ctx     context.Context
	span    trace.Span
	started time.Time
	method  string
}

// BeginLocalCall 开始一次同 unit 组件调用，仅供生成代码使用。
func BeginLocalCall(ctx context.Context, procedure string) (context.Context, *LocalCall) {
	// rpc.method 与 otelconnect 一致使用 "包名.Service/Method"，使同一方法在本地和远程部署下可按相同维度聚合。
	method := strings.TrimLeft(strings.TrimSpace(procedure), "/")
	ctx, span := otel.Tracer(telemetryInstrumentationName).Start(
		ctx,
		method,
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(
			semconv.RPCMethodKey.String(method),
			attribute.Bool("weaver.local", true),
		),
	)
	return ctx, &LocalCall{
		ctx:     ctx,
		span:    span,
		started: time.Now(),
		method:  method,
	}
}

// End 结束同 unit 调用并记录结果，仅供生成代码使用。
func (call *LocalCall) End(err error) {
	if call == nil {
		return
	}
	attributes := []attribute.KeyValue{semconv.RPCMethodKey.String(call.method)}
	if err == nil {
		attributes = append(attributes, semconv.RPCResponseStatusCodeKey.String(statusCodeOK))
	} else {
		code := rpcStatusCode(connect.CodeOf(err))
		attributes = append(attributes,
			semconv.RPCResponseStatusCodeKey.String(code),
			semconv.ErrorTypeKey.String(code),
		)
		if !errors.Is(err, recoveredPanicError) {
			call.span.RecordError(err)
		}
		call.span.SetStatus(codes.Error, code)
	}
	call.span.SetAttributes(attributes[1:]...)

	if duration := localCallDuration(); duration != nil {
		duration.Record(
			context.WithoutCancel(call.ctx),
			time.Since(call.started).Seconds(),
			metric.WithAttributes(attributes...),
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
			metric.WithExplicitBucketBoundaries(durationBuckets...),
		)
		if err != nil {
			slog.Error("weaver: 创建本地调用指标失败", "error", err)
			return
		}
		localCallMetrics.duration = duration
	})
	return localCallMetrics.duration
}

// rpcStatusCode 与 otelconnect 保持一致，使用 gRPC 风格的大写结果码，例如 DEADLINE_EXCEEDED。
func rpcStatusCode(code connect.Code) string {
	return strings.ToUpper(code.String())
}
