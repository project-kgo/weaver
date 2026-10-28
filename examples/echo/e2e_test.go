package echo_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/project-kgo/weaver"
	examplev1 "github.com/project-kgo/weaver/examples/echo/gen/example/v1"
	examplev1connect "github.com/project-kgo/weaver/examples/echo/gen/example/v1/examplev1connect"
	_ "github.com/project-kgo/weaver/examples/echo/internal/app"
)

func TestGeneratedCodeEndToEnd(t *testing.T) {
	t.Run("monolith", func(t *testing.T) {
		config := loadConfig(t, "monolith.yaml")
		server := startUnit(t, "app", config)
		client := examplev1connect.NewEchoServiceClient(server.Client(), server.URL)
		assertEchoBehavior(t, client)
		assertCallbackBehavior(t, server)
	})

	t.Run("two units", func(t *testing.T) {
		config := loadConfig(t, "microservices.yaml")
		coreServer := startUnit(t, "core", config)

		// game 启动前替换 core 地址，使 Echo -> Upper 经过真实 ConnectRPC。
		config.Units["core"] = coreServer.URL
		gameServer := startUnit(t, "game", config)
		client := examplev1connect.NewEchoServiceClient(gameServer.Client(), gameServer.URL)
		assertEchoBehavior(t, client)
		// 回调组件放在 game，经 Ref 远程调用 core 上的 UpperService；core 不挂载回调路由。
		assertCallbackBehavior(t, gameServer)
		if status, _ := postCallback(t, coreServer, "/callbacks/upper/hello", "secret"); status != http.StatusNotFound {
			t.Fatalf("expected callback route absent on core, got %d", status)
		}
	})
}

func assertCallbackBehavior(t *testing.T, server *httptest.Server) {
	t.Helper()
	status, body := postCallback(t, server, "/callbacks/upper/hello", "secret")
	if status != http.StatusOK || body != "echo:HELLO" {
		t.Fatalf("unexpected callback response: %d %q", status, body)
	}
	if status, _ := postCallback(t, server, "/callbacks/upper/hello", "wrong"); status != http.StatusUnauthorized {
		t.Fatalf("expected unauthorized, got %d", status)
	}
	if status, _ := postCallback(t, server, "/callbacks/upper/error", "secret"); status != http.StatusBadGateway {
		t.Fatalf("expected bad gateway, got %d", status)
	}
}

func postCallback(t *testing.T, server *httptest.Server, path, token string) (int, string) {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, server.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("X-Callback-Token", token)
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, strings.TrimSpace(string(body))
}

func TestUnitHandlerSupportsConnectAndGRPC(t *testing.T) {
	config := loadConfig(t, "monolith.yaml")
	var mu sync.Mutex
	var requests []observedHTTPRequest
	server := startUnitServer(t, "app", config, false, func(request *http.Request) {
		mu.Lock()
		requests = append(requests, observedHTTPRequest{
			httpMajor:   request.ProtoMajor,
			contentType: request.Header.Get("Content-Type"),
		})
		mu.Unlock()
	})

	connectClient := examplev1connect.NewEchoServiceClient(server.Client(), server.URL)
	assertEchoBehavior(t, connectClient)
	grpcClient := examplev1connect.NewEchoServiceClient(newH2CClient(t), server.URL, connect.WithGRPC())
	assertEchoBehavior(t, grpcClient)

	mu.Lock()
	defer mu.Unlock()
	if !hasObservedRequest(requests, 1, "application/grpc", false) {
		t.Fatalf("expected Connect request over HTTP/1, got %#v", requests)
	}
	if !hasObservedRequest(requests, 2, "application/grpc", true) {
		t.Fatalf("expected gRPC request over HTTP/2, got %#v", requests)
	}
}

