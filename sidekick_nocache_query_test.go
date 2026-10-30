package sidekick

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stepZeroSidekick is the bypass fixture with the module's default nocache prefixes and
// nocache_query left unset, so the defaults are what is under test.
func stepZeroSidekick(t *testing.T) *Sidekick {
	t.Helper()
	s := bypassReasonSidekick(t)
	s.NoCache = append(strings.Split(DefaultNoCache, ","), "/sitepro")
	s.NoCacheQuery = nil
	return s
}

// TestShouldBypass_RESTAPIInEveryURLForm is the core of issue #8: WordPress serves the
// REST API on three URL shapes, and a path prefix only ever saw one of them.
func TestShouldBypass_RESTAPIInEveryURLForm(t *testing.T) {
	s := stepZeroSidekick(t)

	private := []string{
		// The reported form, and the one get_rest_url() emits on plain permalinks.
		"/index.php?rest_route=/vendor/v1/sync",
		// rest_route works on ANY path, including the home page and ordinary pages.
		"/?rest_route=/",
		"/about/?rest_route=/wp/v2/posts",
		// Present but empty still bypasses: cheaper than reasoning about emptiness.
		"/?rest_route=",
		"/?rest_route",
		// Names PHP registers as rest_route although Go parses them as something else.
		"/?rest.route=/",
		"/?rest+route=/",
		"/?rest%20route=/",
		"/?rest%5Froute=/",
		"/?rest[route=/",
		"/?rest_route[]=/",
		"/?%20rest_route=/",
		"/?rest_route%00x=/",
		// rest_route among other parameters, and after a ';' that Go would reject.
		"/?page=2&rest_route=/x",
		"/?a=1;b&rest_route=/x",
		// The second REST path form, registered unconditionally by core.
		"/index.php/wp-json/wp/v2/posts",
		"/wp-json/wp/v2/posts",
	}
	for _, target := range private {
		t.Run(target, func(t *testing.T) {
			assert.Equal(t, bypassPrivate, s.shouldBypass(httptest.NewRequest("GET", target, nil)))
		})
	}

	notREST := []string{
		// PHP keys are case-sensitive, so this is not the REST API.
		"/?REST_ROUTE=/",
		// PHP splits on '&' only, so this registers "p" alone.
		"/about?p=1;rest_route=/",
		"/about?route=/x",
	}
	for _, target := range notREST {
		t.Run(target, func(t *testing.T) {
			req := httptest.NewRequest("GET", target, nil)
			_, isREST := s.matchNoCacheQuery(req.URL.RawQuery)
			assert.False(t, isREST, "not the REST API as WordPress sees it")
			// Still not cached: an unkeyed parameter, or (for the ';' form) a query
			// Go and PHP read differently. See querykey_test.go.
			assert.NotEqual(t, bypassNone, s.shouldBypass(req))
		})
	}
}

// TestShouldBypass_HeldCommentQuery covers core's "your comment is awaiting moderation"
// view: WordPress renders the held comment into the page for the visitor carrying
// these parameters, so that view is specific to them.
func TestShouldBypass_HeldCommentQuery(t *testing.T) {
	s := stepZeroSidekick(t)

	for _, target := range []string{
		"/hello-world/?unapproved=42&moderation-hash=abc123",
		"/hello-world/?unapproved=42",
		"/hello-world/?moderation-hash=abc123",
	} {
		t.Run(target, func(t *testing.T) {
			assert.Equal(t, bypassPrivate, s.shouldBypass(httptest.NewRequest("GET", target, nil)))
		})
	}
}

// TestShouldBypass_CommentAuthorCookie is the cookie half of the same problem:
// wp_get_unapproved_comment_author_email() falls back to these cookies.
func TestShouldBypass_CommentAuthorCookie(t *testing.T) {
	s := stepZeroSidekick(t)

	for _, name := range []string{
		"comment_author_0123abcd",
		"comment_author_email_0123abcd",
		"comment_author_url_0123abcd",
	} {
		t.Run(name, func(t *testing.T) {
			cookie := &http.Cookie{Name: name, Value: "v"}
			assert.Equal(t, bypassPrivate, s.shouldBypass(requestWithCookies("/hello-world/", cookie)))

			// Static assets stay exempt, exactly as for the login cookie: their bytes
			// cannot contain a held comment.
			assert.Equal(t, bypassNone, s.shouldBypass(requestWithCookies("/wp-content/themes/t/style.css", cookie)))
		})
	}

	// An unrelated cookie that merely contains the word does not bypass.
	assert.Equal(t, bypassNone,
		s.shouldBypass(requestWithCookies("/hello-world/", &http.Cookie{Name: "my_comment_author", Value: "v"})))
}

