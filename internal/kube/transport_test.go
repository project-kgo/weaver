package kube

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestBackendClientUsesH2C(t *testing.T) {
	var calls atomic.Int64
	server := startH2CServer(t, &calls)
	defer server.Close()

	client, err := newBackendClient(target{
		namespace: "production",
		service:   "game",
		port:      "connect",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.update([]string{server.Listener.Addr().String()}); err != nil {
		t.Fatal(err)
	}
	defer client.close()

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
	if calls.Load() != 1 {
		t.Fatalf("calls = %d, want 1", calls.Load())
	}
}

func TestEndpointClientMarksDialErrors(t *testing.T) {
	client := newEndpointClient(
		target{namespace: "production", service: "game"},
		"127.0.0.1:not-a-port",
	)
	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport = %T, want *http.Transport", client.Transport)
	}
	_, err := transport.DialContext(context.Background(), "tcp", "ignored")
	if !isEndpointDialError(err) {
		t.Fatalf("error = %v, want endpointDialError", err)
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
