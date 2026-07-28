package weaver

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

var (
	testSpanRecorder *tracetest.SpanRecorder
	testMetricReader *sdkmetric.ManualReader
)

func TestMain(m *testing.M) {
	testSpanRecorder = tracetest.NewSpanRecorder()
	tracerProvider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(testSpanRecorder))
	testMetricReader = sdkmetric.NewManualReader()
	meterProvider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(testMetricReader))
	otel.SetTracerProvider(tracerProvider)
	otel.SetMeterProvider(meterProvider)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	code := m.Run()
	_ = tracerProvider.Shutdown(context.Background())
	_ = meterProvider.Shutdown(context.Background())
	os.Exit(code)
}

func TestLocalCallTelemetryAndRecovery(t *testing.T) {
	tests := []struct {
		name       string
		invoke     upperAPI
		context    func(*testing.T) context.Context
		wantResult string
		wantCode   connect.Code
		wantStatus codes.Code
		wantEvent  bool
		wantLog    bool
	}{
		{
			name: "success",
			invoke: upperFunc(func(_ context.Context, request *wrapperspb.StringValue) (*wrapperspb.StringValue, error) {
				return wrapperspb.String(strings.ToUpper(request.Value)), nil
			}),
			wantResult: "ok",
		},
		{
			name: "error",
			invoke: upperFunc(func(context.Context, *wrapperspb.StringValue) (*wrapperspb.StringValue, error) {
				// 故意同时返回 response 和 error，验证本地代理不会泄漏无效 response。
				return wrapperspb.String("invalid"), errors.New("local failure")
			}),
			wantCode:   connect.CodeUnknown,
			wantResult: connect.CodeUnknown.String(),
			wantStatus: codes.Error,
			wantEvent:  true,
		},
		{
			name: "panic",
			invoke: upperFunc(func(context.Context, *wrapperspb.StringValue) (*wrapperspb.StringValue, error) {
				panic("local panic detail")
			}),
			wantCode:   connect.CodeInternal,
			wantResult: connect.CodeInternal.String(),
			wantStatus: codes.Error,
			wantEvent:  true,
			wantLog:    true,
		},
		{
			name: "canceled",
			invoke: upperFunc(func(ctx context.Context, _ *wrapperspb.StringValue) (*wrapperspb.StringValue, error) {
				return nil, ctx.Err()
			}),
			context: func(t *testing.T) context.Context {
				t.Helper()
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return ctx
			},
			wantCode:   connect.CodeCanceled,
			wantResult: connect.CodeCanceled.String(),
			wantStatus: codes.Error,
			wantEvent:  true,
		},
		{
			name: "deadline",
			invoke: upperFunc(func(ctx context.Context, _ *wrapperspb.StringValue) (*wrapperspb.StringValue, error) {
				return nil, ctx.Err()
			}),
			context: func(t *testing.T) context.Context {
				t.Helper()
				ctx, cancel := context.WithDeadline(context.Background(), time.Unix(1, 0))
				t.Cleanup(cancel)
				return ctx
			},
			wantCode:   connect.CodeDeadlineExceeded,
			wantResult: connect.CodeDeadlineExceeded.String(),
			wantStatus: codes.Error,
			wantEvent:  true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			before := len(testSpanRecorder.Ended())
			var logs bytes.Buffer
			previousLogger := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
			t.Cleanup(func() { slog.SetDefault(previousLogger) })

			request := wrapperspb.String("hello")
			ctx := context.Background()
			if test.context != nil {
				ctx = test.context(t)
			}
			response, err := (upperLocalClient{implementation: test.invoke}).Upper(ctx, request)
			if request.Value != "hello" {
				t.Fatalf("request was modified: %q", request.Value)
			}
			if err == nil && test.wantResult != "ok" {
				t.Fatalf("expected error code %v", test.wantCode)
			}
			if err != nil && connect.CodeOf(err) != test.wantCode {
				t.Fatalf("expected code %v, got %v (%v)", test.wantCode, connect.CodeOf(err), err)
			}
			if err != nil && response != nil {
				t.Fatalf("error response must be nil, got %v", response)
			}
			if err == nil && (response == nil || response.Value != "HELLO") {
				t.Fatalf("unexpected success response: %v", response)
			}

			spans := testSpanRecorder.Ended()[before:]
			if len(spans) != 1 {
				t.Fatalf("expected one local span, got %d", len(spans))
			}
			span := spans[0]
			if span.Name() != strings.TrimLeft(upperProcedure, "/") || span.SpanKind() != trace.SpanKindInternal {
				t.Fatalf("unexpected local span name=%q kind=%v", span.Name(), span.SpanKind())
			}
			if span.Status().Code != test.wantStatus {
				t.Fatalf("unexpected span status: %v", span.Status())
			}
			if got := spanAttribute(span.Attributes(), "weaver.status_code"); got != test.wantResult {
				t.Fatalf("unexpected span result %q", got)
			}
			if hasSpanEvent(span, "exception") != test.wantEvent {
				t.Fatalf("unexpected exception event state: %#v", span.Events())
			}
			if strings.Contains(logs.String(), "local panic detail") != test.wantLog {
				t.Fatalf("unexpected panic log: %s", logs.String())
			}
			if test.wantLog && (!strings.Contains(logs.String(), "stack=") || !strings.Contains(logs.String(), strings.TrimLeft(upperProcedure, "/"))) {
				t.Fatalf("panic log lacks procedure or stack: %s", logs.String())
			}
			assertHistogramPoint(t, "weaver.local.call.duration", test.wantResult)
		})
	}
}

