package sidekick

import (
	"net/http"
	"strings"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
)

func init() {
	caddy.RegisterModule(MetricsHandler{})
	httpcaddyfile.RegisterHandlerDirective("sidekick_metrics", parseMetricsHandler)
	httpcaddyfile.RegisterDirectiveOrder("sidekick_metrics", httpcaddyfile.Before, "respond")
}

// MetricsHandler serves the sidekick metrics from an ordinary HTTP route, so they can be
// exposed without enabling Caddy's admin API (which also serves /config and /load):
//
//	http://:2019 {
//		route {
//			metrics /metrics
//			sidekick_metrics /metrics/sidekick*
//			respond 404
//		}
//	}
//
// A request path ending in "/stats" gets the JSON stats document (same as the admin
// endpoint /metrics/sidekick/stats); any other matched path gets the Prometheus metrics
// (same as /metrics/sidekick). Requires `metrics` in a sidekick block, as the admin
// endpoints do.
type MetricsHandler struct{}

// CaddyModule returns the Caddy module information.
func (MetricsHandler) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.handlers.sidekick_metrics",
		New: func() caddy.Module { return new(MetricsHandler) },
	}
}

// ServeHTTP implements caddyhttp.MiddlewareHandler. It always answers; it never calls next.
func (MetricsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request, _ caddyhttp.Handler) error {
	if r.Method != http.MethodGet {
		return caddyhttp.Error(http.StatusMethodNotAllowed, nil)
	}

	metrics := GetMetrics()
	if metrics == nil {
		http.Error(w, "metrics not enabled - add 'metrics /metrics/sidekick' to your sidekick configuration", http.StatusNotFound)
		return nil
	}

	if strings.HasSuffix(strings.TrimSuffix(r.URL.Path, "/"), "/stats") {
		return writeStats(w, metrics)
	}

	metrics.ServeHTTP(w, r)
	return nil
}

// UnmarshalCaddyfile sets up the handler from Caddyfile tokens. Syntax:
//
//	sidekick_metrics [<matcher>]
func (m *MetricsHandler) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	d.Next() // consume directive name
	if d.NextArg() {
		return d.ArgErr()
	}
	if d.NextBlock(0) {
		return d.Errf("sidekick_metrics takes no options")
	}
	return nil
}

func parseMetricsHandler(h httpcaddyfile.Helper) (caddyhttp.MiddlewareHandler, error) {
	m := new(MetricsHandler)
	if err := m.UnmarshalCaddyfile(h.Dispenser); err != nil {
		return nil, err
	}
	return m, nil
}

// Interface guards
var (
	_ caddyhttp.MiddlewareHandler = (*MetricsHandler)(nil)
	_ caddyfile.Unmarshaler       = (*MetricsHandler)(nil)
)
