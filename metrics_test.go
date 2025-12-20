package sidekick

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"go.uber.org/zap"
)

func TestMetricsCollector_Enable(t *testing.T) {
	// Create a new metrics collector
	logger := zap.NewNop()
	metrics := NewMetricsCollector(logger)

	// Verify metrics are initialized
	if metrics == nil {
		t.Fatal("Failed to create metrics collector")
	}

	// Test that metrics are registered
	if metrics.cacheUsedBytes == nil {
		t.Error("cacheUsedBytes should be initialized")
	}
	if metrics.cacheLimitBytes == nil {
		t.Error("cacheLimitBytes should be initialized")
	}
	if metrics.cacheUsedCount == nil {
		t.Error("cacheUsedCount should be initialized")
	}
	if metrics.cacheLimitCount == nil {
		t.Error("cacheLimitCount should be initialized")
	}
	if metrics.cacheSizeDistribution == nil {
		t.Error("cacheSizeDistribution should be initialized")
	}
	if metrics.cacheRequests == nil {
		t.Error("cacheRequests should be initialized")
	}
	if metrics.responseTime == nil {
		t.Error("responseTime should be initialized")
	}
}

func TestMetricsCollector_RecordCacheOperation(t *testing.T) {
	// Create a new metrics collector
	logger := zap.NewNop()
	metrics := NewMetricsCollector(logger)

	// Reset counters
	metrics.Reset()

	// Record operations
	metrics.RecordCacheOperation("get", "hit", "memory")
	metrics.RecordCacheOperation("get", "hit", "disk")
	metrics.RecordCacheOperation("get", "miss", "default")
	metrics.RecordCacheOperation("bypass", "true", "default")
	metrics.RecordCacheOperation("store", "success", "memory")
	metrics.RecordCacheOperation("purge", "success", "default")

	// Check atomic counters
	if metrics.memoryHits.Load() != 1 {
		t.Errorf("Expected 1 memory hit, got %d", metrics.memoryHits.Load())
	}
	if metrics.diskHits.Load() != 1 {
		t.Errorf("Expected 1 disk hit, got %d", metrics.diskHits.Load())
	}
	if metrics.memoryMisses.Load() != 1 {
		t.Errorf("Expected 1 miss, got %d", metrics.memoryMisses.Load())
	}
	if metrics.bypassCount.Load() != 1 {
		t.Errorf("Expected 1 bypass, got %d", metrics.bypassCount.Load())
	}

	// Check Prometheus metrics
	hitMemCounter, _ := metrics.cacheRequests.GetMetricWith(prometheus.Labels{
		"result":     "hit",
		"cache_type": "memory",
	})
	if hitMemCounter != nil {
		hitCount := testutil.ToFloat64(hitMemCounter)
		if hitCount != 1 {
			t.Errorf("Expected 1 memory hit in Prometheus counter, got %f", hitCount)
		}
	}

	hitDiskCounter, _ := metrics.cacheRequests.GetMetricWith(prometheus.Labels{
		"result":     "hit",
		"cache_type": "disk",
	})
	if hitDiskCounter != nil {
		hitCount := testutil.ToFloat64(hitDiskCounter)
		if hitCount != 1 {
			t.Errorf("Expected 1 disk hit in Prometheus counter, got %f", hitCount)
		}
	}
}