func TestCrossUnitTelemetryAndInterceptorOrder(t *testing.T) {
	prefix := "prefix:"
	var coreEvents []string
	var coreUpper *upperImpl
	var unusedCaller *callerImpl
	coreRegistry := testRegistry(t, &coreEvents, &coreUpper, &unusedCaller)
	recorder := newInterceptorRecorder()
	coreConfig := Config{
		Units:      map[string]string{"core": "", "game": "http://unused.invalid"},
		Placements: map[string]string{upperServiceName: "core", callerServiceName: "game"},
	}
	coreRuntime, err := New(
		context.Background(),
		"core",
		coreConfig,
		WithRegistry(coreRegistry),
		WithResource(&prefix),
		WithHandlerInterceptors(recorder.interceptor("handler-1")),
		WithHandlerInterceptors(recorder.interceptor("handler-2")),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer coreRuntime.Shutdown(context.Background())
	server := startHTTP2TestServer(coreRuntime.Handler(), false)
	defer server.Close()

	var gameEvents []string
	var unusedUpper *upperImpl
	var gameCaller *callerImpl
	gameRegistry := testRegistry(t, &gameEvents, &unusedUpper, &gameCaller)
	gameConfig := Config{
		Units:      map[string]string{"core": server.URL, "game": ""},
		Placements: map[string]string{upperServiceName: "core", callerServiceName: "game"},
	}
	before := len(testSpanRecorder.Ended())
	gameRuntime, err := New(
		context.Background(),
		"game",
		gameConfig,
		WithRegistry(gameRegistry),
		WithClientInterceptors(recorder.interceptor("client-1")),
		WithClientInterceptors(recorder.interceptor("client-2")),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer gameRuntime.Shutdown(context.Background())

	response, err := gameCaller.Call(context.Background(), wrapperspb.String("hello"))
	if err != nil {
		t.Fatal(err)
	}
	if response.Value != "prefix:HELLO" {
		t.Fatalf("unexpected response %q", response.Value)
	}
	wantOneCall := []string{
		"client-1:before", "client-2:before",
		"handler-1:before", "handler-2:before",
		"handler-2:after", "handler-1:after",
		"client-2:after", "client-1:after",
	}
	wantEvents := append(append([]string(nil), wantOneCall...), wantOneCall...)
	if got := recorder.values(); !reflect.DeepEqual(got, wantEvents) {
		t.Fatalf("unexpected interceptor order:\nwant %v\n got %v", wantEvents, got)
	}

	spans := testSpanRecorder.Ended()[before:]
	procedure := strings.TrimLeft(upperProcedure, "/")
	var clientSpans, serverSpans []sdktrace.ReadOnlySpan
	for _, span := range spans {
		if span.Name() != procedure {
			continue
		}
		switch span.SpanKind() {
		case trace.SpanKindClient:
			clientSpans = append(clientSpans, span)
		case trace.SpanKindServer:
			serverSpans = append(serverSpans, span)
		}
	}
	if len(clientSpans) != 2 || len(serverSpans) != 2 {
		t.Fatalf("expected two client/server spans, got client=%d server=%d", len(clientSpans), len(serverSpans))
	}
	for _, serverSpan := range serverSpans {
		matched := false
		for _, clientSpan := range clientSpans {
			if serverSpan.SpanContext().TraceID() == clientSpan.SpanContext().TraceID() && serverSpan.Parent().SpanID() == clientSpan.SpanContext().SpanID() {
				matched = true
				break
			}
		}
		if !matched {
			t.Fatalf("server span %s is not a child of a client span", serverSpan.SpanContext().SpanID())
		}
	}
	assertMetricExists(t, "rpc.client.duration")
	assertMetricExists(t, "rpc.server.duration")
}

func TestCustomInterceptorsDoNotRunForLocalCalls(t *testing.T) {
	prefix := "x:"
	var events []string
	var upper *upperImpl
	var caller *callerImpl
	registry := testRegistry(t, &events, &upper, &caller)
	recorder := newInterceptorRecorder()
	config := Config{
		Units:      map[string]string{"app": ""},
		Placements: map[string]string{upperServiceName: "app", callerServiceName: "app"},
	}
	runtime, err := New(
		context.Background(),
		"app",
		config,
		WithRegistry(registry),
		WithResource(&prefix),
		WithClientInterceptors(recorder.interceptor("client")),
		WithHandlerInterceptors(recorder.interceptor("handler")),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Shutdown(context.Background())
	if _, err := caller.Call(context.Background(), wrapperspb.String("hello")); err != nil {
		t.Fatal(err)
	}
	if got := recorder.values(); len(got) != 0 {
		t.Fatalf("local calls unexpectedly ran Connect interceptors: %v", got)
	}
}

func TestHandlerRecovery(t *testing.T) {
	tests := []struct {
		name        string
		request     string
		interceptor connect.Interceptor
		panicText   string
	}{
		{name: "service panic", request: "panic", panicText: "upper panic"},
		{
			name:      "handler interceptor panic",
			request:   "ok",
			panicText: "handler panic",
			interceptor: func() connect.Interceptor {
				var first atomic.Bool
				return connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
					return func(ctx context.Context, request connect.AnyRequest) (connect.AnyResponse, error) {
						if !first.Swap(true) {
							panic("handler panic")
						}
						return next(ctx, request)
					}
				})
			}(),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			prefix := "x:"
			var events []string
			var upper *upperImpl
			var unusedCaller *callerImpl
			registry := testRegistry(t, &events, &upper, &unusedCaller)
			config := Config{
				Units:      map[string]string{"core": "", "game": "http://unused.invalid"},
				Placements: map[string]string{upperServiceName: "core", callerServiceName: "game"},
			}
			options := []Option{WithRegistry(registry), WithResource(&prefix)}
			if test.interceptor != nil {
				options = append(options, WithHandlerInterceptors(test.interceptor))
			}
			runtime, err := New(context.Background(), "core", config, options...)
			if err != nil {
				t.Fatal(err)
			}
			defer runtime.Shutdown(context.Background())
			server := startHTTP2TestServer(runtime.Handler(), false)
			defer server.Close()

			var logs bytes.Buffer
			previousLogger := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
			t.Cleanup(func() { slog.SetDefault(previousLogger) })
			before := len(testSpanRecorder.Ended())
			client := connect.NewClient[wrapperspb.StringValue, wrapperspb.StringValue](
				defaultHTTPClient(),
				server.URL+upperProcedure,
			)
			_, err = client.CallUnary(context.Background(), connect.NewRequest(wrapperspb.String(test.request)))
			if connect.CodeOf(err) != connect.CodeInternal {
				t.Fatalf("expected internal, got %v", err)
			}
			if strings.Contains(err.Error(), test.panicText) {
				t.Fatalf("panic detail leaked to caller: %v", err)
			}
			response, err := client.CallUnary(context.Background(), connect.NewRequest(wrapperspb.String("ok")))
			if err != nil || response.Msg.Value != "x:OK" {
				t.Fatalf("handler did not recover for subsequent request: response=%v err=%v", response, err)
			}
			if strings.Count(logs.String(), "组件调用发生 panic") != 1 || !strings.Contains(logs.String(), test.panicText) || !strings.Contains(logs.String(), "stack=") {
				t.Fatalf("unexpected recovery log: %s", logs.String())
			}
			foundException := false
			for _, span := range testSpanRecorder.Ended()[before:] {
				if span.SpanKind() == trace.SpanKindServer && hasSpanEvent(span, "exception") {
					foundException = true
					break
				}
			}
			if !foundException {
				t.Fatal("recovered panic did not add an exception event")
			}
		})
	}
}

