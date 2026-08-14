package balancer

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

type trackingTransport struct {
	roundTrip func(*http.Request) (*http.Response, error)
	closed    atomic.Int64
}

func (t *trackingTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if t.roundTrip != nil {
		return t.roundTrip(request)
	}
	return &http.Response{StatusCode: http.StatusNoContent, Body: http.NoBody}, nil
}

func (t *trackingTransport) CloseIdleConnections() {
	t.closed.Add(1)
}

type retryableTestError struct {
	err error
}

func (e *retryableTestError) Error() string { return e.err.Error() }
func (e *retryableTestError) Unwrap() error { return e.err }

func TestP2CChoosesLowerScore(t *testing.T) {
	client := newScoringClient(t)
	now := client.now()
	first := client.byAddr["first"]
	second := client.byAddr["second"]

	tests := []struct {
		name          string
		firstLatency  time.Duration
		firstErrors   float64
		firstInflight int64
		secondLatency time.Duration
		secondErrors  float64
		want          string
	}{
		{
			name:          "latency",
			firstLatency:  10 * time.Millisecond,
			secondLatency: 100 * time.Millisecond,
			want:          "first",
		},
		{
			name:          "errors",
			firstLatency:  10 * time.Millisecond,
			firstErrors:   1,
			secondLatency: 50 * time.Millisecond,
			want:          "second",
		},
		{
			name:          "inflight",
			firstLatency:  10 * time.Millisecond,
			firstInflight: 10,
			secondLatency: 50 * time.Millisecond,
			want:          "second",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			first.metrics.Store(testMetrics(test.firstLatency, test.firstErrors, now))
			second.metrics.Store(testMetrics(test.secondLatency, test.secondErrors, now))
			first.inflight.Store(test.firstInflight)
			second.inflight.Store(0)
			setRandomSequence(client, 0, 1)

			selected, err := client.pick(nil)
			if err != nil {
				t.Fatal(err)
			}
			if selected.address != test.want {
				t.Fatalf("selected = %q, want %q", selected.address, test.want)
			}
		})
	}
}

func TestP2CSingleAndNoEndpoint(t *testing.T) {
	client, err := New(testFactory(nil))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.pick(nil); !errors.Is(err, ErrNoAvailableEndpoint) {
		t.Fatalf("empty pick error = %v, want %v", err, ErrNoAvailableEndpoint)
	}
	if err := client.Update([]string{"only"}); err != nil {
		t.Fatal(err)
	}
	selected, err := client.pick(nil)
	if err != nil {
		t.Fatal(err)
	}
	if selected.address != "only" {
		t.Fatalf("selected = %q, want only", selected.address)
	}
	if _, err := client.pick(map[string]struct{}{"only": {}}); !errors.Is(err, errNoUntriedEndpoint) {
		t.Fatalf("excluded pick error = %v, want %v", err, errNoUntriedEndpoint)
	}
}

func TestErrorPenaltyDecays(t *testing.T) {
	client := newScoringClient(t)
	now := client.now()
	current := client.byAddr["first"]
	current.metrics.Store(testMetrics(20*time.Millisecond, 1, now))

	initial := current.score(now, client.options)
	afterHalfLife := current.score(now.Add(client.options.ewmaHalfLife), client.options)
	want := float64(20*time.Millisecond + client.options.errorPenalty/2)
	if initial <= afterHalfLife {
		t.Fatalf("score did not decay: initial=%f after=%f", initial, afterHalfLife)
	}
	if difference := afterHalfLife - want; difference < -1 || difference > 1 {
		t.Fatalf("score after half-life = %f, want %f", afterHalfLife, want)
	}
}

func TestFastRetryableFailureIsPenalized(t *testing.T) {
	var firstCalls atomic.Int64
	var secondCalls atomic.Int64
	factory := func(address string) (*http.Client, error) {
		switch address {
		case "first":
			return &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
				firstCalls.Add(1)
				return nil, &retryableTestError{err: errors.New("dial failed")}
			})}, nil
		case "second":
			return &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
				secondCalls.Add(1)
				return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}, nil
			})}, nil
		default:
			return nil, errors.New("unexpected endpoint")
		}
	}
	client, err := New(factory, WithRetryableError(isRetryableTestError))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(100, 0)
	client.now = func() time.Time { return now }
	setRandomSequence(client, 0, 1)
	if err := client.Update([]string{"first", "second"}); err != nil {
		t.Fatal(err)
	}

	request, _ := http.NewRequest(http.MethodPost, "http://service/test", nil)
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if firstCalls.Load() != 1 || secondCalls.Load() != 1 {
		t.Fatalf("calls = %d/%d, want 1/1", firstCalls.Load(), secondCalls.Load())
	}

	first := client.byAddr["first"]
	second := client.byAddr["second"]
	if first.score(now, client.options) <= second.score(now, client.options) {
		t.Fatalf("failed endpoint score = %f, healthy score = %f", first.score(now, client.options), second.score(now, client.options))
	}
}

