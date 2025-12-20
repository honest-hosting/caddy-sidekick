package sidekick

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/zap"
)

// MetricsCollector handles all metrics collection for the sidekick plugin
type MetricsCollector struct {
	mu     sync.RWMutex
	logger *zap.Logger

	// Own Prometheus registry
	registry *prometheus.Registry

	// Cache size metrics (bytes)
	cacheUsedBytes  *prometheus.GaugeVec
	cacheLimitBytes *prometheus.GaugeVec

	// Cache count metrics
	cacheUsedCount  *prometheus.GaugeVec
	cacheLimitCount *prometheus.GaugeVec

	// Cache size distribution histogram
	cacheSizeDistribution *prometheus.HistogramVec

	// Request metrics - simplified to track HIT/MISS/BYPASS/TOTAL
	cacheRequests *prometheus.CounterVec

	// Response time histogram
	responseTime *prometheus.HistogramVec

	// Request tracking for all types
	totalRequests atomic.Uint64
	// Memory cache tracking
	memoryHits   atomic.Uint64
	memoryMisses atomic.Uint64
	// Disk cache tracking
	diskHits   atomic.Uint64
	diskMisses atomic.Uint64
	// Bypass tracking
	bypassCount atomic.Uint64

	// Context for lifecycle management
	ctx    context.Context
	cancel context.CancelFunc
}

var (
	// Global metrics instance - survives config reloads
	globalMetrics   *MetricsCollector
	globalMetricsMu sync.RWMutex
)

// Size buckets for distribution (in bytes)
var sizeBuckets = []float64{
	1024,       // 1KB
	4096,       // 4KB
	16384,      // 16KB
	65536,      // 64KB
	262144,     // 256KB
	1048576,    // 1MB
	4194304,    // 4MB
	16777216,   // 16MB
	67108864,   // 64MB
	268435456,  // 256MB
	1073741824, // 1GB
}

// Response time buckets (in milliseconds)
var responseTimeBuckets = []float64{
	0.5, 1, 2, 5, 10, 20, 50, 100, 200, 500, 1000, 2000, 5000,
}

// NewMetricsCollector creates a new metrics collector with its own registry
func NewMetricsCollector(logger *zap.Logger) *MetricsCollector {
	registry := prometheus.NewRegistry()
	ctx, cancel := context.WithCancel(context.Background())

	m := &MetricsCollector{
		logger:   logger,
		registry: registry,
		ctx:      ctx,
		cancel:   cancel,
	}

	// Initialize all metrics with the custom registry
	m.initMetrics()

	return m
}

// initMetrics initializes all Prometheus metrics
func (m *MetricsCollector) initMetrics() {
	// Register with both custom registry and default registry
	// Ignore errors from duplicate registration

	// Cache used bytes
	m.cacheUsedBytes = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "caddy_sidekick_cache_used_bytes",
			Help: "Current cache usage in bytes",
		},
		[]string{"type", "server"},
	)
	m.registry.MustRegister(m.cacheUsedBytes)
	_ = prometheus.DefaultRegisterer.Register(m.cacheUsedBytes) // Ignore duplicate registration

	// Cache limit bytes
	m.cacheLimitBytes = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "caddy_sidekick_cache_limit_bytes",
			Help: "Cache limit in bytes (-1 for unlimited, 0 for disabled)",
		},
		[]string{"type", "server"},
	)
	m.registry.MustRegister(m.cacheLimitBytes)
	_ = prometheus.DefaultRegisterer.Register(m.cacheLimitBytes)

	// Cache used count
	m.cacheUsedCount = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "caddy_sidekick_cache_used_count",
			Help: "Current number of cached items",
		},
		[]string{"type", "server"},
	)
	m.registry.MustRegister(m.cacheUsedCount)
	_ = prometheus.DefaultRegisterer.Register(m.cacheUsedCount)

	// Cache limit count
	m.cacheLimitCount = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "caddy_sidekick_cache_limit_count",
			Help: "Cache item count limit (-1 for unlimited, 0 for disabled)",
		},
		[]string{"type", "server"},
	)
	m.registry.MustRegister(m.cacheLimitCount)
	_ = prometheus.DefaultRegisterer.Register(m.cacheLimitCount)

	// Cache size distribution
	m.cacheSizeDistribution = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "caddy_sidekick_cache_size_distribution_bytes",
			Help:    "Distribution of cached item sizes in bytes",
			Buckets: sizeBuckets,
		},
		[]string{"type", "server"},
	)
	m.registry.MustRegister(m.cacheSizeDistribution)
	_ = prometheus.DefaultRegisterer.Register(m.cacheSizeDistribution)

	// Simplified request metrics
	m.cacheRequests = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "caddy_sidekick_cache_requests_total",
			Help: "Total number of cache requests by result type",
		},
		[]string{"result", "cache_type"},
	)
	m.registry.MustRegister(m.cacheRequests)
	_ = prometheus.DefaultRegisterer.Register(m.cacheRequests)

	// Response time
	m.responseTime = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "caddy_sidekick_response_time_ms",
			Help:    "Response time in milliseconds",
			Buckets: responseTimeBuckets,
		},
		[]string{"cache_status", "server"},
	)
	m.registry.MustRegister(m.responseTime)
	_ = prometheus.DefaultRegisterer.Register(m.responseTime)

	// Set initial values to 0 for all metrics
	for _, t := range []string{"memory", "disk", "total"} {
		m.cacheUsedBytes.WithLabelValues(t, "default").Set(0)
		m.cacheLimitBytes.WithLabelValues(t, "default").Set(0)
		m.cacheUsedCount.WithLabelValues(t, "default").Set(0)
		m.cacheLimitCount.WithLabelValues(t, "default").Set(0)
	}

	// Initialize request counters to 0
	for _, cacheType := range []string{"memory", "disk", "total"} {
		for _, result := range []string{"hit", "miss", "bypass", "total"} {
			m.cacheRequests.WithLabelValues(result, cacheType).Add(0)
		}
	}
}

