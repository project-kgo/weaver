package kube

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"

	"github.com/project-kgo/weaver/balancer"
)

type backendClient struct {
	target target
	client *balancer.Client
}

func newBackendClient(value target) (*backendClient, error) {
	client, err := balancer.New(
		func(address string) (*http.Client, error) {
			return newEndpointClient(value, address), nil
		},
		balancer.WithRetryableError(isEndpointDialError),
	)
	if err != nil {
		return nil, err
	}
	return &backendClient{target: value, client: client}, nil
}

func (c *backendClient) Do(request *http.Request) (*http.Response, error) {
	return c.client.Do(request)
}

func (c *backendClient) update(addresses []string) error {
	return c.client.Update(addresses)
}

func (c *backendClient) close() {
	c.client.Close()
}

type endpointDialError struct {
	err error
}

func (e *endpointDialError) Error() string {
	return e.err.Error()
}

func (e *endpointDialError) Unwrap() error {
	return e.err
}

func isEndpointDialError(err error) bool {
	var dialError *endpointDialError
	return errors.As(err, &dialError)
}

func newEndpointClient(target target, address string) *http.Client {
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
		connection, err := dialer.DialContext(ctx, network, address)
		if err != nil {
			// 只标记连接建立前的错误，确保切换 endpoint 不会重放已送达的 RPC。
			return nil, &endpointDialError{err: err}
		}
		return connection, nil
	}
	if target.transport == transportHTTPS {
		transport.TLSClientConfig = &tls.Config{
			MinVersion: tls.VersionTLS12,
			ServerName: target.serverName(),
		}
	}

	return &http.Client{Transport: transport}
}