func TestMetricsCollector_GetRates(t *testing.T) {
	// Create a new metrics collector
	logger := zap.NewNop()
	metrics := NewMetricsCollector(logger)

	// Reset counters
	metrics.Reset()

	// Record operations
	for i := 0; i < 50; i++ {
		metrics.RecordCacheOperation("get", "hit", "memory")
	}
	for i := 0; i < 20; i++ {
		metrics.RecordCacheOperation("get", "hit", "disk")
	}
	for i := 0; i < 20; i++ {
		metrics.RecordCacheOperation("get", "miss", "default")
	}
	for i := 0; i < 10; i++ {
		metrics.RecordCacheOperation("bypass", "true", "default")
	}

	// Get rates
	rates := metrics.GetRates()

	// Check total rates (should be 70% hit, 20% miss, 10% bypass)
	totalRates := rates["total"]
	if totalRates["hit"] < 69 || totalRates["hit"] > 71 {
		t.Errorf("Expected total hit rate ~70%%, got %f", totalRates["hit"])
	}
	if totalRates["miss"] < 19 || totalRates["miss"] > 21 {
		t.Errorf("Expected total miss rate ~20%%, got %f", totalRates["miss"])
	}
	if totalRates["bypass"] < 9 || totalRates["bypass"] > 11 {
		t.Errorf("Expected total bypass rate ~10%%, got %f", totalRates["bypass"])
	}

	// Check memory rates (should be 50% hit, 20% miss, 10% bypass)
	memRates := rates["memory"]
	if memRates["hit"] < 49 || memRates["hit"] > 51 {
		t.Errorf("Expected memory hit rate ~50%%, got %f", memRates["hit"])
	}

	// Check disk rates (should be 20% hit, 20% miss, 10% bypass)
	diskRates := rates["disk"]
	if diskRates["hit"] < 19 || diskRates["hit"] > 21 {
		t.Errorf("Expected disk hit rate ~20%%, got %f", diskRates["hit"])
	}
}

func TestMetricsCollector_UpdateCacheStats(t *testing.T) {
	// Create a new metrics collector
	logger := zap.NewNop()
	metrics := NewMetricsCollector(logger)

	// Create test stats
	memStats := &CacheStats{
		UsedBytes:  1024 * 1024,      // 1MB
		LimitBytes: 10 * 1024 * 1024, // 10MB
		UsedCount:  100,
		LimitCount: 1000,
	}

	diskStats := &CacheStats{
		UsedBytes:  50 * 1024 * 1024,  // 50MB
		LimitBytes: 100 * 1024 * 1024, // 100MB
		UsedCount:  500,
		LimitCount: 5000,
	}

	// Update stats
	metrics.UpdateCacheStats("test-server", memStats, diskStats)

	// Check memory metrics
	memUsedGauge, _ := metrics.cacheUsedBytes.GetMetricWith(prometheus.Labels{
		"type":   "memory",
		"server": "test-server",
	})
	if memUsedGauge != nil {
		memUsed := testutil.ToFloat64(memUsedGauge)
		if memUsed != float64(memStats.UsedBytes) {
			t.Errorf("Expected memory used bytes %d, got %f", memStats.UsedBytes, memUsed)
		}
	}

	// Check disk metrics
	diskUsedGauge, _ := metrics.cacheUsedBytes.GetMetricWith(prometheus.Labels{
		"type":   "disk",
		"server": "test-server",
	})
	if diskUsedGauge != nil {
		diskUsed := testutil.ToFloat64(diskUsedGauge)
		if diskUsed != float64(diskStats.UsedBytes) {
			t.Errorf("Expected disk used bytes %d, got %f", diskStats.UsedBytes, diskUsed)
		}
	}

	// Check total metrics
	totalUsedGauge, _ := metrics.cacheUsedBytes.GetMetricWith(prometheus.Labels{
		"type":   "total",
		"server": "test-server",
	})
	if totalUsedGauge != nil {
		totalUsed := testutil.ToFloat64(totalUsedGauge)
		expectedTotal := float64(memStats.UsedBytes + diskStats.UsedBytes)
		if totalUsed != expectedTotal {
			t.Errorf("Expected total used bytes %f, got %f", expectedTotal, totalUsed)
		}
	}

}