// GetOrCreateGlobalMetrics returns the global metrics instance, creating it if needed
func GetOrCreateGlobalMetrics(logger *zap.Logger) *MetricsCollector {
	globalMetricsMu.Lock()
	defer globalMetricsMu.Unlock()

	if globalMetrics == nil {
		globalMetrics = NewMetricsCollector(logger)
		if logger != nil {
			logger.Info("Created global sidekick metrics collector")
		}
	}
	return globalMetrics
}

// GetMetrics returns the global metrics instance if it exists
func GetMetrics() *MetricsCollector {
	globalMetricsMu.RLock()
	defer globalMetricsMu.RUnlock()
	return globalMetrics
}

// ServeHTTP serves the Prometheus metrics endpoint
func (m *MetricsCollector) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if m == nil {
		http.Error(w, "Metrics not initialized", http.StatusInternalServerError)
		return
	}

	// Use the default registry which contains our metrics
	handler := promhttp.HandlerFor(
		prometheus.DefaultGatherer,
		promhttp.HandlerOpts{
			EnableOpenMetrics: true,
		},
	)
	handler.ServeHTTP(w, r)
}

// Cleanup stops the metrics collector and cleans up resources
func (m *MetricsCollector) Cleanup() {
	if m.cancel != nil {
		m.cancel()
	}

	if m.logger != nil {
		m.logger.Info("Sidekick metrics collector cleaned up")
	}
}

// UpdateCacheStats updates cache statistics
func (m *MetricsCollector) UpdateCacheStats(serverName string, memStats, diskStats *CacheStats) {
	if m == nil {
		return
	}

	m.mu.RLock()
	defer m.mu.RUnlock()

	if m.cacheUsedBytes != nil && memStats != nil {
		// Memory metrics
		m.cacheUsedBytes.WithLabelValues("memory", serverName).Set(float64(memStats.UsedBytes))
		m.cacheLimitBytes.WithLabelValues("memory", serverName).Set(float64(memStats.LimitBytes))
		m.cacheUsedCount.WithLabelValues("memory", serverName).Set(float64(memStats.UsedCount))
		m.cacheLimitCount.WithLabelValues("memory", serverName).Set(float64(memStats.LimitCount))
	}

	if m.cacheUsedBytes != nil && diskStats != nil {
		// Disk metrics
		m.cacheUsedBytes.WithLabelValues("disk", serverName).Set(float64(diskStats.UsedBytes))
		m.cacheLimitBytes.WithLabelValues("disk", serverName).Set(float64(diskStats.LimitBytes))
		m.cacheUsedCount.WithLabelValues("disk", serverName).Set(float64(diskStats.UsedCount))
		m.cacheLimitCount.WithLabelValues("disk", serverName).Set(float64(diskStats.LimitCount))
	}

	// Total metrics (memory + disk)
	if m.cacheUsedBytes != nil && memStats != nil && diskStats != nil {
		totalUsedBytes := memStats.UsedBytes + diskStats.UsedBytes
		totalLimitBytes := memStats.LimitBytes + diskStats.LimitBytes
		totalUsedCount := memStats.UsedCount + diskStats.UsedCount
		totalLimitCount := memStats.LimitCount + diskStats.LimitCount

		m.cacheUsedBytes.WithLabelValues("total", serverName).Set(float64(totalUsedBytes))
		m.cacheLimitBytes.WithLabelValues("total", serverName).Set(float64(totalLimitBytes))
		m.cacheUsedCount.WithLabelValues("total", serverName).Set(float64(totalUsedCount))
		m.cacheLimitCount.WithLabelValues("total", serverName).Set(float64(totalLimitCount))
	}
}

