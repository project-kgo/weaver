package kube

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"sort"
	"sync"
	"sync/atomic"
)

type endpointSnapshot struct {
	endpoints []*endpointTransport
}

type endpointTransport struct {
	address string
	client  *http.Client
	active  atomic.Bool
}

type backendClient struct {
	target target

	next     atomic.Uint64
	snapshot atomic.Pointer[endpointSnapshot]
	mu       sync.Mutex
	byAddr   map[string]*endpointTransport
}

func newBackendClient(value target) *backendClient {
	client := &backendClient{target: value, byAddr: make(map[string]*endpointTransport)}
	client.snapshot.Store(&endpointSnapshot{})
	return client
}

func (c *backendClient) Do(request *http.Request) (*http.Response, error) {
	endpoint, err := c.selectEndpoint()
	if err != nil {
		return nil, err
	}
	return endpoint.client.Do(request)
}

func (c *backendClient) selectEndpoint() (*endpointTransport, error) {
	for {
		snapshot := c.snapshot.Load()
		if snapshot == nil || len(snapshot.endpoints) == 0 {
			return nil, fmt.Errorf("kube: service %s/%s 没有可用 endpoint", c.target.namespace, c.target.service)
		}
		index := (c.next.Add(1) - 1) % uint64(len(snapshot.endpoints))
		endpoint := snapshot.endpoints[index]
		if endpoint.active.Load() {
			return endpoint, nil
		}
		// 端点刚被新快照替换时重新读取，不使用固定重试次数，避免批量缩容时误失败。
	}
}

func (c *backendClient) update(addresses []string) {
	addresses = append([]string(nil), addresses...)
	sort.Strings(addresses)

	c.mu.Lock()
	defer c.mu.Unlock()

	nextByAddr := make(map[string]*endpointTransport, len(addresses))
	nextEndpoints := make([]*endpointTransport, 0, len(addresses))
	for _, address := range addresses {
		if _, duplicate := nextByAddr[address]; duplicate {
			continue
		}
		endpoint := c.byAddr[address]
		if endpoint == nil {
			endpoint = newEndpointTransport(c.target, address)
		}
		endpoint.active.Store(true)
		nextByAddr[address] = endpoint
		nextEndpoints = append(nextEndpoints, endpoint)
	}

	previous := c.byAddr
	// 先原子发布完整新快照。已经读取旧快照的请求视为更新前开始，允许继续完成；
	// 更新后的请求只会看到新快照，不会因为旧 endpoint 被逐个失活而短暂误失败。
	c.byAddr = nextByAddr
	c.snapshot.Store(&endpointSnapshot{endpoints: nextEndpoints})
	for address, endpoint := range previous {
		if _, retained := nextByAddr[address]; retained {
			continue
		}
		endpoint.active.Store(false)
		endpoint.client.CloseIdleConnections()
	}
}

func (c *backendClient) close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	previous := c.byAddr
	c.byAddr = make(map[string]*endpointTransport)
	c.snapshot.Store(&endpointSnapshot{})
	for _, endpoint := range previous {
		endpoint.active.Store(false)
		endpoint.client.CloseIdleConnections()
	}
}

func newEndpointTransport(target target, address string) *endpointTransport {
	protocols := new(http.Protocols)
	protocols.SetHTTP2(true)
	if target.transport == transportH2C {
		protocols.SetUnencryptedHTTP2(true)
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Protocols = protocols
	transport.Proxy = nil
	dialer := &net.Dialer{}
	transport.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return dialer.DialContext(ctx, network, address)
	}
	if target.transport == transportHTTPS {
		transport.TLSClientConfig = &tls.Config{
			MinVersion: tls.VersionTLS12,
			ServerName: target.serverName(),
		}
	}

	result := &endpointTransport{
		address: address,
		client:  &http.Client{Transport: transport},
	}
	result.active.Store(true)
	return result
}
