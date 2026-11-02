package weaver

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func TestNewMeterProviderSetsGlobalProvider(t *testing.T) {
	previous := otel.GetMeterProvider()
	reader := sdkmetric.NewManualReader()
	provider := NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() {
		otel.SetMeterProvider(previous)
		if err := provider.Shutdown(context.Background()); err != nil {
			t.Errorf("关闭 MeterProvider 失败: %v", err)
		}
	})
	if otel.GetMeterProvider() != provider {
		t.Fatal("NewMeterProvider 未设置全局 MeterProvider")
	}

	counter, err := otel.Meter("test").Int64Counter("custom.counter")
	if err != nil {
		t.Fatal(err)
	}
	counter.Add(context.Background(), 1)

	var resources metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &resources); err != nil {
		t.Fatal(err)
	}
	if len(resources.ScopeMetrics) != 1 || resources.ScopeMetrics[0].Metrics[0].Name != "custom.counter" {
		t.Fatalf("reader 收到的指标不符合预期: %#v", resources.ScopeMetrics)
	}
}
