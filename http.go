package weaver

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

var httpRouteMetrics struct {
	sync.Once
	duration metric.Float64Histogram
}

// HTTPDescriptor 描述 weaver generate 根据 //weaver:http 与 //weaver:route
// 生成的原生 HTTP 组件。HTTP 组件只接收外部请求，不能被 Ref 引用。
type HTTPDescriptor struct {
	Name   string
	Routes []Route
}

// Route 是一条原生 HTTP 路由，Pattern 使用 net/http.ServeMux 的语法。
type Route struct {
	Pattern string
	Handler func(any) http.HandlerFunc
}

// HTTPMiddleware 是标准库风格的 HTTP 中间件。
type HTTPMiddleware func(http.Handler) http.Handler

func (d HTTPDescriptor) validate() error {
	if len(d.Routes) == 0 {
		return fmt.Errorf("weaver: HTTP 组件 %q 没有声明路由", d.Name)
	}
	for _, route := range d.Routes {
		if strings.TrimSpace(route.Pattern) == "" || route.Handler == nil {
			return fmt.Errorf("weaver: HTTP 组件 %q 包含无效路由 %q", d.Name, route.Pattern)
		}
	}
	return nil
}

// routePath 返回 pattern 中的路径部分。方法和主机名都不包含 "/"，因此第一个 "/" 即路径起点。
func routePath(pattern string) string {
	index := strings.IndexByte(pattern, '/')
	if index < 0 {
		return ""
	}
	return pattern[index:]
}

// handleRoute 把 ServeMux 对非法或冲突 pattern 的 panic 转换为启动错误。
func handleRoute(mux *http.ServeMux, pattern string, handler http.Handler) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("%v", recovered)
		}
	}()
	mux.Handle(pattern, handler)
	return nil
}

// wrapHTTPRoute 为 HTTP 组件路由加上与 Connect handler 对齐的 tracing 和 panic recovery。
// 内置包装位于最外层，业务中间件中的 panic 同样会被恢复。
func wrapHTTPRoute(component, pattern string, handler http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		started := time.Now()
		attributes := []attribute.KeyValue{
			attribute.String("http.request.method", normalizeHTTPMethod(request.Method)),
			attribute.String("http.route", pattern),
			attribute.String("weaver.component", component),
		}
		ctx, span := otel.Tracer(telemetryInstrumentationName).Start(
			request.Context(),
			pattern,
			trace.WithSpanKind(trace.SpanKindServer),
			trace.WithAttributes(attributes...),
		)
		recorder := &statusRecorder{ResponseWriter: writer}
		defer func() {
			recovered := recover()
			if recovered == http.ErrAbortHandler {
				span.End()
				panic(recovered)
			}
			if recovered != nil {
				_ = RecoverPanic(ctx, pattern, recovered)
				if !recorder.wroteHeader {
					http.Error(recorder, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
				}
			}
			status := recorder.status
			if status == 0 {
				status = http.StatusOK
			}
			statusAttribute := attribute.Int("http.response.status_code", status)
			span.SetAttributes(statusAttribute)
			if status >= http.StatusInternalServerError {
				span.SetStatus(codes.Error, http.StatusText(status))
			}
			if duration := httpRouteDuration(); duration != nil {
				duration.Record(
					context.WithoutCancel(ctx),
					time.Since(started).Seconds(),
					metric.WithAttributes(append(attributes, statusAttribute)...),
				)
			}
			span.End()
		}()
		handler.ServeHTTP(recorder, request.WithContext(ctx))
	})
}

func httpRouteDuration() metric.Float64Histogram {
	httpRouteMetrics.Do(func() {
		duration, err := otel.Meter(telemetryInstrumentationName).Float64Histogram(
			"http.server.request.duration",
			metric.WithUnit("s"),
			metric.WithDescription("HTTP 组件路由请求耗时"),
		)
		if err != nil {
			slog.Error("weaver: 创建 HTTP 路由指标失败", "error", err)
			return
		}
		httpRouteMetrics.duration = duration
	})
	return httpRouteMetrics.duration
}

// normalizeHTTPMethod 把非标准方法归为 _OTHER，避免未声明方法的路由被任意方法撑高指标基数。
func normalizeHTTPMethod(method string) string {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch,
		http.MethodDelete, http.MethodConnect, http.MethodOptions, http.MethodTrace:
		return method
	default:
		return "_OTHER"
	}
}

// statusRecorder 记录响应状态码；Unwrap 让 http.ResponseController 仍能访问底层能力。
type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (r *statusRecorder) WriteHeader(status int) {
	if !r.wroteHeader {
		r.status = status
		r.wroteHeader = true
	}
	r.ResponseWriter.WriteHeader(status)
}

func (r *statusRecorder) Write(data []byte) (int, error) {
	if !r.wroteHeader {
		r.status = http.StatusOK
		r.wroteHeader = true
	}
	return r.ResponseWriter.Write(data)
}

func (r *statusRecorder) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}
