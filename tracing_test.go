package weaver

import (
	"context"
	"math/rand"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

func TestTraceSamplerInterval(t *testing.T) {
	sampler := newTraceSampler(time.Second, rand.New(rand.NewSource(1)))
	now := time.Unix(10, 0)
	if !sampler.shouldTrace(now) {
		t.Fatal("首次请求应被采样")
	}

	next := sampler.nextSampleTime.Load()
	if next <= now.UnixNano() {
		t.Fatalf("下一次采样时间没有推进: %d", next)
	}
	if sampler.shouldTrace(time.Unix(0, next-1)) {
		t.Fatal("采样间隔内的请求不应被采样")
	}
	if !sampler.shouldTrace(time.Unix(0, next)) {
		t.Fatal("到达下一次采样时间后请求应被采样")
	}
}

func TestTraceSamplerConcurrent(t *testing.T) {
	sampler := newTraceSampler(time.Second, rand.New(rand.NewSource(1)))
	now := time.Unix(10, 0)
	var sampled atomic.Int64

	var wait sync.WaitGroup
	for range 100 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			if sampler.shouldTrace(now) {
				sampled.Add(1)
			}
		}()
	}
	wait.Wait()

	if got := sampled.Load(); got != 1 {
		t.Fatalf("并发请求采样数为 %d，期望 1", got)
	}
}

func TestDefaultTraceSamplerFollowsParent(t *testing.T) {
	root := newTraceSampler(time.Hour, rand.New(rand.NewSource(1)))
	sampler := sdktrace.ParentBased(root)
	parameters := sdktrace.SamplingParameters{ParentContext: context.Background()}
	if result := sampler.ShouldSample(parameters); result.Decision != sdktrace.RecordAndSample {
		t.Fatalf("首次根 trace 的采样结果为 %v", result.Decision)
	}
	if result := sampler.ShouldSample(parameters); result.Decision != sdktrace.Drop {
		t.Fatalf("采样间隔内根 trace 的采样结果为 %v", result.Decision)
	}

	sampledParent := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    trace.TraceID{1},
		SpanID:     trace.SpanID{1},
		TraceFlags: trace.FlagsSampled,
	})
	sampledContext := trace.ContextWithSpanContext(context.Background(), sampledParent)
	if result := sampler.ShouldSample(sdktrace.SamplingParameters{ParentContext: sampledContext}); result.Decision != sdktrace.RecordAndSample {
		t.Fatalf("已采样父 span 的子 span 采样结果为 %v", result.Decision)
	}

	unsampledParent := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: trace.TraceID{1},
		SpanID:  trace.SpanID{1},
	})
	unsampledContext := trace.ContextWithSpanContext(context.Background(), unsampledParent)
	if result := sampler.ShouldSample(sdktrace.SamplingParameters{ParentContext: unsampledContext}); result.Decision != sdktrace.Drop {
		t.Fatalf("未采样父 span 的子 span 采样结果为 %v", result.Decision)
	}
}

func TestNewTracerProviderAcceptsSDKOptions(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	provider := NewTracerProvider(
		sdktrace.WithSyncer(exporter),
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	)
	t.Cleanup(func() {
		if err := provider.Shutdown(context.Background()); err != nil {
			t.Errorf("关闭 TracerProvider 失败: %v", err)
		}
	})

	_, span := provider.Tracer("test").Start(context.Background(), "custom-options")
	span.End()

	spans := exporter.GetSpans()
	if len(spans) != 1 || spans[0].Name != "custom-options" {
		t.Fatalf("exporter 收到的 spans 不符合预期: %#v", spans)
	}
}
