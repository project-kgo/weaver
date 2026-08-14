package kube

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/project-kgo/weaver/balancer"
)

func TestAPITransportPrefersHTTP2AndAllowsHTTP1Fallback(t *testing.T) {
	roots := x509.NewCertPool()
	transport := newAPITransport(roots)
	if transport.MaxConnsPerHost != 0 {
		t.Fatalf("MaxConnsPerHost = %d, want 0", transport.MaxConnsPerHost)
	}
	if !transport.ForceAttemptHTTP2 || transport.Protocols == nil || !transport.Protocols.HTTP1() || !transport.Protocols.HTTP2() {
		t.Fatal("Kubernetes API Transport 未同时启用 HTTP/2 和 HTTP/1 fallback")
	}
	if transport.TLSClientConfig == nil || transport.TLSClientConfig.RootCAs != roots {
		t.Fatal("Kubernetes API Transport 未正确配置 HTTP/2 或 CA")
	}
}

func TestAPITransportNegotiatesHTTP2(t *testing.T) {
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.WriteHeader(http.StatusNoContent)
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()

	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	client := &http.Client{Transport: newAPITransport(roots)}
	defer client.CloseIdleConnections()
	response, err := client.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.ProtoMajor != 2 {
		t.Fatalf("Kubernetes API 协议 = %s, want HTTP/2", response.Proto)
	}
}

func TestAggregateEndpointSlices(t *testing.T) {
	resolver := newResolver("http://unused", &http.Client{}, "")
	ready, notReady := true, false
	portName, tcp := "connect", "TCP"
	port := 8080
	first := testSlice("first", "production", "game", "IPv4", []endpoint{
		{Addresses: []string{"10.0.0.2", "10.0.0.1"}, Conditions: endpointConditions{Ready: &ready}},
		{Addresses: []string{"10.0.0.3"}, Conditions: endpointConditions{Ready: &notReady}},
	}, []endpointPort{{Name: &portName, Protocol: &tcp, Port: &port}})
	second := testSlice("second", "production", "game", "IPv4", []endpoint{
		// 重复地址用于验证跨 slice 去重，nil ready 按可用处理。
		{Addresses: []string{"10.0.0.2", "10.0.0.4"}},
	}, []endpointPort{{Name: &portName, Protocol: &tcp, Port: &port}})
	putSlice(resolver.slices, compactSlice(first))
	putSlice(resolver.slices, compactSlice(second))

	value, err := parseTarget("kube://production/game:connect")
	if err != nil {
		t.Fatal(err)
	}
	got := resolver.aggregateLocked(value)
	want := []string{"10.0.0.1:8080", "10.0.0.2:8080", "10.0.0.4:8080"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("aggregate = %v, want %v", got, want)
	}

	numeric, err := parseTarget("kube://production/game:9090")
	if err != nil {
		t.Fatal(err)
	}
	got = resolver.aggregateLocked(numeric)
	want = []string{"10.0.0.1:9090", "10.0.0.2:9090", "10.0.0.4:9090"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("numeric aggregate = %v, want %v", got, want)
	}
}

