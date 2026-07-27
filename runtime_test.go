package weaver

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

const (
	upperServiceName  = "weaver.test.v1.UpperService"
	callerServiceName = "weaver.test.v1.CallerService"
	upperProcedure    = "/weaver.test.v1.UpperService/Upper"
	callerProcedure   = "/weaver.test.v1.CallerService/Call"
)

type upperAPI interface {
	Upper(context.Context, *wrapperspb.StringValue) (*wrapperspb.StringValue, error)
}

type callerAPI interface {
	Call(context.Context, *wrapperspb.StringValue) (*wrapperspb.StringValue, error)
}

type componentSettings struct {
	Prefix string `yaml:"prefix"`
}

type configurableUpper struct {
	WithConfig[componentSettings]
	initPrefix string
}

func (u *configurableUpper) Init(context.Context) error {
	u.initPrefix = u.Config().Prefix
	return nil
}

func (u *configurableUpper) Upper(context.Context, *wrapperspb.StringValue) (*wrapperspb.StringValue, error) {
	return wrapperspb.String(u.Config().Prefix), nil
}

type upperImpl struct {
	prefix Resource[*string]
	events *[]string
}

func (u *upperImpl) Init(context.Context) error {
	*u.events = append(*u.events, "upper:init")
	return nil
}

func (u *upperImpl) Shutdown(context.Context) error {
	*u.events = append(*u.events, "upper:shutdown")
	return nil
}

func (u *upperImpl) Upper(ctx context.Context, request *wrapperspb.StringValue) (*wrapperspb.StringValue, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if request.Value == "error" {
		return nil, errors.New("ordinary failure")
	}
	return wrapperspb.String(*u.prefix.Get() + strings.ToUpper(request.Value)), nil
}

type callerImpl struct {
	upper   Ref[upperAPI]
	second  Ref[upperAPI]
	events  *[]string
	initErr error
}

func (c *callerImpl) Init(context.Context) error {
	*c.events = append(*c.events, "caller:init")
	return c.initErr
}

func (c *callerImpl) Shutdown(context.Context) error {
	*c.events = append(*c.events, "caller:shutdown")
	return nil
}

func (c *callerImpl) Call(ctx context.Context, request *wrapperspb.StringValue) (*wrapperspb.StringValue, error) {
	// 调用两次用于验证同一组件的代理和 Resolver 会被缓存。
	if _, err := c.second.Get().Upper(ctx, wrapperspb.String("warmup")); err != nil {
		return nil, err
	}
	return c.upper.Get().Upper(ctx, request)
}

type upperRemoteClient struct {
	client *connect.Client[wrapperspb.StringValue, wrapperspb.StringValue]
}

func (c *upperRemoteClient) Upper(ctx context.Context, request *wrapperspb.StringValue) (*wrapperspb.StringValue, error) {
	response, err := c.client.CallUnary(ctx, connect.NewRequest(request))
	if err != nil {
		return nil, err
	}
	return response.Msg, nil
}

type callerRemoteClient struct {
	client *connect.Client[wrapperspb.StringValue, wrapperspb.StringValue]
}

func (c *callerRemoteClient) Call(ctx context.Context, request *wrapperspb.StringValue) (*wrapperspb.StringValue, error) {
	response, err := c.client.CallUnary(ctx, connect.NewRequest(request))
	if err != nil {
		return nil, err
	}
	return response.Msg, nil
}

type upperLocalClient struct{ implementation upperAPI }

func (c upperLocalClient) Upper(ctx context.Context, request *wrapperspb.StringValue) (*wrapperspb.StringValue, error) {
	response, err := c.implementation.Upper(ctx, request)
	return response, NormalizeError(err)
}

type callerLocalClient struct{ implementation callerAPI }

func (c callerLocalClient) Call(ctx context.Context, request *wrapperspb.StringValue) (*wrapperspb.StringValue, error) {
	response, err := c.implementation.Call(ctx, request)
	return response, NormalizeError(err)
}

func upperService() Service[upperAPI] {
	return Service[upperAPI]{
		Name:     upperServiceName,
		NewLocal: func(implementation upperAPI) upperAPI { return upperLocalClient{implementation: implementation} },
		NewRemote: func(client connect.HTTPClient, baseURL string, options ...connect.ClientOption) upperAPI {
			return &upperRemoteClient{client: connect.NewClient[wrapperspb.StringValue, wrapperspb.StringValue](client, baseURL+upperProcedure, options...)}
		},
		NewHandler: func(implementation upperAPI, options ...connect.HandlerOption) (string, http.Handler) {
			return upperProcedure, connect.NewUnaryHandlerSimple(upperProcedure, implementation.Upper, options...)
		},
	}
}