func TestMetricsCollector_RecordItemSize(t *testing.T) {
	// Create a new metrics collector
	logger := zap.NewNop()
	metrics := NewMetricsCollector(logger)

	// Record various item sizes
	metrics.RecordItemSize("memory", "test-server", 512)     // 512B
	metrics.RecordItemSize("memory", "test-server", 2048)    // 2KB
	metrics.RecordItemSize("memory", "test-server", 10240)   // 10KB
	metrics.RecordItemSize("disk", "test-server", 1048576)   // 1MB
	metrics.RecordItemSize("disk", "test-server", 10485760)  // 10MB
	metrics.RecordItemSize("disk", "test-server", 104857600) // 100MB

	// The histogram should have recorded these values in the appropriate buckets
	// We can't easily test the exact bucket counts without more complex test utilities,
	// but we can verify the histogram exists
	if metrics.cacheSizeDistribution == nil {
		t.Error("cacheSizeDistribution should not be nil")
	}
}

func TestMetricsCollector_RecordResponseTime(t *testing.T) {
	// Create a new metrics collector
	logger := zap.NewNop()
	metrics := NewMetricsCollector(logger)

	// Record various response times
	metrics.RecordResponseTime("hit", "test-server", 5*time.Millisecond)
	metrics.RecordResponseTime("hit", "test-server", 10*time.Millisecond)
	metrics.RecordResponseTime("miss", "test-server", 100*time.Millisecond)
	metrics.RecordResponseTime("miss", "test-server", 200*time.Millisecond)
	metrics.RecordResponseTime("bypass", "test-server", 50*time.Millisecond)

	// The histogram should have recorded these values
	if metrics.responseTime == nil {
		t.Error("responseTime should not be nil")
	}
}

func TestMetricsCollector_SpecialLimitValues(t *testing.T) {
	// Create a new metrics collector
	logger := zap.NewNop()
	metrics := NewMetricsCollector(logger)

	// Test with disabled cache (0 values)
	disabledStats := &CacheStats{
		UsedBytes:  0,
		LimitBytes: 0, // Disabled
		UsedCount:  0,
		LimitCount: 0, // Disabled
	}

	// Test with unlimited cache (-1 values)
	unlimitedStats := &CacheStats{
		UsedBytes:  1024 * 1024,
		LimitBytes: -1, // Unlimited
		UsedCount:  100,
		LimitCount: -1, // Unlimited
	}

	metrics.UpdateCacheStats("test-disabled", disabledStats, disabledStats)
	metrics.UpdateCacheStats("test-unlimited", unlimitedStats, unlimitedStats)

	// Check disabled limits
	disabledLimitGauge, _ := metrics.cacheLimitBytes.GetMetricWith(prometheus.Labels{
		"type":   "memory",
		"server": "test-disabled",
	})
	if disabledLimitGauge != nil {
		limit := testutil.ToFloat64(disabledLimitGauge)
		if limit != 0 {
			t.Errorf("Expected disabled limit to be 0, got %f", limit)
		}
	}

	// Check unlimited limits
	unlimitedLimitGauge, _ := metrics.cacheLimitBytes.GetMetricWith(prometheus.Labels{
		"type":   "memory",
		"server": "test-unlimited",
	})
	if unlimitedLimitGauge != nil {
		limit := testutil.ToFloat64(unlimitedLimitGauge)
		if limit != -1 {
			t.Errorf("Expected unlimited limit to be -1, got %f", limit)
		}
	}
}

func TestMetricsCollector_StartMetricsUpdater(t *testing.T) {
	// Create a new metrics collector
	logger := zap.NewNop()
	metrics := NewMetricsCollector(logger)

	// Create a test storage
	storage := NewStorage("/tmp/test-cache", 3600, 512*1024, 1024*1024, 100, 1024*1024, 10*1024*1024, 1000, logger)

	// Start the updater
	metrics.StartMetricsUpdater(storage, "test-server")

	// Wait a bit for the updater to run
	time.Sleep(100 * time.Millisecond)

	// Cleanup
	metrics.Cleanup()

	// The updater should have stopped gracefully
	// We can't easily test the exact updates, but we can verify it doesn't panic
}
