package sidekick

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// TestClassifyQuery_Defaults walks every class with the default lists (nil = default).
func TestClassifyQuery_Defaults(t *testing.T) {
	s := &Sidekick{}

	tests := []struct {
		name      string
		query     string
		canonical string
		reason    bypassReason
		readOnly  bool
	}{
		{name: "no query", query: ""},
		{name: "keyed", query: "p=5", canonical: "p=5"},
		{name: "keyed, sorted by name", query: "page=2&p=5", canonical: "p=5&page=2"},
		{name: "every value kept, in order", query: "s=a&s=b", canonical: "s=a&s=b"},
		{name: "empty value is not absent", query: "s=", canonical: "s="},
		{name: "core query var", query: "page_id=2", canonical: "page_id=2"},
		{name: "asset buster", query: "ver=6.8.1", canonical: "ver=6.8.1"},
		{name: "campaign label keyed by prefix", query: "utm_campaign=spring", canonical: "utm_campaign=spring"},
		{name: "click id left out of the key", query: "gclid=Cj0KCQjw-_x", canonical: ""},
		{name: "click id with a markup value is read-only", query: "gclid=%3Cscript%3E", readOnly: true},
		{name: "click id with a space is read-only", query: "fbclid=a+b", readOnly: true},
		{name: "ga linker value passes the gate", query: "_gl=1*abc*_ga*MTIz", canonical: ""},
		{name: "mixed keyed, label and click id", query: "page=2&utm_source=x&gclid=abc", canonical: "page=2&utm_source=x"},
		{name: "unknown parameter bypasses", query: "foo=1", reason: bypassQuery},
		{name: "unknown wins over keyed, key still built", query: "foo=1&p=2", canonical: "p=2", reason: bypassQuery},
		{name: "array parameter bypasses", query: "s[]=x", reason: bypassQuery},
		{name: "REST is private", query: "rest_route=/", reason: bypassPrivate},
		{name: "two spellings of one PHP name", query: "product.cat=1&product_cat=2", reason: bypassPrivate},
		{name: "semicolon Go rejects", query: "p=1;x=2", reason: bypassPrivate},
		{name: "malformed escape Go rejects", query: "p=%zz", reason: bypassPrivate},
		{name: "a name PHP drops is not keyed", query: "%00x=1", canonical: ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			qc := s.classifyQuery(tc.query)
			assert.Equal(t, tc.reason, qc.Reason, "reason")
			assert.Equal(t, tc.readOnly, qc.ReadOnly, "read-only")
			if tc.reason != bypassPrivate {
				assert.Equal(t, tc.canonical, qc.Canonical, "canonical")
			}
		})
	}

	// The first offending parameter in name order is reported, whatever the request order.
	assert.Equal(t, "bar", s.classifyQuery("foo=1&bar=2&p=1").Param)
	assert.Equal(t, "bar", s.classifyQuery("p=1&bar=2&foo=1").Param)
}

func TestClassifyQuery_Configured(t *testing.T) {
	t.Run("explicit lists and prefixes", func(t *testing.T) {
		s := &Sidekick{CacheKeyQueries: []string{"color", "filter_*"}, CacheIgnoreQueries: []string{"ref"}}
		assert.Equal(t, "color=red&filter_size=m", s.classifyQuery("filter_size=m&color=red").Canonical)
		assert.Equal(t, bypassNone, s.classifyQuery("ref=abc").Reason)
		assert.Equal(t, bypassQuery, s.classifyQuery("p=1").Reason, "a replaced list drops the defaults")
	})

	t.Run("keyed wins over ignored", func(t *testing.T) {
		s := &Sidekick{CacheKeyQueries: []string{"gclid"}}
		assert.Equal(t, "gclid=abc", s.classifyQuery("gclid=abc").Canonical)
	})

	t.Run("star keys everything except ignored", func(t *testing.T) {
		s := &Sidekick{CacheKeyQueries: []string{"*"}}
		qc := s.classifyQuery("zzz=1&gclid=abc&a=2")
		assert.Equal(t, bypassNone, qc.Reason)
		assert.Equal(t, "a=2&zzz=1", qc.Canonical)
		assert.Equal(t, bypassPrivate, s.classifyQuery("rest_route=/").Reason, "nocache_query still wins")
	})

	t.Run("explicitly empty keyed list", func(t *testing.T) {
		s := &Sidekick{CacheKeyQueries: []string{}}
		assert.Equal(t, bypassQuery, s.classifyQuery("p=1").Reason)
		assert.Equal(t, bypassNone, s.classifyQuery("gclid=abc").Reason)
	})
}

func TestClickIDValueOK(t *testing.T) {
	for _, ok := range []string{"", "Cj0KCQjw-_x", "IwAR3abc.def", "1*abc*_ga*MTIz", "abc==", strings.Repeat("a", 512)} {
		assert.True(t, clickIDValueOK(ok), ok)
	}
	for _, bad := range []string{"<script>", `a"b`, "a'b", "a b", "a/b", "a+b", "a%b", "a\x00", strings.Repeat("a", 513)} {
		assert.False(t, clickIDValueOK(bad), bad)
	}
}