// TestShouldBypass_AuthorizationHeader: RFC 9111 §3.5. The key does not include
// Authorization, so a stored authenticated response would be served to anyone.
func TestShouldBypass_AuthorizationHeader(t *testing.T) {
	s := stepZeroSidekick(t)

	for _, value := range []string{"Bearer abc.def.ghi", "Basic dXNlcjpwYXNz"} {
		req := httptest.NewRequest("GET", "/about/", nil)
		req.Header.Set("Authorization", value)
		assert.Equal(t, bypassPrivate, s.shouldBypass(req), value)
	}

	// Even a static asset: the cache key has no idea who was authenticated.
	req := httptest.NewRequest("GET", "/app.css", nil)
	req.Header.Set("Authorization", "Bearer x")
	assert.Equal(t, bypassPrivate, s.shouldBypass(req))
}

// TestShouldBypass_PrivateOutranksPolicy: REST and authenticated requests must stay
// private even where a policy rule (nocache_home, nocache_regex) also matches, because
// a policy bypass leaves the origin's headers alone and lets CloudFront store them.
func TestShouldBypass_PrivateOutranksPolicy(t *testing.T) {
	s := stepZeroSidekick(t)
	s.NoCacheHome = true

	assert.Equal(t, bypassPrivate, s.shouldBypass(httptest.NewRequest("GET", "/?rest_route=/", nil)))
	assert.Equal(t, bypassPrivate, s.shouldBypass(httptest.NewRequest("GET", "/a.mp4?rest_route=/", nil)))

	req := httptest.NewRequest("GET", "/a.mp4", nil)
	req.Header.Set("Authorization", "Bearer x")
	assert.Equal(t, bypassPrivate, s.shouldBypass(req))

	// The debug query still wins over everything, as before.
	assert.Equal(t, bypassDebug,
		s.shouldBypass(httptest.NewRequest("GET", "/?rest_route=/&"+DefaultBypassDebugQuery+"=1", nil)))
}

// TestShouldBypass_MultisiteSubdirectoryPrefixes: on subdirectory multisite every
// subsite's system paths live under /<site>/, which root-anchored prefixes never saw.
func TestShouldBypass_MultisiteSubdirectoryPrefixes(t *testing.T) {
	s := stepZeroSidekick(t)
	s.NoCache = append(s.NoCache, "/wp-cron.php")

	for _, target := range []string{
		"/blog/wp-json/wp/v2/posts",
		"/blog/index.php/wp-json/wp/v2/posts",
		"/blog/wp-admin/admin-ajax.php?action=x",
		"/blog/wp-cron.php?doing_wp_cron=1",
		"/Site_2-b/wp-admin/",
	} {
		t.Run(target, func(t *testing.T) {
			assert.Equal(t, bypassPrivate, s.shouldBypass(httptest.NewRequest("GET", target, nil)))
		})
	}

	for _, target := range []string{
		"/blog/hello-world/",
		// Deeper than WordPress's own rewrite strips.
		"/a/b/wp-json/",
		// Not a valid site segment under [_0-9a-zA-Z-]+.
		"/a.b/wp-json/",
	} {
		t.Run(target, func(t *testing.T) {
			assert.Equal(t, bypassNone, s.shouldBypass(httptest.NewRequest("GET", target, nil)))
		})
	}
}