func TestMetricsClassifyInfrastructureFailures(t *testing.T) {
	tests := []struct {
		name      string
		response  func() *http.Response
		err       error
		cancel    bool
		want      outcome
		readBody  bool
		closeBody bool
	}{
		{
			name: "success",
			response: func() *http.Response {
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("ok"))}
			},
			want:     outcomeSuccess,
			readBody: true,
		},
		{
			name: "business 500",
			response: func() *http.Response {
				return &http.Response{StatusCode: http.StatusInternalServerError, Body: http.NoBody}
			},
			want:      outcomeSuccess,
			closeBody: true,
		},
		{
			name: "gateway 502",
			response: func() *http.Response {
				return &http.Response{StatusCode: http.StatusBadGateway, Body: http.NoBody}
			},
			want:      outcomeFailure,
			closeBody: true,
		},
		{
			name: "unavailable 503",
			response: func() *http.Response {
				return &http.Response{StatusCode: http.StatusServiceUnavailable, Body: http.NoBody}
			},
			want:      outcomeFailure,
			closeBody: true,
		},
		{
			name: "unavailable remains failure when body canceled",
			response: func() *http.Response {
				return &http.Response{StatusCode: http.StatusServiceUnavailable, Body: &errorReadCloser{err: context.Canceled}}
			},
			want:     outcomeFailure,
			readBody: true,
		},
		{
			name: "gateway timeout 504",
			response: func() *http.Response {
				return &http.Response{StatusCode: http.StatusGatewayTimeout, Body: http.NoBody}
			},
			want:      outcomeFailure,
			closeBody: true,
		},
		{name: "transport error", err: errors.New("network failed"), want: outcomeFailure},
		{name: "caller canceled", err: context.Canceled, cancel: true, want: outcomeNeutral},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			factory := func(string) (*http.Client, error) {
				return &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
					if test.cancel {
						cancel()
					}
					if test.response == nil {
						return nil, test.err
					}
					return test.response(), test.err
				})}, nil
			}
			client, err := New(factory)
			if err != nil {
				t.Fatal(err)
			}
			now := time.Unix(200, 0)
			client.now = func() time.Time { return now }
			if err := client.Update([]string{"endpoint"}); err != nil {
				t.Fatal(err)
			}
			request, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://service/test", nil)
			response, gotErr := client.Do(request)
			if test.err == nil && gotErr != nil {
				t.Fatal(gotErr)
			}
			now = now.Add(20 * time.Millisecond)
			if response != nil && test.readBody {
				_, _ = io.ReadAll(response.Body)
			}
			if response != nil && test.closeBody {
				_ = response.Body.Close()
			}

			metrics := client.byAddr["endpoint"].metrics.Load()
			switch test.want {
			case outcomeNeutral:
				if metrics.errorSet || metrics.latencySet {
					t.Fatalf("neutral outcome updated metrics: %+v", metrics)
				}
			case outcomeSuccess:
				if !metrics.latencySet || metrics.errorRate != 0 {
					t.Fatalf("success metrics = %+v", metrics)
				}
			case outcomeFailure:
				if !metrics.errorSet || metrics.errorRate != 1 || metrics.latencySet {
					t.Fatalf("failure metrics = %+v", metrics)
				}
			}
		})
	}
}

func TestResponseBodyErrorAndCloseSettleOnce(t *testing.T) {
	bodyError := errors.New("body read failed")
	body := &errorReadCloser{err: bodyError}
	client, err := New(func(string) (*http.Client, error) {
		return &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Body: body}, nil
		})}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(300, 0)
	client.now = func() time.Time { return now }
	if err := client.Update([]string{"endpoint"}); err != nil {
		t.Fatal(err)
	}
	request, _ := http.NewRequest(http.MethodPost, "http://service/test", nil)
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(10 * time.Millisecond)
	_, err = io.ReadAll(response.Body)
	if !errors.Is(err, bodyError) {
		t.Fatalf("body error = %v, want %v", err, bodyError)
	}
	current := client.byAddr["endpoint"]
	settled := current.metrics.Load()
	if current.inflight.Load() != 0 || settled.errorRate != 1 {
		t.Fatalf("settled state: inflight=%d metrics=%+v", current.inflight.Load(), settled)
	}
	now = now.Add(time.Second)
	_ = response.Body.Close()
	if current.metrics.Load() != settled || current.inflight.Load() != 0 {
		t.Fatal("response body settled more than once")
	}
}

