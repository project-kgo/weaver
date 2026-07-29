package weaver

import (
	"math/rand"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

const defaultTraceSampleInterval = time.Second

// NewTracerProvider 创建适合 Weaver Runtime 的 OpenTelemetry TracerProvider。
//
// 默认采样器平均每秒采样一条由当前进程发起的根 trace，并让子 span 跟随父
// span 的采样决定。传入的 SDK 选项在默认项之后生效，因此业务可以通过
// sdktrace.WithBatcher 配置 exporter，或通过 sdktrace.WithSampler 覆盖默认采样器。
func NewTracerProvider(options ...sdktrace.TracerProviderOption) *sdktrace.TracerProvider {
	defaults := []sdktrace.TracerProviderOption{
		sdktrace.WithSampler(sdktrace.ParentBased(newTraceSampler(
			defaultTraceSampleInterval,
			rand.New(rand.NewSource(time.Now().UnixNano())),
		))),
	}
	tp := sdktrace.NewTracerProvider(append(defaults, options...)...)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(
		propagation.NewCompositeTextMapPropagator(
			propagation.TraceContext{},
			propagation.Baggage{},
		),
	)
	return tp
}

// traceSampler 是基于时间的根 trace 采样器。每次采样后会随机决定下一次
// 允许采样的时间，使采样间隔期望值等于 interval，同时避免进程间同步采样。
type traceSampler struct {
	intervalNanos float64

	mu             sync.Mutex
	rng            *rand.Rand
	nextSampleTime atomic.Int64
}

func newTraceSampler(interval time.Duration, rng *rand.Rand) *traceSampler {
	return &traceSampler{
		intervalNanos: float64(interval / time.Nanosecond),
		rng:           rng,
	}
}

func (s *traceSampler) ShouldSample(parameters sdktrace.SamplingParameters) sdktrace.SamplingResult {
	decision := sdktrace.Drop
	if s.shouldTrace(time.Now()) {
		decision = sdktrace.RecordAndSample
	}
	return sdktrace.SamplingResult{
		Decision:   decision,
		Tracestate: trace.SpanContextFromContext(parameters.ParentContext).TraceState(),
	}
}

func (s *traceSampler) Description() string {
	return "WeaverTimeBasedSampler"
}

func (s *traceSampler) shouldTrace(now time.Time) bool {
	nowNanos := now.UnixNano()
	if nowNanos < s.nextSampleTime.Load() {
		return false
	}

	// 快路径未命中后需要再次检查，确保并发请求中只有一个推进采样时间。
	s.mu.Lock()
	defer s.mu.Unlock()
	if nowNanos < s.nextSampleTime.Load() {
		return false
	}

	s.nextSampleTime.Store(nowNanos + int64(2*s.rng.Float64()*s.intervalNanos))
	return true
}