// TestBuildCacheKey_QueryAndFraming: distinct requests as PHP sees them never share a
// key, and component boundaries cannot be forged.
func TestBuildCacheKey_QueryAndFraming(t *testing.T) {
	s := &Sidekick{CacheKeyHeaders: []string{"Accept-Encoding"}, logger: zap.NewNop()}
	key := func(target string, hdrs ...string) string {
		req := httptest.NewRequest("GET", target, nil)
		for i := 0; i+1 < len(hdrs); i += 2 {
			req.Header.Set(hdrs[i], hdrs[i+1])
		}
		k, _ := s.buildCacheKey(req)
		return k
	}

	assert.NotEqual(t, key("/"), key("/?s="), "an empty search is not the home page")
	assert.NotEqual(t, key("/"), key("/?page_id=2"))
	assert.NotEqual(t, key("/?s=a"), key("/?s=a&s=b"))
	assert.Equal(t, key("/?p=1&page=2"), key("/?page=2&p=1"))
	assert.Equal(t, key("/landing/"), key("/landing/?gclid=abc"), "click IDs share the page's entry")
	assert.NotEqual(t, key("/landing/"), key("/landing/?utm_campaign=x"))

	star := &Sidekick{CacheKeyQueries: []string{"*"}, logger: zap.NewNop()}
	k1, _ := star.buildCacheKey(httptest.NewRequest("GET", "/a?b=1", nil))
	k2, _ := star.buildCacheKey(httptest.NewRequest("GET", "/ab=1", nil))
	assert.NotEqual(t, k1, k2, "path and query boundary")

	assert.NotEqual(t, key("/", "Accept-Encoding", "gzip"), key("/Accept-Encoding:gzip"), "path and header boundary")
}

// pageOrigin answers like WordPress for an ordinary page. Its body names the path and
// query it rendered, so a test can tell which request's response came back.
func pageOrigin(setCookie, cacheControl string, status int) caddyhttp.Handler {
	return caddyhttp.HandlerFunc(func(w http.ResponseWriter, r *http.Request) error {
		w.Header().Set("Content-Type", "text/html")
		if setCookie != "" {
			w.Header().Set("Set-Cookie", setCookie)
		}
		if cacheControl != "" {
			w.Header().Set("Cache-Control", cacheControl)
		}
		if status == http.StatusServiceUnavailable {
			w.Header().Set("Retry-After", "30")
		}
		w.WriteHeader(status)
		_, err := fmt.Fprintf(w, "<html>%s?%s</html>", r.URL.Path, r.URL.RawQuery)
		return err
	})
}

func serveWith(t *testing.T, s *Sidekick, origin caddyhttp.Handler, target string) (*http.Response, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	require.NoError(t, s.ServeHTTP(rec, httptest.NewRequest("GET", target, nil), origin))
	resp := rec.Result()
	return resp, string(readAll(t, resp))
}

// TestQueryParameterCannotFillAnotherURLsEntry: a parameter outside the key must not
// store its render under the key of the page without it.
func TestQueryParameterCannotFillAnotherURLsEntry(t *testing.T) {
	s, _ := rangeTestSidekick(t)
	origin := pageOrigin("", "", http.StatusOK)

	resp, _ := serveWith(t, s, origin, "/about/?unrecognized=1")
	assert.Equal(t, "BYPASS", resp.Header.Get(CacheHeaderName))

	resp, body := serveWith(t, s, origin, "/about/")
	assert.Equal(t, "MISS", resp.Header.Get(CacheHeaderName))
	assert.Equal(t, "<html>/about/?</html>", body)

	// A keyed core var gets its own entry instead of sharing the page's.
	serveWith(t, s, origin, "/?page_id=2")
	resp, body = serveWith(t, s, origin, "/")
	assert.Equal(t, "MISS", resp.Header.Get(CacheHeaderName))
	assert.Equal(t, "<html>/?</html>", body)
}

// TestQueryBypass_DownstreamHeaders: an unkeyed-parameter bypass is advertised as a
// MISS would be, with the carve-outs.
func TestQueryBypass_DownstreamHeaders(t *testing.T) {
	tests := []struct {
		name   string
		origin caddyhttp.Handler
		want   string
	}{
		{"origin silent: as cacheable as a MISS", pageOrigin("", "", http.StatusOK), "public, max-age=3600"},
		{"origin directives stand", pageOrigin("", "no-cache", http.StatusOK), "no-cache"},
		{"origin private stands", pageOrigin("", "private, max-age=0", http.StatusOK), "private, max-age=0"},
		{"a cookie-setting response is never public", pageOrigin("cart=abc", "", http.StatusOK), "private, no-store"},
		{"an uncacheable status is left alone", pageOrigin("", "", http.StatusInternalServerError), ""},
		{"a rate-limit response is left alone", pageOrigin("", "", http.StatusServiceUnavailable), ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := rangeTestSidekick(t)
			resp, _ := serveWith(t, s, tc.origin, "/shop/?unrecognized=1")
			assert.Equal(t, "BYPASS", resp.Header.Get(CacheHeaderName))
			assert.Equal(t, tc.want, resp.Header.Get("Cache-Control"))
		})
	}
}

