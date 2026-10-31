package sidekick

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	_ "github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	_ "github.com/caddyserver/caddy/v2/modules/metrics"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// withGlobalMetrics swaps the package-global collector for the duration of a test.
func withGlobalMetrics(t *testing.T, m *MetricsCollector) {
	t.Helper()
	globalMetricsMu.Lock()
	prev := globalMetrics
	globalMetrics = m
	globalMetricsMu.Unlock()
	t.Cleanup(func() {
		globalMetricsMu.Lock()
		globalMetrics = prev
		globalMetricsMu.Unlock()
	})
}

func TestMetricsHandler_UnmarshalCaddyfile(t *testing.T) {
	for _, tc := range []struct {
		input   string
		wantErr bool
	}{
		{`sidekick_metrics`, false},
		{`sidekick_metrics extra`, true},
		{"sidekick_metrics {\n\tfoo\n}", true},
	} {
		m := &MetricsHandler{}
		err := m.UnmarshalCaddyfile(caddyfile.NewTestDispenser(tc.input))
		if tc.wantErr {
			assert.Error(t, err, tc.input)
		} else {
			assert.NoError(t, err, tc.input)
		}
	}
}

func TestMetricsHandler_ServeHTTP(t *testing.T) {
	next := caddyhttp.HandlerFunc(func(http.ResponseWriter, *http.Request) error {
		t.Fatal("sidekick_metrics must not call the next handler")
		return nil
	})

	t.Run("metrics disabled", func(t *testing.T) {
		withGlobalMetrics(t, nil)
		w := httptest.NewRecorder()
		require.NoError(t, MetricsHandler{}.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics/sidekick", nil), next))
		assert.Equal(t, http.StatusNotFound, w.Code)
	})

	withGlobalMetrics(t, NewMetricsCollector(zap.NewNop()))

	t.Run("prometheus", func(t *testing.T) {
		w := httptest.NewRecorder()
		require.NoError(t, MetricsHandler{}.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics/sidekick", nil), next))
		assert.Equal(t, http.StatusOK, w.Code)
		assert.Contains(t, w.Body.String(), "caddy_sidekick_cache_used_bytes")
	})

	for _, path := range []string{"/metrics/sidekick/stats", "/metrics/sidekick/stats/"} {
		t.Run("stats "+path, func(t *testing.T) {
			w := httptest.NewRecorder()
			require.NoError(t, MetricsHandler{}.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil), next))
			assert.Equal(t, http.StatusOK, w.Code)
			assert.Equal(t, "application/json", w.Header().Get("Content-Type"))
			var doc map[string]any
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &doc))
			assert.Contains(t, doc, "rates")
			assert.Contains(t, doc, "totals")
		})
	}

	t.Run("non-GET", func(t *testing.T) {
		err := MetricsHandler{}.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/metrics/sidekick", nil), next)
		var herr caddyhttp.HandlerError
		require.ErrorAs(t, err, &herr)
		assert.Equal(t, http.StatusMethodNotAllowed, herr.StatusCode)
	})
}

// TestMetricsHandler_AdminOffServer runs real Caddy with the FrankenWP shape: admin API off
// and a metrics-only server. Caddy's `metrics` handler must serve the same registry the admin
// /metrics endpoint did (Go/process collectors and Caddy's own config metrics), the sidekick
// paths must work, and nothing else (admin routes like /config/ or /load) may answer.
func TestMetricsHandler_AdminOffServer(t *testing.T) {
	withGlobalMetrics(t, NewMetricsCollector(zap.NewNop()))

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	require.NoError(t, ln.Close())

	cfgJSON, warnings, err := caddyconfig.GetAdapter("caddyfile").Adapt([]byte(fmt.Sprintf(`{
	admin off
	persist_config off
	metrics
	servers %[1]s {
		name metrics
	}
}

http://%[1]s {
	route {
		metrics /metrics {
			disable_openmetrics
		}
		sidekick_metrics /metrics/sidekick*
		respond 404
	}
}
`, addr)), nil)
	require.NoError(t, err)
	require.Empty(t, warnings)
	assert.NotContains(t, string(cfgJSON), `"admin":{"listen"`)

	require.NoError(t, caddy.Load(cfgJSON, true))
	t.Cleanup(func() { _ = caddy.Stop() })

	client := &http.Client{Timeout: 5 * time.Second}
	do := func(method, path string) (int, string) {
		t.Helper()
		req, err := http.NewRequest(method, "http://"+addr+path, nil)
		require.NoError(t, err)
		resp, err := client.Do(req)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		return resp.StatusCode, string(body)
	}

	code, body := do(http.MethodGet, "/metrics")
	require.Equal(t, http.StatusOK, code)
	for _, series := range []string{"go_goroutines", "process_cpu_seconds_total", "caddy_config_last_reload_successful"} {
		assert.True(t, strings.Contains(body, series), "/metrics missing %s", series)
	}

	code, body = do(http.MethodGet, "/metrics/sidekick")
	assert.Equal(t, http.StatusOK, code)
	assert.Contains(t, body, "caddy_sidekick_cache_used_bytes")

	code, body = do(http.MethodGet, "/metrics/sidekick/stats")
	assert.Equal(t, http.StatusOK, code)
	assert.Contains(t, body, `"totals"`)

	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/config/"},
		{http.MethodPost, "/load"},
		{http.MethodPost, "/stop"},
		{http.MethodGet, "/reverse_proxy/upstreams"},
		{http.MethodGet, "/"},
	} {
		code, _ := do(tc.method, tc.path)
		assert.Equal(t, http.StatusNotFound, code, "%s %s", tc.method, tc.path)
	}
}
