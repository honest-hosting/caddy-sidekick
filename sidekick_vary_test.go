package sidekick

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// varyTestSidekick is a serving Sidekick with vary support enabled, as Provision
// configures it by default.
func varyTestSidekick(t *testing.T) *Sidekick {
	t.Helper()
	s, _ := rangeTestSidekick(t)
	on := true
	s.RespectVary = &on
	s.varyIdx = newVaryIndex(0)
	return s
}

// countryOrigin renders a page whose currency depends on CF-IPCountry and declares it.
type countryOrigin struct {
	calls  atomic.Int32
	vary   string
	cookie string // X-Sidekick-Vary declaration, if any
}

func (o *countryOrigin) ServeHTTP(w http.ResponseWriter, r *http.Request) error {
	o.calls.Add(1)
	if o.vary != "" {
		w.Header().Set("Vary", o.vary)
	}
	if o.cookie != "" {
		w.Header().Set(VaryCookieHeader, o.cookie)
	}
	w.Header().Set("Content-Type", "text/html")
	body := "country=" + r.Header.Get("CF-IPCountry")
	if c, err := r.Cookie("aelia_cs_selected_currency"); err == nil {
		body += " currency=" + c.Value
	}
	_, _ = io.WriteString(w, body)
	return nil
}

func get(t *testing.T, s *Sidekick, o *countryOrigin, path string, setup func(*http.Request)) *http.Response {
	t.Helper()
	req := httptest.NewRequest("GET", path, nil)
	if setup != nil {
		setup(req)
	}
	rec := httptest.NewRecorder()
	require.NoError(t, s.ServeHTTP(rec, req, caddyhttp.HandlerFunc(o.ServeHTTP)))
	s.Storage.WaitForAsyncOps()
	return rec.Result()
}

func body(t *testing.T, resp *http.Response) string {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return string(b)
}

func country(c string) func(*http.Request) {
	return func(r *http.Request) { r.Header.Set("CF-IPCountry", c) }
}

func TestVary_HeaderSplitsVariantsAndNeverCrossServes(t *testing.T) {
	s := varyTestSidekick(t)
	o := &countryOrigin{vary: "Accept-Encoding, CF-IPCountry"}

	us := get(t, s, o, "/pricing/", country("US"))
	assert.Equal(t, "MISS", us.Header.Get(CacheHeaderName))
	assert.Equal(t, "country=US", body(t, us))

	de := get(t, s, o, "/pricing/", country("DE"))
	assert.Equal(t, "MISS", de.Header.Get(CacheHeaderName), "a new country is a new variant")
	assert.Equal(t, "country=DE", body(t, de))

	us2 := get(t, s, o, "/pricing/", country("US"))
	assert.Equal(t, "HIT", us2.Header.Get(CacheHeaderName))
	assert.Equal(t, "country=US", body(t, us2))

	de2 := get(t, s, o, "/pricing/", country("DE"))
	assert.Equal(t, "HIT", de2.Header.Get(CacheHeaderName))
	assert.Equal(t, "country=DE", body(t, de2))

	none := get(t, s, o, "/pricing/", nil)
	assert.Equal(t, "MISS", none.Header.Get(CacheHeaderName), "absent header is its own variant")
	assert.Equal(t, int32(3), o.calls.Load())

	for _, r := range []*http.Response{us2, de2} {
		assert.Contains(t, r.Header.Get("Vary"), "Cf-Ipcountry")
		assert.Equal(t, "no-store", r.Header.Get("Cloudflare-CDN-Cache-Control"),
			"CDNs that ignore Vary must not store a varied page")
		assert.Contains(t, r.Header.Get("Cache-Control"), "public")
	}
}

func TestVary_UnvariedSiteKeepsOneSharedEntry(t *testing.T) {
	s := varyTestSidekick(t)
	o := &countryOrigin{vary: "Accept-Encoding"}

	get(t, s, o, "/about/", country("US"))
	hit := get(t, s, o, "/about/", country("DE"))
	assert.Equal(t, "HIT", hit.Header.Get(CacheHeaderName),
		"a page that does not vary on country must be shared across countries")
	assert.Equal(t, int32(1), o.calls.Load())
	assert.Empty(t, hit.Header.Get("Cloudflare-CDN-Cache-Control"))
	assert.Equal(t, "Accept-Encoding", hit.Header.Get("Vary"))
}

