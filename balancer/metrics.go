package balancer

import (
	"math"
	"time"
)

const defaultInitialLatency = 10 * time.Millisecond

type outcome uint8

const (
	outcomeNeutral outcome = iota
	outcomeSuccess
	outcomeFailure
)

type endpointMetrics struct {
	latency          float64
	latencyUpdatedAt time.Time
	latencySet       bool
	errorRate        float64
	errorUpdatedAt   time.Time
	errorSet         bool
}

func newEndpointMetrics(initialLatency time.Duration, now time.Time) *endpointMetrics {
	if initialLatency <= 0 {
		initialLatency = defaultInitialLatency
	}
	return &endpointMetrics{latency: float64(initialLatency), latencyUpdatedAt: now}
}

func (e *endpoint) score(now time.Time, options options) float64 {
	metrics := e.metrics.Load()
	latency := float64(defaultInitialLatency)
	errorRate := 0.0
	if metrics != nil {
		latency = metrics.latency
		if metrics.errorSet {
			errorRate = metrics.errorRate * decay(now.Sub(metrics.errorUpdatedAt), options.ewmaHalfLife)
		}
	}
	if latency < 1 {
		latency = 1
	}
	return (latency + errorRate*float64(options.errorPenalty)) * float64(e.inflight.Load()+1)
}

func (e *endpoint) observe(now time.Time, latency time.Duration, result outcome, halfLife time.Duration) {
	if result == outcomeNeutral {
		return
	}
	for {
		previous := e.metrics.Load()
		if previous == nil {
			previous = newEndpointMetrics(defaultInitialLatency, now)
		}
		next := *previous
		if result == outcomeSuccess {
			if !previous.latencySet {
				next.latency = float64(latency)
				next.latencySet = true
			} else {
				weight := decayWeight(now.Sub(previous.latencyUpdatedAt), halfLife)
				next.latency = previous.latency*(1-weight) + float64(latency)*weight
			}
			next.latencyUpdatedAt = maxTime(now, previous.latencyUpdatedAt)
		}

		sample := 0.0
		if result == outcomeFailure {
			sample = 1
		}
		if !previous.errorSet {
			next.errorRate = sample
			next.errorSet = true
		} else {
			weight := decayWeight(now.Sub(previous.errorUpdatedAt), halfLife)
			next.errorRate = previous.errorRate*(1-weight) + sample*weight
		}
		next.errorUpdatedAt = maxTime(now, previous.errorUpdatedAt)
		if e.metrics.CompareAndSwap(previous, &next) {
			return
		}
	}
}

func decay(elapsed, halfLife time.Duration) float64 {
	if elapsed <= 0 {
		return 1
	}
	return math.Exp(-math.Ln2 * float64(elapsed) / float64(halfLife))
}

func decayWeight(elapsed, halfLife time.Duration) float64 {
	return 1 - decay(elapsed, halfLife)
}

func maxTime(first, second time.Time) time.Time {
	if first.Before(second) {
		return second
	}
	return first
}