func TestInterceptorOptionsRejectNil(t *testing.T) {
	var nilInterceptor connect.Interceptor
	var typedNil connect.UnaryInterceptorFunc
	for _, option := range []Option{
		WithClientInterceptors(nilInterceptor),
		WithClientInterceptors(typedNil),
		WithHandlerInterceptors(nilInterceptor),
		WithHandlerInterceptors(typedNil),
	} {
		options := newRuntimeOptions()
		if err := option.apply(&options); err == nil || !strings.Contains(err.Error(), "Interceptor 不能为空") {
			t.Fatalf("expected nil interceptor error, got %v", err)
		}
	}
}

type upperFunc func(context.Context, *wrapperspb.StringValue) (*wrapperspb.StringValue, error)

func (f upperFunc) Upper(ctx context.Context, request *wrapperspb.StringValue) (*wrapperspb.StringValue, error) {
	return f(ctx, request)
}

type interceptorRecorder struct {
	mu     sync.Mutex
	events []string
}

func newInterceptorRecorder() *interceptorRecorder {
	return new(interceptorRecorder)
}

func (r *interceptorRecorder) interceptor(name string) connect.Interceptor {
	return connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, request connect.AnyRequest) (connect.AnyResponse, error) {
			r.add(name + ":before")
			response, err := next(ctx, request)
			r.add(name + ":after")
			return response, err
		}
	})
}