// RecordCacheOperation records a cache operation with simplified metrics
func (m *MetricsCollector) RecordCacheOperation(operation, status, serverName string) {
	if m == nil {
		return
	}

	// Always increment total requests
	m.totalRequests.Add(1)

	m.mu.RLock()
	defer m.mu.RUnlock()

	// Map old operation/status to new simplified metrics
	switch operation {
	case "get":
		switch status {
		case "hit":
			// Determine if it's memory or disk based on serverName
			switch serverName {
			case "memory":
				m.memoryHits.Add(1)
				if m.cacheRequests != nil {
					m.cacheRequests.WithLabelValues("hit", "memory").Inc()
					m.cacheRequests.WithLabelValues("hit", "total").Inc()
					m.cacheRequests.WithLabelValues("total", "total").Inc()
				}
			case "disk":
				m.diskHits.Add(1)
				if m.cacheRequests != nil {
					m.cacheRequests.WithLabelValues("hit", "disk").Inc()
					m.cacheRequests.WithLabelValues("hit", "total").Inc()
					m.cacheRequests.WithLabelValues("total", "total").Inc()
				}
			default:
				// Default/unknown - count as total hit
				if m.cacheRequests != nil {
					m.cacheRequests.WithLabelValues("hit", "total").Inc()
					m.cacheRequests.WithLabelValues("total", "total").Inc()
				}
			}
		case "miss":
			// A miss means it wasn't in either cache
			m.memoryMisses.Add(1)
			m.diskMisses.Add(1)
			if m.cacheRequests != nil {
				m.cacheRequests.WithLabelValues("miss", "memory").Inc()
				m.cacheRequests.WithLabelValues("miss", "disk").Inc()
				m.cacheRequests.WithLabelValues("miss", "total").Inc()
				m.cacheRequests.WithLabelValues("total", "total").Inc()
			}
		}
	case "bypass":
		m.bypassCount.Add(1)
		if m.cacheRequests != nil {
			// Bypass affects all cache types
			m.cacheRequests.WithLabelValues("bypass", "memory").Inc()
			m.cacheRequests.WithLabelValues("bypass", "disk").Inc()
			m.cacheRequests.WithLabelValues("bypass", "total").Inc()
			m.cacheRequests.WithLabelValues("total", "total").Inc()
		}
	case "store":
		// Store operations don't affect request counts
		return
	case "purge":
		// Purge operations don't affect request counts
		return
	}
}

// RecordResponseTime records the response time for a request
func (m *MetricsCollector) RecordResponseTime(cacheStatus, serverName string, duration time.Duration) {
	if m == nil {
		return
	}

	m.mu.RLock()
	defer m.mu.RUnlock()

	if m.responseTime != nil {
		m.responseTime.WithLabelValues(cacheStatus, serverName).Observe(float64(duration.Milliseconds()))
	}
}

// RecordItemSize records the size of a cached item for distribution analysis
func (m *MetricsCollector) RecordItemSize(itemType, serverName string, size int64) {
	if m == nil {
		return
	}

	m.mu.RLock()
	defer m.mu.RUnlock()

	if m.cacheSizeDistribution != nil {
		m.cacheSizeDistribution.WithLabelValues(itemType, serverName).Observe(float64(size))
	}
}

// GetRates returns the current hit, miss, and bypass rates for each cache type
func (m *MetricsCollector) GetRates() map[string]map[string]float64 {
	total := m.totalRequests.Load()
	if total == 0 {
		return map[string]map[string]float64{
			"memory": {"hit": 0, "miss": 0, "bypass": 0, "total": 0},
			"disk":   {"hit": 0, "miss": 0, "bypass": 0, "total": 0},
			"total":  {"hit": 0, "miss": 0, "bypass": 0, "total": 0},
		}
	}

	memHits := m.memoryHits.Load()
	memMisses := m.memoryMisses.Load()
	diskHits := m.diskHits.Load()
	diskMisses := m.diskMisses.Load()
	bypasses := m.bypassCount.Load()

	totalHits := memHits + diskHits
	totalMisses := memMisses // Misses are counted once for both

	rates := map[string]map[string]float64{
		"memory": {
			"hit":    float64(memHits) / float64(total) * 100,
			"miss":   float64(memMisses) / float64(total) * 100,
			"bypass": float64(bypasses) / float64(total) * 100,
			"total":  100.0,
		},
		"disk": {
			"hit":    float64(diskHits) / float64(total) * 100,
			"miss":   float64(diskMisses) / float64(total) * 100,
			"bypass": float64(bypasses) / float64(total) * 100,
			"total":  100.0,
		},
		"total": {
			"hit":    float64(totalHits) / float64(total) * 100,
			"miss":   float64(totalMisses) / float64(total) * 100,
			"bypass": float64(bypasses) / float64(total) * 100,
			"total":  100.0,
		},
	}

	return rates
}