func TestRetryUsesAnotherEndpointAndReplaysBody(t *testing.T) {
	var firstCalls atomic.Int64
	var secondCalls atomic.Int64
	client, err := New(func(address string) (*http.Client, error) {
		return &http.Client{Transport: roundTripperFunc(func(request *http.Request) (*http.Response, error) {
			switch address {
			case "first":
				firstCalls.Add(1)
				return nil, &retryableTestError{err: errors.New("dial failed")}
			case "second":
				secondCalls.Add(1)
				body, readErr := io.ReadAll(request.Body)
				if readErr != nil {
					t.Fatal(readErr)
				}
				if string(body) != "payload" {
					t.Fatalf("retry body = %q, want payload", body)
				}
				return &http.Response{StatusCode: http.StatusNoContent, Body: http.NoBody}, nil
			default:
				return nil, errors.New("unexpected endpoint")
			}
		})}, nil
	}, WithRetryableError(isRetryableTestError))
	if err != nil {
		t.Fatal(err)
	}
	client.randomN = func(int) int { return 0 }
	if err := client.Update([]string{"first", "second"}); err != nil {
		t.Fatal(err)
	}
	request, _ := http.NewRequest(http.MethodPost, "http://service/test", strings.NewReader("payload"))
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if firstCalls.Load() != 1 || secondCalls.Load() != 1 {
		t.Fatalf("calls = %d/%d, want 1/1", firstCalls.Load(), secondCalls.Load())
	}
}

func TestRetrySafetyGuards(t *testing.T) {
	t.Run("ambiguous error", func(t *testing.T) {
		ambiguous := errors.New("connection reset")
		client, secondCalls := retryGuardClient(t, func() error { return ambiguous })
		request, _ := http.NewRequest(http.MethodPost, "http://service/test", nil)
		_, err := client.Do(request)
		if !errors.Is(err, ambiguous) || secondCalls.Load() != 0 {
			t.Fatalf("error=%v secondCalls=%d", err, secondCalls.Load())
		}
	})

	t.Run("context canceled", func(t *testing.T) {
		var cancel context.CancelFunc
		client, secondCalls := retryGuardClient(t, func() error {
			cancel()
			return &retryableTestError{err: errors.New("dial failed")}
		})
		ctx, cancelContext := context.WithCancel(context.Background())
		cancel = cancelContext
		defer cancel()
		request, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://service/test", nil)
		_, _ = client.Do(request)
		if secondCalls.Load() != 0 {
			t.Fatalf("secondCalls=%d, want 0", secondCalls.Load())
		}
	})

	t.Run("body not replayable", func(t *testing.T) {
		client, secondCalls := retryGuardClient(t, func() error {
			return &retryableTestError{err: errors.New("dial failed")}
		})
		request, _ := http.NewRequest(http.MethodPost, "http://service/test", io.NopCloser(strings.NewReader("payload")))
		_, _ = client.Do(request)
		if secondCalls.Load() != 0 {
			t.Fatalf("secondCalls=%d, want 0", secondCalls.Load())
		}
	})
}

func TestMaxAttemptsUsesDistinctEndpoints(t *testing.T) {
	calls := make(map[string]*atomic.Int64)
	client, err := New(func(address string) (*http.Client, error) {
		counter := new(atomic.Int64)
		calls[address] = counter
		return &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
			counter.Add(1)
			return nil, &retryableTestError{err: errors.New("dial failed")}
		})}, nil
	}, WithRetryableError(isRetryableTestError), WithMaxAttempts(2))
	if err != nil {
		t.Fatal(err)
	}
	client.randomN = func(int) int { return 0 }
	if err := client.Update([]string{"first", "second", "third"}); err != nil {
		t.Fatal(err)
	}
	request, _ := http.NewRequest(http.MethodPost, "http://service/test", nil)
	_, _ = client.Do(request)
	if calls["first"].Load() != 1 || calls["second"].Load() != 1 || calls["third"].Load() != 0 {
		t.Fatalf("calls = %d/%d/%d, want 1/1/0", calls["first"].Load(), calls["second"].Load(), calls["third"].Load())
	}
}

