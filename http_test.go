package weaver

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

const callbacksName = "test.Callbacks"

type callbacksImpl struct {
	upper  Ref[upperAPI]
	events *[]string
}

func (c *callbacksImpl) Init(context.Context) error {
	*c.events = append(*c.events, "callbacks:init")
	return nil
}

func (c *callbacksImpl) Shutdown(context.Context) error {
	*c.events = append(*c.events, "callbacks:shutdown")
	return nil
}

func (c *callbacksImpl) upperValue(writer http.ResponseWriter, request *http.Request) {
	response, err := c.upper.Get().Upper(request.Context(), wrapperspb.String(request.PathValue("value")))
	if err != nil {
		http.Error(writer, err.Error(), http.StatusBadGateway)
		return
	}
	_, _ = io.WriteString(writer, response.GetValue())
}

func (c *callbacksImpl) panics(http.ResponseWriter, *http.Request) {
	panic("callback panic")
}

func callbacksRegistration(events *[]string, routes ...Route) Registration {
	if len(routes) == 0 {
		routes = []Route{
			{Pattern: "POST /callbacks/upper/{value}", Handler: func(target any) http.HandlerFunc { return target.(*callbacksImpl).upperValue }},
			{Pattern: "POST /callbacks/panic", Handler: func(target any) http.HandlerFunc { return target.(*callbacksImpl).panics }},
		}
	}
	return Registration{
		HTTP: HTTPDescriptor{Name: callbacksName, Routes: routes},
		New:  func() any { return &callbacksImpl{events: events} },
		Inject: func(target any, injector Injector) error {
			upper, err := ResolveComponent[upperAPI](injector, upperServiceName)
			if err != nil {
				return err
			}
			SetRef(&target.(*callbacksImpl).upper, upper)
			return nil
		},
		Dependencies: []string{upperServiceName},
	}
}

func httpTestRegistry(t *testing.T, events *[]string, extra ...Registration) *Registry {
	t.Helper()
	var upper *upperImpl
	var caller *callerImpl
	registry := testRegistry(t, events, &upper, &caller)
	for _, registration := range extra {
		if err := registry.Register(registration); err != nil {
			t.Fatal(err)
		}
	}
	return registry
}

func postCallback(t *testing.T, handler http.Handler, path string) (int, string) {
	t.Helper()
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, path, nil))
	return recorder.Code, recorder.Body.String()
}

func TestHTTPComponentServesRoutes(t *testing.T) {
	prefix := "x:"
	var events []string
	registry := httpTestRegistry(t, &events, callbacksRegistration(&events))
	config := Config{
		Units: map[string]string{"app": ""},
		Placements: map[string]string{
			upperServiceName:  "app",
			callerServiceName: "app",
			callbacksName:     "app",
		},
	}
	var middlewareCalls []string
	middleware := func(name string) HTTPMiddleware {
		return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				middlewareCalls = append(middlewareCalls, name+":"+request.Pattern)
				next.ServeHTTP(writer, request)
			})
		}
	}
	runtime, err := New(
		context.Background(),
		"app",
		config,
		WithRegistry(registry),
		WithResource(&prefix),
		WithHTTPMiddleware(middleware("outer")),
		WithHTTPMiddleware(middleware("inner")),
	)
	if err != nil {
		t.Fatal(err)
	}

	status, body := postCallback(t, runtime.Handler(), "/callbacks/upper/abc")
	if status != http.StatusOK || body != "x:ABC" {
		t.Fatalf("unexpected callback response: %d %q", status, body)
	}
	wantCalls := []string{"outer:POST /callbacks/upper/{value}", "inner:POST /callbacks/upper/{value}"}
	if !reflect.DeepEqual(middlewareCalls, wantCalls) {
		t.Fatalf("unexpected middleware order: %v", middlewareCalls)
	}

	// 方法不匹配由 ServeMux 处理，Connect 请求不经过 HTTP 中间件。
	recorder := httptest.NewRecorder()
	runtime.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/callbacks/upper/abc", nil))
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", recorder.Code)
	}
	middlewareCalls = nil
	client := callerService().NewRemote(handlerHTTPClient{handler: runtime.Handler()}, "http://app")
	if _, err := client.Call(context.Background(), wrapperspb.String("abc")); err != nil {
		t.Fatal(err)
	}
	if len(middlewareCalls) != 0 {
		t.Fatalf("HTTP middleware must not wrap Connect handlers: %v", middlewareCalls)
	}

	if err := runtime.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := []string{"upper:init", "callbacks:init", "caller:init", "caller:shutdown", "callbacks:shutdown", "upper:shutdown"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("unexpected lifecycle order: %v", events)
	}
}

