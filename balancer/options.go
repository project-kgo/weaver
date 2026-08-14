package balancer

import (
	"fmt"
	"time"
)

const (
	defaultEWMAHalfLife = 10 * time.Second
	defaultErrorPenalty = time.Second
	defaultMaxAttempts  = 2
)

type options struct {
	ewmaHalfLife time.Duration
	errorPenalty time.Duration
	maxAttempts  int
	retryable    func(error) bool
}

func defaultOptions() options {
	return options{
		ewmaHalfLife: defaultEWMAHalfLife,
		errorPenalty: defaultErrorPenalty,
		maxAttempts:  defaultMaxAttempts,
	}
}

// Option 调整 Client 的选择和重试行为。
type Option func(*options) error

// WithEWMAHalfLife 设置延迟与错误率 EWMA 的半衰期。
func WithEWMAHalfLife(value time.Duration) Option {
	return func(options *options) error {
		if value <= 0 {
			return fmt.Errorf("balancer: EWMA 半衰期必须大于 0")
		}
		options.ewmaHalfLife = value
		return nil
	}
}

// WithErrorPenalty 设置错误率为 100% 时附加到 endpoint 延迟的惩罚。
func WithErrorPenalty(value time.Duration) Option {
	return func(options *options) error {
		if value < 0 {
			return fmt.Errorf("balancer: 错误惩罚不能小于 0")
		}
		options.errorPenalty = value
		return nil
	}
}

// WithMaxAttempts 设置一次请求最多尝试的不同 endpoint 数量。
// 只有 WithRetryableError 识别的安全错误才会触发后续尝试。
func WithMaxAttempts(value int) Option {
	return func(options *options) error {
		if value < 1 {
			return fmt.Errorf("balancer: 最大尝试次数必须大于 0")
		}
		options.maxAttempts = value
		return nil
	}
}

// WithRetryableError 设置可以安全切换 endpoint 重放请求的错误判断函数。
// 调用方必须仅对能确认请求尚未送达服务端的错误返回 true。
func WithRetryableError(predicate func(error) bool) Option {
	return func(options *options) error {
		if predicate == nil {
			return fmt.Errorf("balancer: 可重试错误判断函数不能为 nil")
		}
		options.retryable = predicate
		return nil
	}
}
