//go:build unix

package weaver

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/wrapperspb"
)

const serveSignalHelperEnv = "WEAVER_SERVE_SIGNAL_HELPER"

type signalTestUpper struct{}

func (*signalTestUpper) Init(context.Context) error {
	_, err := fmt.Fprintln(os.Stdout, "ready")
	return err
}

func (*signalTestUpper) Upper(context.Context, *wrapperspb.StringValue) (*wrapperspb.StringValue, error) {
	return wrapperspb.String("ok"), nil
}

func TestServeStopsOnInterruptSignal(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestServeSignalHelper$")
	command.Env = append(os.Environ(), serveSignalHelperEnv+"=1")
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}

	scanner := bufio.NewScanner(stdout)
	if !scanner.Scan() || scanner.Text() != "ready" {
		_ = command.Wait()
		t.Fatalf("helper 未就绪: stdout=%q stderr=%q", scanner.Text(), stderr.String())
	}
	if err := command.Process.Signal(os.Interrupt); err != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		t.Fatal(err)
	}

	lines := []string{"ready"}
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	if err := command.Wait(); err != nil {
		t.Fatalf("helper 退出失败: %v, stdout=%q stderr=%q", err, lines, stderr.String())
	}
	if !slices.Contains(lines, "hook:shutdown") {
		t.Fatalf("退出信号没有执行 shutdown hook: stdout=%q", lines)
	}
}

func TestServeSignalHelper(t *testing.T) {
	if os.Getenv(serveSignalHelperEnv) != "1" {
		return
	}
	registry := NewRegistry()
	if err := registry.Register(Registration{
		Service: upperService().Descriptor(),
		New:     func() any { return new(signalTestUpper) },
		Inject:  func(any, Injector) error { return nil },
	}); err != nil {
		t.Fatal(err)
	}
	config := Config{
		Units:      map[string]string{"app": ""},
		Placements: map[string]string{upperServiceName: "app"},
	}
	err := Serve(
		context.Background(),
		"app",
		config,
		"127.0.0.1:0",
		WithRegistry(registry),
		WithShutdownHook(func(context.Context) error {
			_, err := fmt.Fprintln(os.Stdout, "hook:shutdown")
			return err
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
}