func TestVary_CookieDeclarationVariesOnValueAndIsStripped(t *testing.T) {
	s := varyTestSidekick(t)
	o := &countryOrigin{cookie: "cookie=aelia_cs_selected_currency"}
	currency := func(v string) func(*http.Request) {
		return func(r *http.Request) {
			r.AddCookie(&http.Cookie{Name: "aelia_cs_selected_currency", Value: v})
			r.AddCookie(&http.Cookie{Name: "_ga", Value: "unrelated"})
		}
	}

	first := get(t, s, o, "/", nil)
	assert.Empty(t, first.Header.Get(VaryCookieHeader), "the declaration is never sent downstream")
	gbp := get(t, s, o, "/", currency("GBP"))
	assert.Equal(t, "MISS", gbp.Header.Get(CacheHeaderName))
	assert.Equal(t, "country= currency=GBP", body(t, gbp))

	gbp2 := get(t, s, o, "/", currency("GBP"))
	assert.Equal(t, "HIT", gbp2.Header.Get(CacheHeaderName))
	assert.Equal(t, "country= currency=GBP", body(t, gbp2))
	assert.Contains(t, gbp2.Header.Get("Vary"), "Cookie")

	anon := get(t, s, o, "/", nil)
	assert.Equal(t, "HIT", anon.Header.Get(CacheHeaderName))
	assert.Equal(t, "country=", body(t, anon), "visitors without the cookie share the default variant")
	assert.Equal(t, int32(2), o.calls.Load())
}

func TestVary_CookieOrStarIsNotStored(t *testing.T) {
	for _, v := range []string{"Cookie", "*"} {
		t.Run(v, func(t *testing.T) {
			s := varyTestSidekick(t)
			o := &countryOrigin{vary: v}
			get(t, s, o, "/account-ish/", nil)
			second := get(t, s, o, "/account-ish/", nil)
			assert.Equal(t, "BYPASS", second.Header.Get(CacheHeaderName))
			assert.Equal(t, int32(2), o.calls.Load())
			assert.Contains(t, second.Header.Get("Vary"), v)
			assert.Equal(t, "no-store", second.Header.Get("CDN-Cache-Control"))
		})
	}
}

func TestVary_SpecChangeIsFollowed(t *testing.T) {
	s := varyTestSidekick(t)
	o := &countryOrigin{vary: "CF-IPCountry"}
	get(t, s, o, "/p/", country("US"))

	// The site stops varying (plugin removed): the next miss is stored unvaried
	// and then shared.
	o.vary = ""
	get(t, s, o, "/p/", country("DE"))
	hit := get(t, s, o, "/p/", country("FR"))
	assert.Equal(t, "HIT", hit.Header.Get(CacheHeaderName))
	assert.Equal(t, int32(2), o.calls.Load())
}

func TestVary_PurgeByPathRemovesEveryVariant(t *testing.T) {
	s := varyTestSidekick(t)
	o := &countryOrigin{vary: "CF-IPCountry"}
	get(t, s, o, "/pricing/", country("US"))
	get(t, s, o, "/pricing/", country("DE"))

	req := httptest.NewRequest("POST", s.PurgePath, strings.NewReader(`{"paths":["/pricing/"]}`))
	req.Header.Set(s.PurgeHeader, s.PurgeToken)
	rec := httptest.NewRecorder()
	require.NoError(t, s.ServeHTTP(rec, req, caddyhttp.HandlerFunc(o.ServeHTTP)))
	s.Storage.WaitForAsyncOps()

	assert.Equal(t, "MISS", get(t, s, o, "/pricing/", country("US")).Header.Get(CacheHeaderName))
	assert.Equal(t, "MISS", get(t, s, o, "/pricing/", country("DE")).Header.Get(CacheHeaderName))
}

func TestVary_DisabledKeepsLegacyBehavior(t *testing.T) {
	s, _ := rangeTestSidekick(t) // RespectVary nil: never provisioned
	o := &countryOrigin{vary: "CF-IPCountry"}
	get(t, s, o, "/p/", country("US"))
	hit := get(t, s, o, "/p/", country("DE"))
	assert.Equal(t, "HIT", hit.Header.Get(CacheHeaderName))
}

func TestParseResponseVary(t *testing.T) {
	h := http.Header{}
	h.Add("Vary", "accept-encoding, cf-ipcountry")
	h.Add("Vary", "X-Lang, cf-ipcountry")
	h.Set(VaryCookieHeader, "cookie=b, cookie=a ,value=ismobile, cookie=bad;name")
	d := parseResponseVary(h)
	assert.Equal(t, []string{"Cf-Ipcountry", "X-Lang"}, d.spec.Headers)
	assert.Equal(t, []string{"a", "b"}, d.spec.Cookies)
	assert.False(t, d.uncacheable())
}
