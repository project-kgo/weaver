package weaver

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestServeShutdownOnContextCancellation(t *testing.T) {
	events := make([]string, 0, 4)
	var upper *upperImpl
	var caller *callerImpl
	registry := testRegistry(t, &events, &upper, &caller)
	prefix := "test:"
	config := Config{
		Units: map[string]string{"app": ""},
		Placements: map[string]string{
			upperServiceName:  "app",
			callerServiceName: "app",
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := Serve(
		ctx,
		"app",
		config,
		"127.0.0.1:0",
		WithRegistry(registry),
		WithResource(&prefix),
		WithShutdownHook(func(context.Context) error {
			events = append(events, "hook:shutdown")
			return nil
		}),
	)
	if err != nil {
		t.Fatalf("Serve() error = %v", err)
	}

	want := []string{"upper:init", "caller:init", "caller:shutdown", "upper:shutdown", "hook:shutdown"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("lifecycle events = %v, want %v", events, want)
	}
}

func TestServeShutdownOnListenFailure(t *testing.T) {
	events := make([]string, 0, 4)
	var upper *upperImpl
	var caller *callerImpl
	registry := testRegistry(t, &events, &upper, &caller)
	prefix := "test:"
	config := Config{
		Units: map[string]string{"app": ""},
		Placements: map[string]string{
			upperServiceName:  "app",
			callerServiceName: "app",
		},
	}

	err := Serve(
		context.Background(),
		"app",
		config,
		"invalid-address",
		WithRegistry(registry),
		WithResource(&prefix),
	)
	if err == nil {
		t.Fatal("Serve() error = nil, want listen error")
	}

	want := []string{"upper:init", "caller:init", "caller:shutdown", "upper:shutdown"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("lifecycle events = %v, want %v", events, want)
	}
}

func TestShutdownHooksRunInReverseOrderAndOnlyOnce(t *testing.T) {
	events := make([]string, 0, 6)
	var upper *upperImpl
	var caller *callerImpl
	registry := testRegistry(t, &events, &upper, &caller)
	prefix := "test:"
	config := Config{
		Units: map[string]string{"app": ""},
		Placements: map[string]string{
			upperServiceName:  "app",
			callerServiceName: "app",
		},
	}
	hookError := errors.New("hook failed")
	runtime, err := New(
		context.Background(),
		"app",
		config,
		WithRegistry(registry),
		WithResource(&prefix),
		WithShutdownHook(func(context.Context) error {
			events = append(events, "hook:first")
			return hookError
		}),
		WithShutdownHook(func(context.Context) error {
			events = append(events, "hook:second")
			return nil
		}),
	)
	if err != nil {
		t.Fatal(err)
	}

	err = runtime.Shutdown(context.Background())
	if !errors.Is(err, hookError) {
		t.Fatalf("Shutdown() error = %v, want %v", err, hookError)
	}
	if err := runtime.Shutdown(context.Background()); err != nil {
		t.Fatalf("second Shutdown() error = %v", err)
	}

	want := []string{
		"upper:init",
		"caller:init",
		"caller:shutdown",
		"upper:shutdown",
		"hook:second",
		"hook:first",
	}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("lifecycle events = %v, want %v", events, want)
	}
}

func TestWithShutdownHookRejectsNil(t *testing.T) {
	_, err := New(context.Background(), "app", Config{}, WithShutdownHook(nil))
	if err == nil {
		t.Fatal("New() error = nil, want nil hook error")
	}
}