func TestClickIDs_ShareThePageEntry(t *testing.T) {
	s, _ := rangeTestSidekick(t)
	origin := pageOrigin("", "", http.StatusOK)

	// A click with a well-formed ID fills the page's entry...
	resp, _ := serveWith(t, s, origin, "/landing/?gclid=Cj0KCQjw")
	assert.Equal(t, "MISS", resp.Header.Get(CacheHeaderName))
	resp, _ = serveWith(t, s, origin, "/landing/")
	assert.Equal(t, "HIT", resp.Header.Get(CacheHeaderName))

	// ...and every later click is served from it.
	resp, _ = serveWith(t, s, origin, "/landing/?fbclid=IwAR3")
	assert.Equal(t, "HIT", resp.Header.Get(CacheHeaderName))
}

func TestClickIDs_GatedValueIsReadOnly(t *testing.T) {
	s, _ := rangeTestSidekick(t)
	origin := pageOrigin("", "", http.StatusOK)

	// Cold: served by the origin, nothing stored.
	resp, _ := serveWith(t, s, origin, "/landing/?gclid=%3Cscript%3E")
	assert.Equal(t, "BYPASS", resp.Header.Get(CacheHeaderName))
	assert.Equal(t, "public, max-age=3600", resp.Header.Get("Cache-Control"))
	resp, body := serveWith(t, s, origin, "/landing/")
	assert.Equal(t, "MISS", resp.Header.Get(CacheHeaderName))
	assert.Equal(t, "<html>/landing/?</html>", body, "the gated render did not become the page")

	// Warm: may still be served the clean entry.
	resp, body = serveWith(t, s, origin, "/landing/?gclid=%3Cscript%3E")
	assert.Equal(t, "HIT", resp.Header.Get(CacheHeaderName))
	assert.Equal(t, "<html>/landing/?</html>", body)
}

func TestCampaignLabels_EachGetTheirOwnEntry(t *testing.T) {
	s, _ := rangeTestSidekick(t)
	origin := pageOrigin("", "", http.StatusOK)

	for _, target := range []string{"/landing/?utm_campaign=foo", "/landing/?utm_campaign=bar", "/landing/"} {
		resp, body := serveWith(t, s, origin, target)
		assert.Equal(t, "MISS", resp.Header.Get(CacheHeaderName), target)
		resp, body2 := serveWith(t, s, origin, target)
		assert.Equal(t, "HIT", resp.Header.Get(CacheHeaderName), target)
		assert.Equal(t, body, body2)
	}
}

func TestAssetVersionsAreKeyed(t *testing.T) {
	s, _ := rangeTestSidekick(t)
	origin := pageOrigin("", "", http.StatusOK)

	serveWith(t, s, origin, "/wp-includes/js/app.js?ver=1")
	resp, _ := serveWith(t, s, origin, "/wp-includes/js/app.js?ver=1")
	assert.Equal(t, "HIT", resp.Header.Get(CacheHeaderName))
	resp, body := serveWith(t, s, origin, "/wp-includes/js/app.js?ver=2")
	assert.Equal(t, "MISS", resp.Header.Get(CacheHeaderName))
	assert.Equal(t, "<html>/wp-includes/js/app.js?ver=2</html>", body)
}

// TestMissSettingACookieIsNotShareable: the MISS is stored without its Set-Cookie, so
// only that one response is kept out of shared caches.
func TestMissSettingACookieIsNotShareable(t *testing.T) {
	s, _ := rangeTestSidekick(t)
	origin := pageOrigin("pll_language=en; Path=/", "", http.StatusOK)

	resp, _ := serveWith(t, s, origin, "/about/")
	assert.Equal(t, "MISS", resp.Header.Get(CacheHeaderName))
	assert.Equal(t, "private, no-store", resp.Header.Get("Cache-Control"))

	resp, _ = serveWith(t, s, origin, "/about/")
	assert.Equal(t, "HIT", resp.Header.Get(CacheHeaderName))
	assert.Contains(t, resp.Header.Get("Cache-Control"), "public")
	assert.Empty(t, resp.Header.Get("Set-Cookie"), "the stored entry carries no cookie")
}

func TestBypassMetricReason(t *testing.T) {
	for reason, want := range map[bypassReason]string{
		bypassPrivate: "private", bypassPolicy: "policy", bypassDebug: "debug", bypassQuery: "query",
		bypassReason(99): "private",
	} {
		assert.Equal(t, want, bypassMetricReason(reason))
		assert.Contains(t, bypassReasonLabels, want)
	}
	assert.Contains(t, bypassReasonLabels, "query_readonly")

	var m *MetricsCollector
	assert.NotPanics(t, func() { m.RecordBypassReason("anything") })
}