func callerService() Service[callerAPI] {
	return Service[callerAPI]{
		Name:     callerServiceName,
		NewLocal: func(implementation callerAPI) callerAPI { return callerLocalClient{implementation: implementation} },
		NewRemote: func(client connect.HTTPClient, baseURL string, options ...connect.ClientOption) callerAPI {
			return &callerRemoteClient{client: connect.NewClient[wrapperspb.StringValue, wrapperspb.StringValue](client, baseURL+callerProcedure, options...)}
		},
		NewHandler: func(implementation callerAPI, options ...connect.HandlerOption) (string, http.Handler) {
			return callerProcedure, connect.NewUnaryHandlerSimple(callerProcedure, implementation.Call, options...)
		},
	}
}

func testRegistry(t *testing.T, events *[]string, upperResult **upperImpl, callerResult **callerImpl) *Registry {
	t.Helper()
	registry := NewRegistry()
	if err := registry.Register(Registration{
		Service: upperService().Descriptor(),
		New: func() any {
			implementation := &upperImpl{events: events}
			*upperResult = implementation
			return implementation
		},
		Inject: func(target any, injector Injector) error {
			prefix, err := ResolveResource[*string](injector)
			if err != nil {
				return err
			}
			SetResource(&target.(*upperImpl).prefix, prefix)
			return nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(Registration{
		Service: callerService().Descriptor(),
		New: func() any {
			implementation := &callerImpl{events: events}
			*callerResult = implementation
			return implementation
		},
		Inject: func(target any, injector Injector) error {
			upper, err := ResolveComponent[upperAPI](injector, upperServiceName)
			if err != nil {
				return err
			}
			second, err := ResolveComponent[upperAPI](injector, upperServiceName)
			if err != nil {
				return err
			}
			implementation := target.(*callerImpl)
			SetRef(&implementation.upper, upper)
			SetRef(&implementation.second, second)
			return nil
		},
		Dependencies: []string{upperServiceName},
	}); err != nil {
		t.Fatal(err)
	}
	return registry
}

func configRegistry(t *testing.T, result **configurableUpper, factoryCalled *bool) *Registry {
	t.Helper()
	registry := NewRegistry()
	if err := registry.Register(Registration{
		Service:    upperService().Descriptor(),
		ConfigType: reflect.TypeFor[componentSettings](),
		New: func() any {
			*factoryCalled = true
			implementation := new(configurableUpper)
			*result = implementation
			return implementation
		},
		Inject: func(target any, injector Injector) error {
			config, err := ResolveConfig[componentSettings](injector)
			if err != nil {
				return err
			}
			SetConfig(&target.(*configurableUpper).WithConfig, config)
			return nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	return registry
}

func TestComponentConfigInjection(t *testing.T) {
	t.Run("configured before init", func(t *testing.T) {
		config, err := ParseConfig([]byte("units:\n  app: ''\nplacements:\n  weaver.test.v1.UpperService: app\nweaver.test.v1.UpperService:\n  prefix: 'configured:'\n"))
		if err != nil {
			t.Fatal(err)
		}
		var implementation *configurableUpper
		factoryCalled := false
		registry := configRegistry(t, &implementation, &factoryCalled)
		runtime, err := New(context.Background(), "app", config, WithRegistry(registry))
		if err != nil {
			t.Fatal(err)
		}
		defer runtime.Shutdown(context.Background())
		if !factoryCalled || implementation.initPrefix != "configured:" || implementation.Config().Prefix != "configured:" {
			t.Fatalf("unexpected config state: called=%v init=%q config=%q", factoryCalled, implementation.initPrefix, implementation.Config().Prefix)
		}
	})

	t.Run("missing section uses zero value", func(t *testing.T) {
		config, err := ParseConfig([]byte("units:\n  app: ''\nplacements:\n  weaver.test.v1.UpperService: app\n"))
		if err != nil {
			t.Fatal(err)
		}
		var implementation *configurableUpper
		factoryCalled := false
		registry := configRegistry(t, &implementation, &factoryCalled)
		runtime, err := New(context.Background(), "app", config, WithRegistry(registry))
		if err != nil {
			t.Fatal(err)
		}
		defer runtime.Shutdown(context.Background())
		if implementation.Config().Prefix != "" || implementation.initPrefix != "" {
			t.Fatalf("expected zero config, got %#v", implementation.Config())
		}
	})

	t.Run("empty section uses zero value", func(t *testing.T) {
		config, err := ParseConfig([]byte("units:\n  app: ''\nplacements:\n  weaver.test.v1.UpperService: app\nweaver.test.v1.UpperService: {}\n"))
		if err != nil {
			t.Fatal(err)
		}
		var implementation *configurableUpper
		factoryCalled := false
		registry := configRegistry(t, &implementation, &factoryCalled)
		runtime, err := New(context.Background(), "app", config, WithRegistry(registry))
		if err != nil {
			t.Fatal(err)
		}
		defer runtime.Shutdown(context.Background())
		if implementation.Config().Prefix != "" {
			t.Fatalf("expected zero config, got %#v", implementation.Config())
		}
	})

	t.Run("access before injection panics", func(t *testing.T) {
		defer func() {
			if recover() == nil {
				t.Fatal("expected panic")
			}
		}()
		var config WithConfig[componentSettings]
		config.Config()
	})
}

func TestComponentConfigValidation(t *testing.T) {
	if _, err := ParseConfig([]byte("units:\n  app: ''\nplacements:\n  weaver.test.v1.UpperService: app\nunknown.Service:\n  value: true\n")); err == nil || !strings.Contains(err.Error(), "未出现在 placements") {
		t.Fatalf("expected unknown section error, got %v", err)
	}

	tests := []struct {
		name    string
		section string
		want    string
	}{
		{name: "unknown field", section: "  unknown: value\n", want: "field unknown not found"},
		{name: "type mismatch", section: "  prefix: [invalid]\n", want: "cannot unmarshal"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			data := "units:\n  app: ''\nplacements:\n  weaver.test.v1.UpperService: app\nweaver.test.v1.UpperService:\n" + test.section
			config, err := ParseConfig([]byte(data))
			if err != nil {
				t.Fatal(err)
			}
			var implementation *configurableUpper
			factoryCalled := false
			registry := configRegistry(t, &implementation, &factoryCalled)
			_, err = New(context.Background(), "app", config, WithRegistry(registry))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("expected %q, got %v", test.want, err)
			}
			if factoryCalled {
				t.Fatal("component factory ran before config validation")
			}
		})
	}

	t.Run("section without declaration", func(t *testing.T) {
		config, err := ParseConfig([]byte("units:\n  app: ''\nplacements:\n  weaver.test.v1.UpperService: app\nweaver.test.v1.UpperService:\n  prefix: value\n"))
		if err != nil {
			t.Fatal(err)
		}
		factoryCalled := false
		registry := NewRegistry()
		if err := registry.Register(Registration{
			Service: upperService().Descriptor(),
			New: func() any {
				factoryCalled = true
				return &upperImpl{}
			},
			Inject: func(any, Injector) error { return nil },
		}); err != nil {
			t.Fatal(err)
		}
		_, err = New(context.Background(), "app", config, WithRegistry(registry))
		if err == nil || !strings.Contains(err.Error(), "未声明 WithConfig") {
			t.Fatalf("expected missing declaration error, got %v", err)
		}
		if factoryCalled {
			t.Fatal("component factory ran before config validation")
		}
	})

	t.Run("remote section is validated", func(t *testing.T) {
		config, err := ParseConfig([]byte("units:\n  core: http://core.invalid\n  game: ''\nplacements:\n  weaver.test.v1.UpperService: core\n  weaver.test.v1.CallerService: game\nweaver.test.v1.UpperService:\n  unknown: value\n"))
		if err != nil {
			t.Fatal(err)
		}
		var implementation *configurableUpper
		factoryCalled := false
		registry := configRegistry(t, &implementation, &factoryCalled)
		if err := registry.Register(Registration{
			Service: callerService().Descriptor(),
			New: func() any {
				factoryCalled = true
				return &callerImpl{}
			},
			Inject: func(any, Injector) error { return nil },
		}); err != nil {
			t.Fatal(err)
		}
		_, err = New(context.Background(), "game", config, WithRegistry(registry))
		if err == nil || !strings.Contains(err.Error(), "field unknown not found") {
			t.Fatalf("expected remote config validation error, got %v", err)
		}
		if factoryCalled {
			t.Fatal("component factory ran before remote config validation")
		}
	})
}

func TestRuntimeDirectAndRemote(t *testing.T) {
	prefix := "prefix:"
	t.Run("direct", func(t *testing.T) {
		var events []string
		var upper *upperImpl
		var caller *callerImpl
		registry := testRegistry(t, &events, &upper, &caller)
		config := Config{
			Units: map[string]string{"app": ""},
			Placements: map[string]string{
				upperServiceName:  "app",
				callerServiceName: "app",
			},
		}
		runtime, err := New(context.Background(), "app", config, WithRegistry(registry), WithResource(&prefix))
		if err != nil {
			t.Fatal(err)
		}
		request := wrapperspb.String("hello")
		response, err := caller.Call(context.Background(), request)
		if err != nil {
			t.Fatal(err)
		}
		if response.Value != "prefix:HELLO" || request.Value != "hello" {
			t.Fatalf("unexpected response=%q request=%q", response.Value, request.Value)
		}
		_, err = caller.Call(context.Background(), wrapperspb.String("error"))
		if connect.CodeOf(err) != connect.CodeUnknown {
			t.Fatalf("expected unknown, got %v", err)
		}
		if !reflect.DeepEqual(events, []string{"upper:init", "caller:init"}) {
			t.Fatalf("unexpected init order: %v", events)
		}
		if err := runtime.Shutdown(context.Background()); err != nil {
			t.Fatal(err)
		}
		if err := runtime.Shutdown(context.Background()); err != nil {
			t.Fatal(err)
		}
		want := []string{"upper:init", "caller:init", "caller:shutdown", "upper:shutdown"}
		if !reflect.DeepEqual(events, want) {
			t.Fatalf("unexpected lifecycle: %v", events)
		}
	})

	t.Run("remote", func(t *testing.T) {
		var coreEvents []string
		var coreUpper *upperImpl
		var unusedCaller *callerImpl
		coreRegistry := testRegistry(t, &coreEvents, &coreUpper, &unusedCaller)
		coreConfig := Config{
			Units: map[string]string{"core": "", "game": "http://unused.invalid"},
			Placements: map[string]string{
				upperServiceName:  "core",
				callerServiceName: "game",
			},
		}
		coreRuntime, err := New(context.Background(), "core", coreConfig, WithRegistry(coreRegistry), WithResource(&prefix))
		if err != nil {
			t.Fatal(err)
		}
		server := httptest.NewServer(coreRuntime.Handler())
		defer server.Close()

		var gameEvents []string
		var unusedUpper *upperImpl
		var gameCaller *callerImpl
		gameRegistry := testRegistry(t, &gameEvents, &unusedUpper, &gameCaller)
		gameConfig := Config{
			Units: map[string]string{"core": server.URL, "game": ""},
			Placements: map[string]string{
				upperServiceName:  "core",
				callerServiceName: "game",
			},
		}
		gameRuntime, err := New(context.Background(), "game", gameConfig, WithRegistry(gameRegistry))
		if err != nil {
			t.Fatal(err)
		}
		request := wrapperspb.String("hello")
		response, err := gameCaller.Call(context.Background(), request)
		if err != nil {
			t.Fatal(err)
		}
		if response.Value != "prefix:HELLO" || request.Value != "hello" {
			t.Fatalf("unexpected response=%q request=%q", response.Value, request.Value)
		}
		_, err = gameCaller.Call(context.Background(), wrapperspb.String("error"))
		if connect.CodeOf(err) != connect.CodeUnknown {
			t.Fatalf("expected unknown, got %v", err)
		}
		if err := gameRuntime.Shutdown(context.Background()); err != nil {
			t.Fatal(err)
		}
		if err := coreRuntime.Shutdown(context.Background()); err != nil {
			t.Fatal(err)
		}
	})
}

func TestCancellationCodesMatch(t *testing.T) {
	prefix := "x:"
	assertCodes := func(t *testing.T, caller callerAPI) {
		t.Helper()
		canceled, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := caller.Call(canceled, wrapperspb.String("value")); connect.CodeOf(err) != connect.CodeCanceled {
			t.Fatalf("expected canceled, got %v", err)
		}

		deadline, cancelDeadline := context.WithDeadline(context.Background(), time.Unix(1, 0))
		defer cancelDeadline()
		if _, err := caller.Call(deadline, wrapperspb.String("value")); connect.CodeOf(err) != connect.CodeDeadlineExceeded {
			t.Fatalf("expected deadline_exceeded, got %v", err)
		}
	}

	t.Run("direct", func(t *testing.T) {
		var events []string
		var upper *upperImpl
		var caller *callerImpl
		registry := testRegistry(t, &events, &upper, &caller)
		config := Config{
			Units:      map[string]string{"app": ""},
			Placements: map[string]string{upperServiceName: "app", callerServiceName: "app"},
		}
		runtime, err := New(context.Background(), "app", config, WithRegistry(registry), WithResource(&prefix))
		if err != nil {
			t.Fatal(err)
		}
		defer runtime.Shutdown(context.Background())
		assertCodes(t, caller)
	})

	t.Run("remote", func(t *testing.T) {
		var coreEvents []string
		var coreUpper *upperImpl
		var unusedCaller *callerImpl
		coreRegistry := testRegistry(t, &coreEvents, &coreUpper, &unusedCaller)
		coreConfig := Config{
			Units:      map[string]string{"core": "", "game": "http://unused.invalid"},
			Placements: map[string]string{upperServiceName: "core", callerServiceName: "game"},
		}
		coreRuntime, err := New(context.Background(), "core", coreConfig, WithRegistry(coreRegistry), WithResource(&prefix))
		if err != nil {
			t.Fatal(err)
		}
		defer coreRuntime.Shutdown(context.Background())
		server := httptest.NewServer(coreRuntime.Handler())
		defer server.Close()

		var gameEvents []string
		var unusedUpper *upperImpl
		var caller *callerImpl
		gameRegistry := testRegistry(t, &gameEvents, &unusedUpper, &caller)
		gameConfig := Config{
			Units:      map[string]string{"core": server.URL, "game": ""},
			Placements: map[string]string{upperServiceName: "core", callerServiceName: "game"},
		}
		gameRuntime, err := New(context.Background(), "game", gameConfig, WithRegistry(gameRegistry))
		if err != nil {
			t.Fatal(err)
		}
		defer gameRuntime.Shutdown(context.Background())
		assertCodes(t, caller)
	})
}

func TestInitFailureRollsBack(t *testing.T) {
	prefix := "x:"
	var events []string
	var upper *upperImpl
	var caller *callerImpl
	registry := testRegistry(t, &events, &upper, &caller)
	registry.mu.Lock()
	registration := registry.registrations[callerServiceName]
	registration.New = func() any {
		implementation := &callerImpl{events: &events, initErr: errors.New("init failed")}
		caller = implementation
		return implementation
	}
	registry.registrations[callerServiceName] = registration
	registry.mu.Unlock()

	config := Config{
		Units:      map[string]string{"app": ""},
		Placements: map[string]string{upperServiceName: "app", callerServiceName: "app"},
	}
	_, err := New(context.Background(), "app", config, WithRegistry(registry), WithResource(&prefix))
	if err == nil || !strings.Contains(err.Error(), "init failed") {
		t.Fatalf("expected init failure, got %v", err)
	}
	want := []string{"upper:init", "caller:init", "upper:shutdown"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("unexpected rollback order: %v", events)
	}
}

func TestResourceAndRegistrationValidation(t *testing.T) {
	newRuntime := func(t *testing.T, options ...Option) error {
		t.Helper()
		var events []string
		var upper *upperImpl
		var caller *callerImpl
		registry := testRegistry(t, &events, &upper, &caller)
		config := Config{
			Units:      map[string]string{"app": ""},
			Placements: map[string]string{upperServiceName: "app", callerServiceName: "app"},
		}
		values := append([]Option{WithRegistry(registry)}, options...)
		_, err := New(context.Background(), "app", config, values...)
		return err
	}

	if err := newRuntime(t); err == nil || !strings.Contains(err.Error(), "缺少资源") {
		t.Fatalf("expected missing resource, got %v", err)
	}
	first, second := "a", "b"
	if err := newRuntime(t, WithResource(&first), WithResource(&second)); err == nil || !strings.Contains(err.Error(), "重复注册") {
		t.Fatalf("expected duplicate resource, got %v", err)
	}
	var nilResource *string
	if err := newRuntime(t, WithResource(nilResource)); err == nil || !strings.Contains(err.Error(), "不能为 nil") {
		t.Fatalf("expected nil resource, got %v", err)
	}

	registry := NewRegistry()
	registration := Registration{
		Service: upperService().Descriptor(),
		New:     func() any { return &upperImpl{} },
		Inject:  func(any, Injector) error { return nil },
	}
	if err := registry.Register(registration); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(registration); err == nil || !strings.Contains(err.Error(), "重复注册") {
		t.Fatalf("expected duplicate implementation, got %v", err)
	}

	invalidConfigRegistry := NewRegistry()
	registration.ConfigType = reflect.TypeFor[string]()
	if err := invalidConfigRegistry.Register(registration); err == nil || !strings.Contains(err.Error(), "配置类型必须是结构体") {
		t.Fatalf("expected invalid config type, got %v", err)
	}
}

type countingResolver struct {
	mu     sync.Mutex
	calls  int
	target ResolvedTarget
}

func (r *countingResolver) Resolve(context.Context, string) (ResolvedTarget, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	return r.target, nil
}

func TestResolverCalledOncePerUnit(t *testing.T) {
	prefix := "x:"
	var coreEvents []string
	var coreUpper *upperImpl
	var unusedCaller *callerImpl
	coreRegistry := testRegistry(t, &coreEvents, &coreUpper, &unusedCaller)
	coreConfig := Config{
		Units:      map[string]string{"core": "", "game": "http://unused.invalid"},
		Placements: map[string]string{upperServiceName: "core", callerServiceName: "game"},
	}
	coreRuntime, err := New(context.Background(), "core", coreConfig, WithRegistry(coreRegistry), WithResource(&prefix))
	if err != nil {
		t.Fatal(err)
	}
	defer coreRuntime.Shutdown(context.Background())
	server := httptest.NewServer(coreRuntime.Handler())
	defer server.Close()

	resolver := &countingResolver{target: ResolvedTarget{BaseURL: server.URL, HTTPClient: http.DefaultClient}}
	var events []string
	var unusedUpper *upperImpl
	var caller *callerImpl
	registry := testRegistry(t, &events, &unusedUpper, &caller)
	config := Config{
		Units:      map[string]string{"core": "fake://core", "game": ""},
		Placements: map[string]string{upperServiceName: "core", callerServiceName: "game"},
	}
	runtime, err := New(context.Background(), "game", config, WithRegistry(registry), WithResolver("fake", resolver))
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Shutdown(context.Background())
	if _, err := caller.Call(context.Background(), wrapperspb.String("ok")); err != nil {
		t.Fatal(err)
	}
	if resolver.calls != 1 {
		t.Fatalf("resolver called %d times", resolver.calls)
	}
}

func TestRuntimeValidation(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Registry, *Config)
		want   string
	}{
		{
			name: "missing placement",
			mutate: func(_ *Registry, config *Config) {
				delete(config.Placements, callerServiceName)
			},
			want: "缺少 placement",
		},
		{
			name: "unknown placement",
			mutate: func(_ *Registry, config *Config) {
				config.Placements["unknown.Service"] = "app"
			},
			want: "未注册组件",
		},
		{
			name: "cycle",
			mutate: func(registry *Registry, _ *Config) {
				registry.mu.Lock()
				registration := registry.registrations[upperServiceName]
				registration.Dependencies = []string{callerServiceName}
				registry.registrations[upperServiceName] = registration
				registry.mu.Unlock()
			},
			want: "依赖存在循环",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var events []string
			var upper *upperImpl
			var caller *callerImpl
			registry := testRegistry(t, &events, &upper, &caller)
			config := Config{
				Units:      map[string]string{"app": ""},
				Placements: map[string]string{upperServiceName: "app", callerServiceName: "app"},
			}
			test.mutate(registry, &config)
			_, err := New(context.Background(), "app", config, WithRegistry(registry), WithResource(new(string)))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("expected %q, got %v", test.want, err)
			}
		})
	}
}

func TestParseConfigStrict(t *testing.T) {
	config, err := ParseConfig([]byte("units:\n  app: ''\nplacements:\n  weaver.test.v1.UpperService: app\n"))
	if err != nil {
		t.Fatal(err)
	}
	if config.Placements[upperServiceName] != "app" {
		t.Fatalf("unexpected config: %#v", config)
	}
	if _, err := ParseConfig([]byte("unknown: true\n")); err == nil {
		t.Fatal("expected unknown field error")
	}
}