func TestApplyDeletePublishesEmptySnapshot(t *testing.T) {
	resolver := newResolver("http://unused", &http.Client{}, "")
	portName, tcp := "connect", "TCP"
	port := 8080
	item := testSlice("only", "production", "game", "IPv4", []endpoint{{Addresses: []string{"10.0.0.1"}}}, []endpointPort{{Name: &portName, Protocol: &tcp, Port: &port}})
	putSlice(resolver.slices, compactSlice(item))
	value, err := parseTarget("kube://production/game:connect")
	if err != nil {
		t.Fatal(err)
	}
	client, err := newBackendClient(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.update(resolver.aggregateLocked(value)); err != nil {
		t.Fatal(err)
	}
	resolver.clients[value.key()] = client
	resolver.serviceClients[value.serviceKey()] = map[string]*backendClient{value.key(): client}
	watch := &namespaceWatch{namespace: "production", services: map[string]struct{}{"game": {}}}
	resolver.sliceServices["production/only"] = "production/game"
	if !backendHasEndpoint(client) {
		t.Fatal("初始 endpoint 未发布")
	}

	resolver.applyEvent(watch, "DELETED", item)
	if backendHasEndpoint(client) {
		t.Fatal("删除后仍保留 endpoint")
	}
}

func TestTargetsInOneNamespaceShareFilteredListAndWatch(t *testing.T) {
	transport := &discoveryTransport{watchStarted: make(chan struct{}, 2)}
	resolver := newResolver("https://kubernetes.test", &http.Client{Transport: transport}, "")
	t.Cleanup(func() { _ = resolver.Shutdown(context.Background()) })

	if _, _, err := resolver.Resolve(context.Background(), "kube://production/game:connect"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := resolver.Resolve(context.Background(), "kube://production/wallet:connect"); err != nil {
		t.Fatal(err)
	}
	if err := resolver.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-transport.watchStarted:
	case <-time.After(time.Second):
		t.Fatal("watch 未启动")
	}

	transport.mu.Lock()
	defer transport.mu.Unlock()
	if transport.listCalls != 1 || transport.watchCalls != 1 || transport.maxActiveWatches != 1 {
		t.Fatalf("LIST=%d WATCH=%d maxActive=%d", transport.listCalls, transport.watchCalls, transport.maxActiveWatches)
	}
	if len(transport.requests) != 2 {
		t.Fatalf("请求数 = %d, want 2", len(transport.requests))
	}
	for _, request := range transport.requests {
		if request.namespace != "production" || request.selector != "kubernetes.io/service-name in (game,wallet)" {
			t.Fatalf("请求过滤条件 = %#v", request)
		}
	}
}

func TestTargetsAreGroupedByNamespace(t *testing.T) {
	transport := &discoveryTransport{watchStarted: make(chan struct{}, 2)}
	resolver := newResolver("https://kubernetes.test", &http.Client{Transport: transport}, "")
	t.Cleanup(func() { _ = resolver.Shutdown(context.Background()) })
	for _, target := range []string{
		"kube://production/game:connect",
		"kube://production/wallet:connect",
		"kube://payments/billing:connect",
	} {
		if _, _, err := resolver.Resolve(context.Background(), target); err != nil {
			t.Fatal(err)
		}
	}
	if err := resolver.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		select {
		case <-transport.watchStarted:
		case <-time.After(time.Second):
			t.Fatal("watch 未启动")
		}
	}

	transport.mu.Lock()
	defer transport.mu.Unlock()
	if transport.listCalls != 2 || transport.watchCalls != 2 || transport.maxActiveWatches != 2 {
		t.Fatalf("LIST=%d WATCH=%d maxActive=%d", transport.listCalls, transport.watchCalls, transport.maxActiveWatches)
	}
}

func TestExpiredWatchRelistsAndPublishesFreshState(t *testing.T) {
	transport := &expiredTransport{secondWatchStarted: make(chan struct{})}
	resolver := newResolver("https://kubernetes.test", &http.Client{Transport: transport}, "")
	t.Cleanup(func() { _ = resolver.Shutdown(context.Background()) })

	_, client, err := resolver.Resolve(context.Background(), "kube://production/game:connect")
	if err != nil {
		t.Fatal(err)
	}
	if err := resolver.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-transport.secondWatchStarted:
	case <-time.After(time.Second):
		t.Fatal("410 后没有重新 LIST 并恢复 watch")
	}

	deadline := time.Now().Add(time.Second)
	for !backendHasEndpoint(client) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !backendHasEndpoint(client) {
		t.Fatal("重新 LIST 后没有发布新 endpoint")
	}
	transport.mu.Lock()
	defer transport.mu.Unlock()
	if transport.listCalls != 2 || transport.watchCalls != 2 {
		t.Fatalf("LIST=%d WATCH=%d, want 2/2", transport.listCalls, transport.watchCalls)
	}
}

func TestListTimeoutBoundsStartup(t *testing.T) {
	resolver := newResolver("https://kubernetes.test", &http.Client{Transport: blockingTransport{}}, "")
	resolver.listTimeout = 20 * time.Millisecond
	t.Cleanup(func() { _ = resolver.Shutdown(context.Background()) })
	if _, _, err := resolver.Resolve(context.Background(), "kube://production/game:connect"); err != nil {
		t.Fatal(err)
	}

	started := time.Now()
	err := resolver.Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), context.DeadlineExceeded.Error()) {
		t.Fatalf("Start error = %v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("LIST 超时耗时 %v", elapsed)
	}
}

func TestWatchClientDeadlineReconnectsStalledStream(t *testing.T) {
	transport := &deadlineTransport{secondWatchStarted: make(chan struct{})}
	resolver := newResolver("https://kubernetes.test", &http.Client{Transport: transport}, "")
	resolver.watchTimeout = 20 * time.Millisecond
	resolver.watchGrace = 10 * time.Millisecond
	t.Cleanup(func() { _ = resolver.Shutdown(context.Background()) })
	if _, _, err := resolver.Resolve(context.Background(), "kube://production/game:connect"); err != nil {
		t.Fatal(err)
	}
	if err := resolver.Start(context.Background()); err != nil {
		t.Fatal(err)
	}

	select {
	case <-transport.secondWatchStarted:
	case <-time.After(time.Second):
		t.Fatal("客户端 deadline 后没有重连 WATCH")
	}
}