func TestMatchesNoCachePrefix(t *testing.T) {
	tests := []struct {
		path, prefix string
		want         bool
	}{
		{"/wp-admin", "/wp-admin", true},
		{"/wp-admin/index.php", "/wp-admin", true},
		{"/wp-json-custom", "/wp-json", true}, // existing prefix semantics are kept
		{"/blog/wp-admin/index.php", "/wp-admin", true},
		{"/blog/wp-json-custom", "/wp-json", true},
		{"/wp-admin", "/admin", false}, // prefix not at a segment start
		{"/blog/wp-admin", "/admin", false},
		{"/a/b/wp-admin", "/wp-admin", false},
		{"/blog", "/wp-admin", false},
		{"//wp-admin", "/wp-admin", false},    // empty segment
		{"/blog/wp-admin", "wp-admin", false}, // only "/"-rooted prefixes get the rule
	}
	for _, tc := range tests {
		t.Run(fmt.Sprintf("%s|%s", tc.path, tc.prefix), func(t *testing.T) {
			assert.Equal(t, tc.want, matchesNoCachePrefix(tc.path, tc.prefix))
		})
	}
}

// TestMatchNoCacheQuery_Config covers custom lists, prefix wildcards, and the
// difference between "not configured" (nil, default applies) and "explicitly none".
func TestMatchNoCacheQuery_Config(t *testing.T) {
	s := &Sidekick{}

	_, ok := s.matchNoCacheQuery("rest_route=/")
	assert.True(t, ok, "nil means the default list")

	s.NoCacheQuery = []string{"myplugin_api", "wc-*"}
	name, ok := s.matchNoCacheQuery("x=1&myplugin.api=1")
	assert.True(t, ok)
	assert.Equal(t, "myplugin_api", name)
	_, ok = s.matchNoCacheQuery("wc-ajax=get_refreshed_fragments")
	assert.True(t, ok, "a trailing * matches by prefix")
	_, ok = s.matchNoCacheQuery("rest_route=/")
	assert.False(t, ok, "a configured list replaces the default")

	s.NoCacheQuery = []string{}
	_, ok = s.matchNoCacheQuery("rest_route=/")
	assert.False(t, ok, "an explicitly empty list disables query bypass")
}

func TestUnmarshalCaddyfile_NoCacheQuery(t *testing.T) {
	tests := []struct {
		input string
		want  []string
	}{
		{`sidekick {
			nocache_query rest_route unapproved
		}`, []string{"rest_route", "unapproved"}},
		{`sidekick {
			nocache_query "rest_route, ,myplugin_*"
		}`, []string{"rest_route", "myplugin_*"}},
		{`sidekick {
			nocache_query ""
		}`, []string{}},
	}
	for _, tc := range tests {
		s := &Sidekick{}
		require.NoError(t, s.UnmarshalCaddyfile(caddyfile.NewTestDispenser(tc.input)))
		assert.Equal(t, tc.want, s.NoCacheQuery)
	}

	// Not mentioned at all: stays nil so Provision applies env or the default.
	s := &Sidekick{}
	require.NoError(t, s.UnmarshalCaddyfile(caddyfile.NewTestDispenser(`sidekick {
		cache_ttl 60
	}`)))
	assert.Nil(t, s.NoCacheQuery)
}

// TestNoCacheQuery_SurvivesJSONAdaptation: Caddy adapts the Caddyfile to JSON before
// loading the module, so "explicitly empty" must survive that round trip rather than
// silently becoming the default again.
func TestNoCacheQuery_SurvivesJSONAdaptation(t *testing.T) {
	for _, tc := range []struct {
		in   []string
		want []string
	}{
		{nil, nil},
		{[]string{}, []string{}},
		{[]string{"rest_route"}, []string{"rest_route"}},
	} {
		raw, err := json.Marshal(&Sidekick{NoCacheQuery: tc.in})
		require.NoError(t, err)
		var out Sidekick
		require.NoError(t, json.Unmarshal(raw, &out))
		assert.Equal(t, tc.want, out.NoCacheQuery, string(raw))
	}
}

