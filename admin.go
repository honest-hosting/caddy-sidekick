package sidekick

import (
	"encoding/json"
	"net/http"

	"github.com/caddyserver/caddy/v2"
	"go.uber.org/zap"
)

func init() {
	caddy.RegisterModule(AdminMetrics{})
}

// AdminMetrics is a Caddy admin module that exposes sidekick metrics
type AdminMetrics struct {
	logger *zap.Logger
}

// CaddyModule returns the Caddy module information.
func (AdminMetrics) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "admin.api.sidekick_metrics",
		New: func() caddy.Module { return new(AdminMetrics) },
	}
}

// Provision sets up the admin metrics handler.
func (am *AdminMetrics) Provision(ctx caddy.Context) error {
	am.logger = ctx.Logger(am)

	// Ensure metrics collector is initialized
	metrics := GetOrCreateGlobalMetrics(am.logger)
	if metrics != nil {
		am.logger.Info("Sidekick metrics admin endpoint provisioned at /metrics/sidekick")
	}

	return nil
}

// Routes returns the routes for the admin API.
func (am *AdminMetrics) Routes() []caddy.AdminRoute {
	// Always return routes when the module is loaded
	return []caddy.AdminRoute{
		{
			Pattern: "/metrics/sidekick",
			Handler: caddy.AdminHandlerFunc(am.serveMetrics),
		},
		{
			Pattern: "/metrics/sidekick/stats",
			Handler: caddy.AdminHandlerFunc(am.serveStats),
		},
	}
}

// serveMetrics serves the Prometheus metrics endpoint
func (am *AdminMetrics) serveMetrics(w http.ResponseWriter, r *http.Request) error {
	if r.Method != http.MethodGet {
		return caddy.APIError{
			HTTPStatus: http.StatusMethodNotAllowed,
			Message:    "method not allowed",
		}
	}

	metrics := GetMetrics()
	if metrics == nil {
		return caddy.APIError{
			HTTPStatus: http.StatusServiceUnavailable,
			Message:    "metrics not initialized",
		}
	}

	// Let the metrics collector serve the Prometheus metrics
	metrics.ServeHTTP(w, r)
	return nil
}

// serveStats serves a JSON endpoint with current statistics
func (am *AdminMetrics) serveStats(w http.ResponseWriter, r *http.Request) error {
	if r.Method != http.MethodGet {
		return caddy.APIError{
			HTTPStatus: http.StatusMethodNotAllowed,
			Message:    "method not allowed",
		}
	}

	metrics := GetMetrics()
	if metrics == nil {
		return caddy.APIError{
			HTTPStatus: http.StatusServiceUnavailable,
			Message:    "metrics not initialized",
		}
	}

	// Get current rates for all cache types
	rates := metrics.GetRates()

	stats := map[string]interface{}{
		"rates": rates,
		"totals": map[string]uint64{
			"total_requests": metrics.totalRequests.Load(),
			"memory_hits":    metrics.memoryHits.Load(),
			"memory_misses":  metrics.memoryMisses.Load(),
			"disk_hits":      metrics.diskHits.Load(),
			"disk_misses":    metrics.diskMisses.Load(),
			"bypasses":       metrics.bypassCount.Load(),
		},
	}

	w.Header().Set("Content-Type", "application/json")
	return json.NewEncoder(w).Encode(stats)
}

// Cleanup cleans up the admin metrics handler
func (am *AdminMetrics) Cleanup() error {
	// Note: We don't clean up global metrics here as they should survive config reloads
	// The global metrics are shared across all instances
	if am.logger != nil {
		am.logger.Debug("Sidekick admin metrics cleanup called")
	}
	return nil
}

// Interface guards
var (
	_ caddy.Module       = (*AdminMetrics)(nil)
	_ caddy.Provisioner  = (*AdminMetrics)(nil)
	_ caddy.AdminRouter  = (*AdminMetrics)(nil)
	_ caddy.CleanerUpper = (*AdminMetrics)(nil)
)