func testSlice(name, namespace, service, addressType string, endpoints []endpoint, ports []endpointPort) endpointSlice {
	return endpointSlice{
		Metadata: objectMetadata{
			Name:      name,
			Namespace: namespace,
			Labels:    map[string]string{"kubernetes.io/service-name": service},
		},
		AddressType: addressType,
		Endpoints:   endpoints,
		Ports:       ports,
	}
}

func backendHasEndpoint(client *backendClient) bool {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	request, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://service/test", nil)
	_, err := client.Do(request)
	return !errors.Is(err, balancer.ErrNoAvailableEndpoint)
}

type discoveryTransport struct {
	mu               sync.Mutex
	listCalls        int
	watchCalls       int
	activeWatches    int
	maxActiveWatches int
	watchStarted     chan struct{}
	requests         []discoveryRequest
}

type discoveryRequest struct {
	namespace string
	selector  string
	watch     bool
}

func (t *discoveryTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	parts := strings.Split(request.URL.Path, "/")
	namespace := ""
	for index, part := range parts {
		if part == "namespaces" && index+1 < len(parts) {
			namespace = parts[index+1]
			break
		}
	}
	isWatch := request.URL.Query().Get("watch") == "true"
	t.mu.Lock()
	t.requests = append(t.requests, discoveryRequest{
		namespace: namespace,
		selector:  request.URL.Query().Get("labelSelector"),
		watch:     isWatch,
	})
	t.mu.Unlock()
	if !isWatch {
		t.mu.Lock()
		t.listCalls++
		t.mu.Unlock()
		payload, _ := json.Marshal(endpointSliceList{Metadata: listMetadata{ResourceVersion: "1"}})
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(string(payload))),
			Request:    request,
		}, nil
	}

	t.mu.Lock()
	t.watchCalls++
	t.activeWatches++
	if t.activeWatches > t.maxActiveWatches {
		t.maxActiveWatches = t.activeWatches
	}
	t.mu.Unlock()
	t.watchStarted <- struct{}{}
	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Header:     make(http.Header),
		Body: &contextBody{
			ctx: request.Context(),
			onClose: func() {
				t.mu.Lock()
				t.activeWatches--
				t.mu.Unlock()
			},
		},
		Request: request,
	}, nil
}

type contextBody struct {
	ctx      context.Context
	onClose  func()
	closeOne sync.Once
}

type expiredTransport struct {
	mu                 sync.Mutex
	listCalls          int
	watchCalls         int
	secondWatchStarted chan struct{}
}

type blockingTransport struct{}

func (blockingTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	<-request.Context().Done()
	return nil, request.Context().Err()
}

type deadlineTransport struct {
	mu                 sync.Mutex
	watchCalls         int
	secondWatchStarted chan struct{}
	secondOnce         sync.Once
}

func (t *deadlineTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.URL.Query().Get("watch") != "true" {
		payload, _ := json.Marshal(endpointSliceList{Metadata: listMetadata{ResourceVersion: "1"}})
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(string(payload))),
			Request:    request,
		}, nil
	}
	t.mu.Lock()
	t.watchCalls++
	call := t.watchCalls
	t.mu.Unlock()
	if call >= 2 {
		t.secondOnce.Do(func() { close(t.secondWatchStarted) })
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Header:     make(http.Header),
		Body:       &contextBody{ctx: request.Context(), onClose: func() {}},
		Request:    request,
	}, nil
}

func (t *expiredTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	isWatch := request.URL.Query().Get("watch") == "true"
	t.mu.Lock()
	if isWatch {
		t.watchCalls++
		call := t.watchCalls
		t.mu.Unlock()
		if call == 1 {
			return &http.Response{
				StatusCode: http.StatusGone,
				Status:     "410 Gone",
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(`{"message":"expired"}`)),
				Request:    request,
			}, nil
		}
		close(t.secondWatchStarted)
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     make(http.Header),
			Body:       &contextBody{ctx: request.Context(), onClose: func() {}},
			Request:    request,
		}, nil
	}

	t.listCalls++
	call := t.listCalls
	t.mu.Unlock()
	page := endpointSliceList{Metadata: listMetadata{ResourceVersion: strconv.Itoa(call)}}
	if call == 2 {
		name, tcp, port := "connect", "TCP", 8080
		page.Items = []endpointSlice{testSlice(
			"fresh",
			"production",
			"game",
			"IPv4",
			[]endpoint{{Addresses: []string{"10.0.0.8"}}},
			[]endpointPort{{Name: &name, Protocol: &tcp, Port: &port}},
		)}
	}
	payload, _ := json.Marshal(page)
	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(string(payload))),
		Request:    request,
	}, nil
}

func (b *contextBody) Read([]byte) (int, error) {
	<-b.ctx.Done()
	return 0, b.ctx.Err()
}

func (b *contextBody) Close() error {
	b.closeOne.Do(b.onClose)
	return nil
}
