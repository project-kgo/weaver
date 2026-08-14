package balancer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

var (
	// ErrNoAvailableEndpoint 表示当前没有可用的 endpoint。
	ErrNoAvailableEndpoint = errors.New("balancer:  no available endpoint")
	// ErrClosed 表示 Client 已关闭。
	ErrClosed            = errors.New("balancer: Client closed")
	errNoUntriedEndpoint = errors.New("balancer: no untried endpoint")
)

// EndpointFactory 为一个 endpoint 创建独立的 HTTP Client。
// 调用方可在返回的 Client 中配置 h2c、TLS SNI 和自定义拨号。
type EndpointFactory func(address string) (*http.Client, error)

type endpointSnapshot struct {
	endpoints []*endpoint
}

type endpoint struct {
	address  string
	client   *http.Client
	active   atomic.Bool
	inflight atomic.Int64
	metrics  atomic.Pointer[endpointMetrics]
}

// Client 管理动态 endpoint 集合，并实现 Connect 所需的 HTTPClient 接口。
type Client struct {
	factory EndpointFactory
	options options

	snapshot atomic.Pointer[endpointSnapshot]
	mu       sync.Mutex
	byAddr   map[string]*endpoint
	closed   atomic.Bool

	now     func() time.Time
	randomN func(int) int
}

// New 创建一个初始无 endpoint 的负载均衡 Client。
func New(factory EndpointFactory, values ...Option) (*Client, error) {
	if factory == nil {
		return nil, fmt.Errorf("balancer: EndpointFactory 不能为 nil")
	}
	configured := defaultOptions()
	for _, value := range values {
		if value == nil {
			return nil, fmt.Errorf("balancer: Option 不能为 nil")
		}
		if err := value(&configured); err != nil {
			return nil, err
		}
	}
	client := &Client{
		factory: factory,
		options: configured,
		byAddr:  make(map[string]*endpoint),
		now:     time.Now,
		randomN: rand.IntN,
	}
	client.snapshot.Store(&endpointSnapshot{})
	return client, nil
}

// Update 原子替换 endpoint 地址集合。保留地址会继续复用连接池和历史指标。
func (c *Client) Update(addresses []string) error {
	addresses = normalizeAddresses(addresses)

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed.Load() {
		return ErrClosed
	}

	initialLatency := c.initialLatencyLocked()
	nextByAddr := make(map[string]*endpoint, len(addresses))
	nextEndpoints := make([]*endpoint, 0, len(addresses))
	created := make([]*endpoint, 0, len(addresses))
	for _, address := range addresses {
		current := c.byAddr[address]
		if current == nil {
			httpClient, err := c.factory(address)
			if err != nil {
				closeEndpoints(created)
				return fmt.Errorf("balancer: 创建 endpoint %q 失败: %w", address, err)
			}
			if httpClient == nil {
				closeEndpoints(created)
				return fmt.Errorf("balancer: EndpointFactory 为 %q 返回 nil Client", address)
			}
			current = &endpoint{address: address, client: httpClient}
			current.metrics.Store(newEndpointMetrics(initialLatency, c.now()))
			created = append(created, current)
		}
		nextByAddr[address] = current
		nextEndpoints = append(nextEndpoints, current)
	}

	previous := c.byAddr
	for _, current := range nextEndpoints {
		current.active.Store(true)
	}
	c.byAddr = nextByAddr
	c.snapshot.Store(&endpointSnapshot{endpoints: nextEndpoints})
	for address, current := range previous {
		if _, retained := nextByAddr[address]; retained {
			continue
		}
		current.active.Store(false)
		current.client.CloseIdleConnections()
	}
	return nil
}

// Do 选择 endpoint 并执行 HTTP 请求。
func (c *Client) Do(request *http.Request) (*http.Response, error) {
	if request == nil {
		return nil, fmt.Errorf("balancer: request 不能为 nil")
	}
	if c.closed.Load() {
		return nil, ErrClosed
	}
	attempted := make(map[string]struct{}, c.options.maxAttempts)
	var lastRetryableError error

	for attempt := 0; attempt < c.options.maxAttempts; attempt++ {
		selected, err := c.pick(attempted)
		if errors.Is(err, errNoUntriedEndpoint) {
			return nil, lastRetryableError
		}
		if err != nil {
			return nil, err
		}
		currentRequest := request
		if attempt > 0 {
			replayed, ok := replayRequest(request)
			if !ok {
				return nil, lastRetryableError
			}
			currentRequest = replayed
		}
		attempted[selected.address] = struct{}{}

		selected.inflight.Add(1)
		response, err := c.doEndpoint(selected, currentRequest)
		if err == nil || c.options.retryable == nil || !c.options.retryable(err) || request.Context().Err() != nil {
			return response, err
		}
		lastRetryableError = err
		if response != nil || attempt+1 >= c.options.maxAttempts {
			return response, err
		}
	}
	return nil, lastRetryableError
}

// Close 关闭 Client，清空 endpoint 快照并关闭全部空闲连接。
// 已经开始的请求不会被强制中断。
func (c *Client) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed.Swap(true) {
		return
	}
	previous := c.byAddr
	c.byAddr = make(map[string]*endpoint)
	c.snapshot.Store(&endpointSnapshot{})
	for _, current := range previous {
		current.active.Store(false)
		current.client.CloseIdleConnections()
	}
}