func TestUpdatePreservesEndpointsAndIsAtomicOnFailure(t *testing.T) {
	transports := make(map[string]*trackingTransport)
	factoryError := errors.New("factory failed")
	client, err := New(func(address string) (*http.Client, error) {
		if address == "zbad" {
			return nil, factoryError
		}
		transport := new(trackingTransport)
		transports[address] = transport
		return &http.Client{Transport: transport}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Update([]string{"first", "second", "second"}); err != nil {
		t.Fatal(err)
	}
	first := client.byAddr["first"]
	first.metrics.Store(testMetrics(25*time.Millisecond, 0.25, time.Now()))
	metrics := first.metrics.Load()
	if err := client.Update([]string{"first", "third"}); err != nil {
		t.Fatal(err)
	}
	if client.byAddr["first"] != first || client.byAddr["first"].metrics.Load() != metrics {
		t.Fatal("retained endpoint did not preserve client and metrics")
	}
	if transports["second"].closed.Load() != 1 {
		t.Fatalf("removed endpoint closed = %d, want 1", transports["second"].closed.Load())
	}

	err = client.Update([]string{"first", "temporary", "zbad"})
	if !errors.Is(err, factoryError) {
		t.Fatalf("Update error = %v, want %v", err, factoryError)
	}
	if len(client.snapshot.Load().endpoints) != 2 || client.byAddr["third"] == nil {
		t.Fatal("failed update changed published snapshot")
	}
	if transports["temporary"].closed.Load() != 1 {
		t.Fatal("client created by failed update was not closed")
	}

	client.Close()
	client.Close()
	if transports["first"].closed.Load() != 1 || transports["third"].closed.Load() != 1 {
		t.Fatal("Close did not close retained endpoint clients exactly once")
	}
	if !errors.Is(client.Update([]string{"later"}), ErrClosed) {
		t.Fatal("Update after Close did not return ErrClosed")
	}
}

func TestSelectionDuringUpdatesNeverFailsTransiently(t *testing.T) {
	client, err := New(testFactory(nil))
	if err != nil {
		t.Fatal(err)
	}
	many := make([]string, 128)
	for index := range many {
		many[index] = strings.Repeat("x", index+1)
	}
	retained := []string{many[len(many)-1]}
	if err := client.Update(many); err != nil {
		t.Fatal(err)
	}

	var wait sync.WaitGroup
	errorsFound := make(chan error, 8)
	for range 8 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for range 20_000 {
				if _, err := client.pick(nil); err != nil {
					errorsFound <- err
					return
				}
			}
		}()
	}
	for range 1_000 {
		if err := client.Update(retained); err != nil {
			t.Fatal(err)
		}
		if err := client.Update(many); err != nil {
			t.Fatal(err)
		}
	}
	wait.Wait()
	close(errorsFound)
	for err := range errorsFound {
		t.Fatalf("selection failed during updates: %v", err)
	}
}

func TestOptionsValidation(t *testing.T) {
	tests := []Option{
		nil,
		WithEWMAHalfLife(0),
		WithErrorPenalty(-1),
		WithMaxAttempts(0),
		WithRetryableError(nil),
	}
	for index, option := range tests {
		if _, err := New(testFactory(nil), option); err == nil {
			t.Fatalf("option %d did not fail", index)
		}
	}
	if _, err := New(nil); err == nil {
		t.Fatal("nil factory did not fail")
	}
}

func newScoringClient(t *testing.T) *Client {
	t.Helper()
	client, err := New(testFactory(nil))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(10, 0)
	client.now = func() time.Time { return now }
	if err := client.Update([]string{"first", "second"}); err != nil {
		t.Fatal(err)
	}
	return client
}

func testFactory(roundTrip roundTripperFunc) EndpointFactory {
	if roundTrip == nil {
		roundTrip = func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusNoContent, Body: http.NoBody}, nil
		}
	}
	return func(string) (*http.Client, error) {
		return &http.Client{Transport: roundTrip}, nil
	}
}

func testMetrics(latency time.Duration, errorRate float64, now time.Time) *endpointMetrics {
	return &endpointMetrics{
		latency:          float64(latency),
		latencyUpdatedAt: now,
		latencySet:       true,
		errorRate:        errorRate,
		errorUpdatedAt:   now,
		errorSet:         true,
	}
}

func setRandomSequence(client *Client, values ...int) {
	var index int
	client.randomN = func(limit int) int {
		value := values[index%len(values)] % limit
		index++
		return value
	}
}

func isRetryableTestError(err error) bool {
	var target *retryableTestError
	return errors.As(err, &target)
}

func retryGuardClient(t *testing.T, firstError func() error) (*Client, *atomic.Int64) {
	t.Helper()
	secondCalls := new(atomic.Int64)
	client, err := New(func(address string) (*http.Client, error) {
		return &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
			if address == "first" {
				return nil, firstError()
			}
			secondCalls.Add(1)
			return &http.Response{StatusCode: http.StatusNoContent, Body: http.NoBody}, nil
		})}, nil
	}, WithRetryableError(isRetryableTestError))
	if err != nil {
		t.Fatal(err)
	}
	client.randomN = func(int) int { return 0 }
	if err := client.Update([]string{"first", "second"}); err != nil {
		t.Fatal(err)
	}
	return client, secondCalls
}

type errorReadCloser struct {
	err error
}

func (r *errorReadCloser) Read([]byte) (int, error) { return 0, r.err }
func (r *errorReadCloser) Close() error             { return nil }