func (r *interceptorRecorder) add(event string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, event)
}

func (r *interceptorRecorder) values() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.events...)
}

func hasSpanEvent(span sdktrace.ReadOnlySpan, name string) bool {
	for _, event := range span.Events() {
		if event.Name == name {
			return true
		}
	}
	return false
}

func spanAttribute(attributes []attribute.KeyValue, key string) string {
	for _, value := range attributes {
		if string(value.Key) == key {
			return value.Value.AsString()
		}
	}
	return ""
}

func assertHistogramPoint(t *testing.T, name, result string) {
	t.Helper()
	for _, metric := range collectMetrics(t) {
		if metric.Name != name {
			continue
		}
		histogram, ok := metric.Data.(metricdata.Histogram[float64])
		if !ok {
			t.Fatalf("metric %q has unexpected type %T", name, metric.Data)
		}
		for _, point := range histogram.DataPoints {
			if value, ok := point.Attributes.Value(attribute.Key("weaver.status_code")); ok && value.AsString() == result && point.Count > 0 {
				return
			}
		}
	}
	t.Fatalf("metric %q has no point for result %q", name, result)
}

func assertMetricExists(t *testing.T, name string) {
	t.Helper()
	for _, metric := range collectMetrics(t) {
		if metric.Name != name {
			continue
		}
		switch data := metric.Data.(type) {
		case metricdata.Histogram[int64]:
			for _, point := range data.DataPoints {
				if point.Count > 0 {
					return
				}
			}
		case metricdata.Histogram[float64]:
			for _, point := range data.DataPoints {
				if point.Count > 0 {
					return
				}
			}
		}
	}
	t.Fatalf("metric %q was not recorded", name)
}

func collectMetrics(t *testing.T) []metricdata.Metrics {
	t.Helper()
	var resources metricdata.ResourceMetrics
	if err := testMetricReader.Collect(context.Background(), &resources); err != nil {
		t.Fatal(err)
	}
	var result []metricdata.Metrics
	for _, scope := range resources.ScopeMetrics {
		result = append(result, scope.Metrics...)
	}
	return result
}