func TestProvision_NoCacheDefaults(t *testing.T) {
	t.Setenv("SIDEKICK_CACHE_DIR", t.TempDir())

	t.Run("defaults", func(t *testing.T) {
		t.Setenv("SIDEKICK_NOCACHE", "")
		t.Setenv("SIDEKICK_NOCACHE_QUERY", "")
		s := &Sidekick{}
		require.NoError(t, s.Provision(caddy.Context{}))
		assert.Equal(t, []string{"/wp-admin", "/wp-json", "/index.php/wp-json"}, s.NoCache)
		assert.Equal(t, []string{"rest_route", "unapproved", "moderation-hash"}, s.NoCacheQuery)
	})

	t.Run("env", func(t *testing.T) {
		t.Setenv("SIDEKICK_NOCACHE_QUERY", " rest_route , myplugin_api ,")
		s := &Sidekick{}
		require.NoError(t, s.Provision(caddy.Context{}))
		assert.Equal(t, []string{"rest_route", "myplugin_api"}, s.NoCacheQuery)
	})

	t.Run("configured value wins over env", func(t *testing.T) {
		t.Setenv("SIDEKICK_NOCACHE_QUERY", "from_env")
		s := &Sidekick{NoCacheQuery: []string{"from_config"}}
		require.NoError(t, s.Provision(caddy.Context{}))
		assert.Equal(t, []string{"from_config"}, s.NoCacheQuery)
	})

	t.Run("explicitly empty stays empty", func(t *testing.T) {
		s := &Sidekick{NoCacheQuery: []string{}}
		require.NoError(t, s.Provision(caddy.Context{}))
		assert.Equal(t, []string{}, s.NoCacheQuery)
	})
}

// restOrigin answers like WordPress: a REST response when rest_route is present
// (anywhere), the page otherwise. Bodies name what was served so a test can tell
// which request's response came back.
func restOrigin() caddyhttp.Handler {
	return caddyhttp.HandlerFunc(func(w http.ResponseWriter, r *http.Request) error {
		if route, ok := r.URL.Query()["rest_route"]; ok {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, err := fmt.Fprintf(w, `{"route":%q}`, route[0])
			return err
		}
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusOK)
		_, err := fmt.Fprintf(w, "<html>page %s</html>", r.URL.Path)
		return err
	})
}

func serve(t *testing.T, s *Sidekick, origin caddyhttp.Handler, target string) (*http.Response, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	require.NoError(t, s.ServeHTTP(rec, httptest.NewRequest("GET", target, nil), origin))
	resp := rec.Result()
	return resp, string(readAll(t, resp))
}

// TestRESTRoutesNeverShareACacheEntry is issue #8 end to end: distinct ?rest_route=
// routes on /index.php must each get their own response.
func TestRESTRoutesNeverShareACacheEntry(t *testing.T) {
	s, _ := rangeTestSidekick(t)
	origin := restOrigin()

	respA, bodyA := serve(t, s, origin, "/index.php?rest_route=/vendor/v1/orders")
	respB, bodyB := serve(t, s, origin, "/index.php?rest_route=/vendor/v1/customers")

	assert.Equal(t, `{"route":"/vendor/v1/orders"}`, bodyA)
	assert.Equal(t, `{"route":"/vendor/v1/customers"}`, bodyB, "each route must get its own response")
	for _, resp := range []*http.Response{respA, respB} {
		assert.Equal(t, "BYPASS", resp.Header.Get(CacheHeaderName))
		assert.Equal(t, "private, no-store", resp.Header.Get("Cache-Control"),
			"REST must stay out of shared caches: CloudFront path patterns cannot see ?rest_route=")
	}

	// And nothing was stored under /index.php for an anonymous visitor to receive.
	key, _ := s.buildCacheKey(httptest.NewRequest("GET", "/index.php", nil))
	_, _, err := s.Storage.Get(key)
	assert.Error(t, err)
}

// TestRESTQueryDoesNotFillPageEntry: a REST request made on a page's path must leave
// nothing behind in that page's cache entry.
func TestRESTQueryDoesNotFillPageEntry(t *testing.T) {
	s, _ := rangeTestSidekick(t)
	origin := restOrigin()

	for _, target := range []string{"/?rest_route=/", "/about/?rest.route=/", "/about/?rest_route[]=/"} {
		resp, _ := serve(t, s, origin, target)
		assert.Equal(t, "BYPASS", resp.Header.Get(CacheHeaderName), target)
	}

	for _, page := range []string{"/", "/about/"} {
		resp, body := serve(t, s, origin, page)
		assert.Equal(t, "MISS", resp.Header.Get(CacheHeaderName), "%s must not have been filled by a REST request", page)
		assert.Equal(t, "<html>page "+page+"</html>", body)
	}
}