func TestHTTPComponentRecoversPanic(t *testing.T) {
	prefix := "x:"
	var events []string
	registry := httpTestRegistry(t, &events, callbacksRegistration(&events))
	config := Config{
		Units:      map[string]string{"app": ""},
		Placements: map[string]string{upperServiceName: "app", callerServiceName: "app", callbacksName: "app"},
	}
	runtime, err := New(context.Background(), "app", config, WithRegistry(registry), WithResource(&prefix))
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Shutdown(context.Background())

	status, body := postCallback(t, runtime.Handler(), "/callbacks/panic")
	if status != http.StatusInternalServerError || strings.Contains(body, "callback panic") {
		t.Fatalf("unexpected panic response: %d %q", status, body)
	}
}

func TestHTTPRouteTelemetry(t *testing.T) {
	prefix := "x:"
	var events []string
	registry := httpTestRegistry(t, &events, callbacksRegistration(&events))
	config := Config{
		Units:      map[string]string{"app": ""},
		Placements: map[string]string{upperServiceName: "app", callerServiceName: "app", callbacksName: "app"},
	}
	runtime, err := New(context.Background(), "app", config, WithRegistry(registry), WithResource(&prefix))
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Shutdown(context.Background())

	before := len(testSpanRecorder.Ended())
	postCallback(t, runtime.Handler(), "/callbacks/upper/abc")
	postCallback(t, runtime.Handler(), "/callbacks/panic")

	var routeSpan, panicSpan bool
	for _, span := range testSpanRecorder.Ended()[before:] {
		if span.SpanKind() != trace.SpanKindServer {
			continue
		}
		attributes := attribute.NewSet(span.Attributes()...)
		component, _ := attributes.Value("weaver.component")
		method, _ := attributes.Value("http.request.method")
		status, _ := attributes.Value("http.response.status_code")
		if component.AsString() != callbacksName || method.AsString() != http.MethodPost {
			t.Fatalf("unexpected span attributes: %v", span.Attributes())
		}
		switch span.Name() {
		case "POST /callbacks/upper/{value}":
			routeSpan = status.AsInt64() == http.StatusOK && span.Status().Code == codes.Unset
		case "POST /callbacks/panic":
			panicSpan = status.AsInt64() == http.StatusInternalServerError && span.Status().Code == codes.Error
		}
	}
	if !routeSpan || !panicSpan {
		t.Fatalf("missing HTTP route spans: route=%v panic=%v", routeSpan, panicSpan)
	}

	points := make(map[string]int64)
	for _, value := range collectMetrics(t) {
		if value.Name != "http.server.request.duration" {
			continue
		}
		histogram, ok := value.Data.(metricdata.Histogram[float64])
		if !ok {
			t.Fatalf("unexpected metric type %T", value.Data)
		}
		for _, point := range histogram.DataPoints {
			route, _ := point.Attributes.Value("http.route")
			status, _ := point.Attributes.Value("http.response.status_code")
			points[route.AsString()+" "+status.Emit()] += int64(point.Count)
		}
	}
	for _, key := range []string{"POST /callbacks/upper/{value} 200", "POST /callbacks/panic 500"} {
		if points[key] == 0 {
			t.Fatalf("metric point %q not recorded: %v", key, points)
		}
	}
}

