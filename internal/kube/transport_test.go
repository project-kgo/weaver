package kube

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
)

func TestBackendClientUsesH2CAndRoundRobin(t *testing.T) {
	var firstCalls atomic.Int64
	first := startH2CServer(t, &firstCalls)
	defer first.Close()
	var secondCalls atomic.Int64
	second := startH2CServer(t, &secondCalls)
	defer second.Close()

	client := newBackendClient(target{
		namespace: "production",
		service:   "game",
		port:      "connect",
	})
	client.update([]string{first.Listener.Addr().String(), second.Listener.Addr().String()})
	defer client.close()

	for range 4 {
		request, err := http.NewRequest(http.MethodPost, "http://game.production.svc/test", nil)
		if err != nil {
			t.Fatal(err)
		}
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
	}
	if firstCalls.Load() != 2 || secondCalls.Load() != 2 {
		t.Fatalf("round-robin calls = %d/%d, want 2/2", firstCalls.Load(), secondCalls.Load())
	}

	client.update([]string{second.Listener.Addr().String()})
	for range 2 {
		request, _ := http.NewRequest(http.MethodPost, "http://game.production.svc/test", nil)
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		_ = response.Body.Close()
	}
	if firstCalls.Load() != 2 || secondCalls.Load() != 4 {
		t.Fatalf("移除 endpoint 后 calls = %d/%d, want 2/4", firstCalls.Load(), secondCalls.Load())
	}
}

func TestBackendSelectionDuringEndpointUpdatesNeverFailsTransiently(t *testing.T) {
	client := newBackendClient(target{namespace: "production", service: "game"})
	many := make([]string, 128)
	for index := range many {
		many[index] = fmt.Sprintf("10.0.0.%d:8080", index+1)
	}
	retained := []string{many[len(many)-1]}
	client.update(many)
	defer client.close()

	var wait sync.WaitGroup
	errors := make(chan error, 8)
	for range 8 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for range 20_000 {
				if _, err := client.selectEndpoint(); err != nil {
					errors <- err
					return
				}
			}
		}()
	}
	for range 1_000 {
		client.update(retained)
		client.update(many)
	}
	wait.Wait()
	close(errors)
	for err := range errors {
		t.Fatalf("端点更新期间选择失败: %v", err)
	}
}

func startH2CServer(t *testing.T, calls *atomic.Int64) *httptest.Server {
	t.Helper()
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.ProtoMajor != 2 {
			t.Errorf("协议为 %s, want HTTP/2", request.Proto)
		}
		if request.Host != "game.production.svc" {
			t.Errorf("Host = %q, want game.production.svc", request.Host)
		}
		calls.Add(1)
		response.WriteHeader(http.StatusNoContent)
	}))
	protocols := new(http.Protocols)
	protocols.SetHTTP2(true)
	protocols.SetUnencryptedHTTP2(true)
	server.Config.Protocols = protocols
	server.Start()
	return server
}