// StartMetricsUpdater starts a goroutine that periodically updates metrics
func (m *MetricsCollector) StartMetricsUpdater(storage *Storage, serverName string) {
	if m == nil {
		return
	}

	go func() {
		ticker := time.NewTicker(575 * time.Millisecond) // Update every 1 second for faster testing
		defer ticker.Stop()

		for {
			select {
			case <-m.ctx.Done():
				return
			case <-ticker.C:
				m.updateStorageMetrics(storage, serverName)
			}
		}
	}()
}

// updateStorageMetrics updates storage-related metrics
func (m *MetricsCollector) updateStorageMetrics(storage *Storage, serverName string) {
	if storage == nil {
		return
	}

	// Collect memory cache stats
	memStats := &CacheStats{}
	if memCache := storage.GetMemCache(); memCache != nil {
		memStats.UsedBytes = int64(memCache.Cost())
		memStats.UsedCount = int64(memCache.Size())
		memStats.LimitBytes = int64(storage.memMaxSize)
		memStats.LimitCount = int64(storage.memMaxCount)

		// Set to 0 if disabled, -1 if unlimited
		if storage.memMaxSize == 0 {
			memStats.LimitBytes = 0
		} else if storage.memMaxSize < 0 {
			memStats.LimitBytes = -1
		}

		if storage.memMaxCount == 0 {
			memStats.LimitCount = 0
		} else if storage.memMaxCount < 0 {
			memStats.LimitCount = -1
		}
	}

	// Collect disk cache stats
	diskStats := &CacheStats{}
	if diskCache := storage.GetDiskCache(); diskCache != nil {
		itemCount, diskUsage := diskCache.Stats()
		diskStats.UsedBytes = diskUsage
		diskStats.UsedCount = itemCount
		diskStats.LimitBytes = int64(storage.diskMaxSize)
		diskStats.LimitCount = int64(storage.diskMaxCount)

		// Set to 0 if disabled, -1 if unlimited
		if storage.diskMaxSize == 0 {
			diskStats.LimitBytes = 0
		} else if storage.diskMaxSize < 0 {
			diskStats.LimitBytes = -1
		}

		if storage.diskMaxCount == 0 {
			diskStats.LimitCount = 0
		} else if storage.diskMaxCount < 0 {
			diskStats.LimitCount = -1
		}
	}

	// Update metrics
	m.UpdateCacheStats(serverName, memStats, diskStats)
}

// CacheStats holds cache statistics
type CacheStats struct {
	UsedBytes  int64
	LimitBytes int64 // -1 for unlimited, 0 for disabled
	UsedCount  int64
	LimitCount int64 // -1 for unlimited, 0 for disabled
}

// MetricsMiddleware wraps an HTTP handler to collect metrics
func (m *MetricsCollector) MetricsMiddleware(next func(w http.ResponseWriter, r *http.Request) error, serverName string) func(w http.ResponseWriter, r *http.Request) error {
	return func(w http.ResponseWriter, r *http.Request) error {
		if m == nil {
			return next(w, r)
		}

		start := time.Now()

		// Call the next handler
		err := next(w, r)

		// Record response time
		duration := time.Since(start)

		// Determine cache status from response headers or context
		cacheStatus := "unknown"
		if w.Header().Get(CacheHeaderName) != "" {
			status := w.Header().Get(CacheHeaderName)
			switch status {
			case "HIT":
				cacheStatus = "hit"
			case "MISS":
				cacheStatus = "miss"
			case "BYPASS":
				cacheStatus = "bypass"
			default:
				cacheStatus = status
			}
		}

		m.RecordResponseTime(cacheStatus, serverName, duration)

		return err
	}
}

// Reset resets all metrics (useful for testing)
func (m *MetricsCollector) Reset() {
	m.totalRequests.Store(0)
	m.memoryHits.Store(0)
	m.memoryMisses.Store(0)
	m.diskHits.Store(0)
	m.diskMisses.Store(0)
	m.bypassCount.Store(0)
}