func TestNormalizeHTTPMethod(t *testing.T) {
	if got := normalizeHTTPMethod(http.MethodPost); got != http.MethodPost {
		t.Fatalf("unexpected method %q", got)
	}
	if got := normalizeHTTPMethod("PROPFIND"); got != "_OTHER" {
		t.Fatalf("unexpected method %q", got)
	}
}

func TestHTTPComponentUsesRemoteRef(t *testing.T) {
	prefix := "core:"
	var coreEvents, edgeEvents []string
	coreRegistry := httpTestRegistry(t, &coreEvents, callbacksRegistration(&coreEvents))
	config := Config{
		Units:      map[string]string{"core": "", "edge": "http://edge.invalid"},
		Placements: map[string]string{upperServiceName: "core", callerServiceName: "core", callbacksName: "edge"},
	}
	coreRuntime, err := New(context.Background(), "core", config, WithRegistry(coreRegistry), WithResource(&prefix))
	if err != nil {
		t.Fatal(err)
	}
	defer coreRuntime.Shutdown(context.Background())
	if status, _ := postCallback(t, coreRuntime.Handler(), "/callbacks/upper/abc"); status != http.StatusNotFound {
		t.Fatalf("HTTP component placed on another unit must not be mounted, got %d", status)
	}
	for _, event := range coreEvents {
		if strings.HasPrefix(event, "callbacks:") {
			t.Fatalf("remote HTTP component must not be created: %v", coreEvents)
		}
	}

	server := startHTTP2TestServer(coreRuntime.Handler(), false)
	defer server.Close()
	config.Units["core"] = server.URL
	edgeRegistry := httpTestRegistry(t, &edgeEvents, callbacksRegistration(&edgeEvents))
	edgeRuntime, err := New(context.Background(), "edge", config, WithRegistry(edgeRegistry))
	if err != nil {
		t.Fatal(err)
	}
	defer edgeRuntime.Shutdown(context.Background())

	status, body := postCallback(t, edgeRuntime.Handler(), "/callbacks/upper/abc")
	if status != http.StatusOK || body != "core:ABC" {
		t.Fatalf("unexpected callback response: %d %q", status, body)
	}
}