func (c *Client) pick(excluded map[string]struct{}) (*endpoint, error) {
	for {
		snapshot := c.snapshot.Load()
		if snapshot == nil || len(snapshot.endpoints) == 0 {
			return nil, ErrNoAvailableEndpoint
		}
		first, hasActive := c.randomEndpoint(snapshot.endpoints, excluded, nil)
		if first == nil {
			if hasActive {
				if c.snapshot.Load() != snapshot {
					continue
				}
				return nil, errNoUntriedEndpoint
			}
			continue
		}
		second, _ := c.randomEndpoint(snapshot.endpoints, excluded, first)
		if second == nil {
			return first, nil
		}
		now := c.now()
		selected := first
		if second.score(now, c.options) < first.score(now, c.options) {
			selected = second
		}
		return selected, nil
	}
}

func (c *Client) randomEndpoint(endpoints []*endpoint, excluded map[string]struct{}, skip *endpoint) (*endpoint, bool) {
	hasActive := false
	for range len(endpoints) * 2 {
		candidate := endpoints[c.randomN(len(endpoints))]
		if !candidate.active.Load() {
			continue
		}
		hasActive = true
		if candidate == skip {
			continue
		}
		if _, ignored := excluded[candidate.address]; ignored {
			continue
		}
		return candidate, true
	}
	start := c.randomN(len(endpoints))
	for offset := range len(endpoints) {
		candidate := endpoints[(start+offset)%len(endpoints)]
		if !candidate.active.Load() {
			continue
		}
		hasActive = true
		if candidate == skip {
			continue
		}
		if _, ignored := excluded[candidate.address]; ignored {
			continue
		}
		return candidate, true
	}
	return nil, hasActive
}

func (c *Client) doEndpoint(selected *endpoint, request *http.Request) (*http.Response, error) {
	started := c.now()
	response, err := selected.client.Do(request)
	if err != nil {
		finished := c.now()
		selected.inflight.Add(-1)
		selected.observe(finished, finished.Sub(started), errorOutcome(request.Context(), err), c.options.ewmaHalfLife)
		return response, err
	}
	statusOutcome := responseOutcome(response.StatusCode)
	if response.Body == nil {
		finished := c.now()
		selected.inflight.Add(-1)
		selected.observe(finished, finished.Sub(started), statusOutcome, c.options.ewmaHalfLife)
		return response, nil
	}
	response.Body = &observedBody{
		ReadCloser: response.Body,
		ctx:        request.Context(),
		fallback:   statusOutcome,
		complete: func(result outcome) {
			finished := c.now()
			selected.inflight.Add(-1)
			selected.observe(finished, finished.Sub(started), result, c.options.ewmaHalfLife)
		},
	}
	return response, nil
}

func (c *Client) initialLatencyLocked() time.Duration {
	var total float64
	var count int
	for _, current := range c.byAddr {
		metrics := current.metrics.Load()
		if metrics == nil {
			continue
		}
		total += metrics.latency
		count++
	}
	if count == 0 {
		return defaultInitialLatency
	}
	return time.Duration(total / float64(count))
}

func normalizeAddresses(addresses []string) []string {
	result := append([]string(nil), addresses...)
	sort.Strings(result)
	unique := result[:0]
	for _, address := range result {
		if len(unique) != 0 && unique[len(unique)-1] == address {
			continue
		}
		unique = append(unique, address)
	}
	return unique
}

func closeEndpoints(endpoints []*endpoint) {
	for _, current := range endpoints {
		current.client.CloseIdleConnections()
	}
}

func replayRequest(request *http.Request) (*http.Request, bool) {
	replayed := request.Clone(request.Context())
	if request.Body == nil || request.Body == http.NoBody {
		replayed.Body = request.Body
		return replayed, true
	}
	if request.GetBody == nil {
		return nil, false
	}
	body, err := request.GetBody()
	if err != nil {
		return nil, false
	}
	replayed.Body = body
	return replayed, true
}

func responseOutcome(statusCode int) outcome {
	switch statusCode {
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return outcomeFailure
	default:
		return outcomeSuccess
	}
}

func errorOutcome(ctx context.Context, err error) outcome {
	if errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
		return outcomeNeutral
	}
	return outcomeFailure
}

type observedBody struct {
	io.ReadCloser
	ctx      context.Context
	fallback outcome
	complete func(outcome)
	once     sync.Once
}

func (b *observedBody) Read(buffer []byte) (int, error) {
	read, err := b.ReadCloser.Read(buffer)
	switch {
	case errors.Is(err, io.EOF):
		b.finish(b.fallback)
	case err != nil:
		b.finish(b.outcomeForBodyError(err))
	}
	return read, err
}

func (b *observedBody) Close() error {
	err := b.ReadCloser.Close()
	if err != nil {
		b.finish(b.outcomeForBodyError(err))
	} else {
		b.finish(b.fallback)
	}
	return err
}

func (b *observedBody) outcomeForBodyError(err error) outcome {
	if b.fallback == outcomeFailure {
		return outcomeFailure
	}
	return bodyErrorOutcome(b.ctx, err)
}

func (b *observedBody) finish(result outcome) {
	b.once.Do(func() {
		b.complete(result)
	})
}

func bodyErrorOutcome(ctx context.Context, err error) outcome {
	if errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
		return outcomeNeutral
	}
	return outcomeFailure
}
