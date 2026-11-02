package weaver

import (
	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
)

// NewMeterProvider 创建 OpenTelemetry MeterProvider 并设置为全局 Provider。
//
// Weaver 的耗时直方图在创建时已按语义约定指定秒级分桶，这里不追加默认项；
// 业务通过 sdkmetric.WithReader 接入 Prometheus 或 OTLP exporter，通过
// sdkmetric.WithResource 设置 service.name 等资源属性。
//
// 必须在 weaver.New 之前调用且只调用一次：OpenTelemetry 全局 API 只会把
// 之前创建的指标转发给第一次设置的 Provider，之后替换 Provider 不会迁移已有指标。
func NewMeterProvider(options ...sdkmetric.Option) *sdkmetric.MeterProvider {
	mp := sdkmetric.NewMeterProvider(options...)
	otel.SetMeterProvider(mp)
	return mp
}