func TestHTTPRouteValidation(t *testing.T) {
	newRuntime := func(t *testing.T, events *[]string, extra ...Registration) error {
		t.Helper()
		prefix := "x:"
		registry := httpTestRegistry(t, events, extra...)
		placements := map[string]string{upperServiceName: "app", callerServiceName: "app"}
		for _, registration := range extra {
			placements[registration.name()] = "app"
		}
		config := Config{Units: map[string]string{"app": ""}, Placements: placements}
		runtime, err := New(context.Background(), "app", config, WithRegistry(registry), WithResource(&prefix))
		if err == nil {
			runtime.Shutdown(context.Background())
		}
		return err
	}
	handler := func(any) http.HandlerFunc { return func(http.ResponseWriter, *http.Request) {} }

	t.Run("conflict across components", func(t *testing.T) {
		var events []string
		second := callbacksRegistration(&events, Route{Pattern: "POST /callbacks/upper/{name}", Handler: handler})
		second.HTTP.Name = "test.OtherCallbacks"
		err := newRuntime(t, &events, callbacksRegistration(&events), second)
		if err == nil || !strings.Contains(err.Error(), "conflicts with pattern") || !strings.Contains(err.Error(), "test.OtherCallbacks") {
			t.Fatalf("expected route conflict, got %v", err)
		}
		if len(events) != 0 {
			t.Fatalf("route validation must fail before Init: %v", events)
		}
	})

	t.Run("reserved connect path", func(t *testing.T) {
		var events []string
		registration := callbacksRegistration(&events, Route{Pattern: "POST /" + upperServiceName + "/Hook", Handler: handler})
		err := newRuntime(t, &events, registration)
		if err == nil || !strings.Contains(err.Error(), "Connect 路径冲突") {
			t.Fatalf("expected reserved path error, got %v", err)
		}
	})

	t.Run("invalid pattern", func(t *testing.T) {
		var events []string
		registration := callbacksRegistration(&events, Route{Pattern: "POST callbacks", Handler: handler})
		err := newRuntime(t, &events, registration)
		if err == nil || !strings.Contains(err.Error(), "路由 \"POST callbacks\" 无效") {
			t.Fatalf("expected invalid pattern error, got %v", err)
		}
	})

	t.Run("ref to HTTP component", func(t *testing.T) {
		var events []string
		registration := callbacksRegistration(&events)
		registration.HTTP.Name = "test.Target"
		referrer := callbacksRegistration(&events, Route{Pattern: "POST /referrer", Handler: handler})
		referrer.Inject = func(_ any, injector Injector) error {
			_, err := ResolveComponent[upperAPI](injector, "test.Target")
			return err
		}
		referrer.Dependencies = []string{"test.Target"}
		err := newRuntime(t, &events, registration, referrer)
		if err == nil || !strings.Contains(err.Error(), "不能通过 Ref 引用") {
			t.Fatalf("expected Ref rejection, got %v", err)
		}
	})

	t.Run("nil middleware result", func(t *testing.T) {
		prefix := "x:"
		var events []string
		registry := httpTestRegistry(t, &events, callbacksRegistration(&events))
		config := Config{
			Units:      map[string]string{"app": ""},
			Placements: map[string]string{upperServiceName: "app", callerServiceName: "app", callbacksName: "app"},
		}
		_, err := New(
			context.Background(), "app", config,
			WithRegistry(registry), WithResource(&prefix),
			WithHTTPMiddleware(func(http.Handler) http.Handler { return nil }),
		)
		if err == nil || !strings.Contains(err.Error(), "返回了 nil") {
			t.Fatalf("expected nil middleware error, got %v", err)
		}
		want := []string{"upper:init", "callbacks:init", "caller:init", "caller:shutdown", "callbacks:shutdown", "upper:shutdown"}
		if !reflect.DeepEqual(events, want) {
			t.Fatalf("unexpected rollback order: %v", events)
		}
		if _, err := New(context.Background(), "app", config, WithHTTPMiddleware(nil)); err == nil || !strings.Contains(err.Error(), "HTTP 中间件不能为空") {
			t.Fatalf("expected nil middleware option error, got %v", err)
		}
	})
}

func TestHTTPRegistrationValidation(t *testing.T) {
	var events []string
	handler := func(any) http.HandlerFunc { return func(http.ResponseWriter, *http.Request) {} }
	tests := []struct {
		name   string
		modify func(*Registration)
		want   string
	}{
		{name: "no routes", modify: func(r *Registration) { r.HTTP.Routes = nil }, want: "没有声明路由"},
		{name: "nil handler", modify: func(r *Registration) { r.HTTP.Routes = []Route{{Pattern: "POST /x"}} }, want: "无效路由"},
		{name: "empty pattern", modify: func(r *Registration) { r.HTTP.Routes = []Route{{Pattern: " ", Handler: handler}} }, want: "无效路由"},
		{name: "both kinds", modify: func(r *Registration) { r.Service = upperService().Descriptor() }, want: "不能同时是 protobuf 组件和 HTTP 组件"},
		{name: "same name as service", modify: func(r *Registration) { r.HTTP.Name = upperServiceName }, want: "重复注册"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			registry := httpTestRegistry(t, &events)
			registration := callbacksRegistration(&events)
			test.modify(&registration)
			if err := registry.Register(registration); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("expected %q, got %v", test.want, err)
			}
		})
	}
}
