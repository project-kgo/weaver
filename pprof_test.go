package weaver

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestPprofEndpoints(t *testing.T) {
	server := httptest.NewServer(newPprofServer("").Handler)
	defer server.Close()
	for _, path := range []string{
		"/debug/pprof/", "/debug/pprof/heap", "/debug/pprof/goroutine",
		"/debug/pprof/profile?seconds=1", "/debug/pprof/trace?seconds=0.01",
	} {
		t.Run(path, func(t *testing.T) {
			response, err := server.Client().Get(server.URL + path)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			if err != nil || response.StatusCode != http.StatusOK || len(body) == 0 {
				t.Fatalf("pprof 返回 status=%d, bytes=%d, error=%v", response.StatusCode, len(body), err)
			}
		})
	}
	response, err := server.Client().Get(server.URL + "/business")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("调试端口暴露了额外路由: %d", response.StatusCode)
	}
}

func TestServePprofListenFailureAndDisable(t *testing.T) {
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	config, options := pprofTestOptions(t)
	closed := false
	err = Serve(context.Background(), "app", config, "127.0.0.1:0",
		append(options, WithPprofAddress(occupied.Addr().String()),
			WithShutdownHook(func(context.Context) error { closed = true; return nil }),
		)...)
	if err == nil || !strings.Contains(err.Error(), occupied.Addr().String()) || !closed {
		t.Fatalf("端口冲突未返回错误或未清理 Runtime: error=%v, closed=%v", err, closed)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Serve(ctx, "app", config, "127.0.0.1:0",
		append(options, WithPprofAddress(""))...); err != nil {
		t.Fatalf("关闭 pprof 后启动失败: %v", err)
	}
}

func TestServePprofLifecycle(t *testing.T) {
	if got := newRuntimeOptions().pprofAddress; got != "" {
		t.Fatalf("默认 pprof 地址 = %q", got)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	config, options := pprofTestOptions(t)
	go func() {
		done <- Serve(ctx, "app", config, "127.0.0.1:0",
			append(options, WithPprofAddress(address))...)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Serve 退出失败: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("Serve 未按时退出")
		}
		listener, err := net.Listen("tcp", address)
		if err != nil {
			t.Errorf("pprof 端口未释放: %v", err)
		} else {
			listener.Close()
		}
	})
	client := &http.Client{Timeout: time.Second}
	deadline := time.Now().Add(5 * time.Second)
	for {
		response, err := client.Get("http://" + address + "/debug/pprof/heap")
		if err == nil {
			io.Copy(io.Discard, response.Body)
			response.Body.Close()
			if response.StatusCode != http.StatusOK {
				t.Fatalf("heap status = %d", response.StatusCode)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("pprof 未就绪: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestServePprofDoesNotWaitForProfile(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	config, options := pprofTestOptions(t)
	go func() {
		done <- Serve(ctx, "app", config, "127.0.0.1:0",
			append(options, WithPprofAddress(address))...)
	}()
	profileDone := make(chan struct{})
	go func() {
		defer close(profileDone)
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			response, err := http.Get("http://" + address + "/debug/pprof/profile?seconds=30")
			if err == nil {
				io.Copy(io.Discard, response.Body)
				response.Body.Close()
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	// 等待 profile 请求进入处理阶段。
	time.Sleep(500 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve 退出失败: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Serve 等待了进行中的 profile")
	}
	<-profileDone
}

func pprofTestOptions(t *testing.T) (Config, []Option) {
	t.Helper()
	var events []string
	var upper *upperImpl
	var caller *callerImpl
	registry := testRegistry(t, &events, &upper, &caller)
	prefix := "test:"
	return Config{
		Units:      map[string]string{"app": ""},
		Placements: map[string]string{upperServiceName: "app", callerServiceName: "app"},
	}, []Option{WithRegistry(registry), WithResource(&prefix)}
}