func TestUnitHandlerSupportsGRPCOverTLS(t *testing.T) {
	config := loadConfig(t, "monolith.yaml")
	var mu sync.Mutex
	var requests []observedHTTPRequest
	server := startUnitServer(t, "app", config, true, func(request *http.Request) {
		mu.Lock()
		requests = append(requests, observedHTTPRequest{
			httpMajor:   request.ProtoMajor,
			contentType: request.Header.Get("Content-Type"),
		})
		mu.Unlock()
	})

	client := examplev1connect.NewEchoServiceClient(server.Client(), server.URL, connect.WithGRPC())
	assertEchoBehavior(t, client)

	mu.Lock()
	defer mu.Unlock()
	if !hasObservedRequest(requests, 2, "application/grpc", true) {
		t.Fatalf("expected gRPC request over TLS HTTP/2, got %#v", requests)
	}
}

type observedHTTPRequest struct {
	httpMajor   int
	contentType string
}

func hasObservedRequest(requests []observedHTTPRequest, httpMajor int, contentTypePrefix string, matchesPrefix bool) bool {
	for _, request := range requests {
		if request.httpMajor == httpMajor && strings.HasPrefix(request.contentType, contentTypePrefix) == matchesPrefix {
			return true
		}
	}
	return false
}

func loadConfig(t *testing.T, name string) weaver.Config {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("config", name))
	if err != nil {
		t.Fatal(err)
	}
	config, err := weaver.ParseConfig(data)
	if err != nil {
		t.Fatal(err)
	}
	return config
}

func startUnit(t *testing.T, unit string, config weaver.Config, options ...weaver.Option) *httptest.Server {
	return startUnitServer(t, unit, config, false, nil, options...)
}

func startUnitServer(t *testing.T, unit string, config weaver.Config, useTLS bool, observe func(*http.Request), options ...weaver.Option) *httptest.Server {
	t.Helper()
	runtime, err := weaver.New(context.Background(), unit, config, options...)
	if err != nil {
		t.Fatal(err)
	}
	handler := runtime.Handler()
	if observe != nil {
		handler = http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
			observe(request)
			runtime.Handler().ServeHTTP(response, request)
		})
	}
	server := httptest.NewUnstartedServer(handler)
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	protocols.SetHTTP2(true)
	protocols.SetUnencryptedHTTP2(true)
	server.Config.Protocols = protocols
	if useTLS {
		server.EnableHTTP2 = true
		server.StartTLS()
	} else {
		server.Start()
	}
	t.Cleanup(func() {
		server.Close()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := runtime.Shutdown(ctx); err != nil {
			t.Errorf("shutdown %s: %v", unit, err)
		}
	})
	return server
}

func newH2CClient(t *testing.T) *http.Client {
	t.Helper()
	protocols := new(http.Protocols)
	protocols.SetUnencryptedHTTP2(true)
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Protocols = protocols
	t.Cleanup(transport.CloseIdleConnections)
	return &http.Client{Transport: transport}
}

func assertEchoBehavior(t *testing.T, client examplev1connect.EchoServiceClient) {
	t.Helper()
	request := &examplev1.EchoRequest{Value: "hello"}
	response, err := client.Echo(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if response.GetValue() != "echo:HELLO" {
		t.Fatalf("unexpected response: %q", response.GetValue())
	}
	if request.GetValue() != "hello" {
		t.Fatalf("request was modified: %q", request.GetValue())
	}

	if _, err := client.Echo(context.Background(), &examplev1.EchoRequest{Value: "error"}); connect.CodeOf(err) != connect.CodeUnknown {
		t.Fatalf("expected unknown, got %v", err)
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := client.Echo(canceled, &examplev1.EchoRequest{Value: "hello"}); connect.CodeOf(err) != connect.CodeCanceled {
		t.Fatalf("expected canceled, got %v", err)
	}

	deadline, cancelDeadline := context.WithDeadline(context.Background(), time.Unix(1, 0))
	defer cancelDeadline()
	if _, err := client.Echo(deadline, &examplev1.EchoRequest{Value: "hello"}); connect.CodeOf(err) != connect.CodeDeadlineExceeded {
		t.Fatalf("expected deadline_exceeded, got %v", err)
	}
}
