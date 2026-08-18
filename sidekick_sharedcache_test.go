package sidekick

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// prodLikeSidekick mirrors the production configuration that exposed the issue:
// the cache key varies on the WordPress session cookies, so any response for a
// request carrying one of them is session-specific.
func prodLikeSidekick(t *testing.T) *Sidekick {
	t.Helper()

	s := &Sidekick{
		CacheTTL:        86400,
		CacheKeyHeaders: []string{"Accept-Encoding"},
		CacheKeyCookies: []string{"wordpress_logged_in_*", "wordpress_sec_*"},
		NoCache:         []string{"/wp-admin", "/wp-json", "/sitepro"},
		logger:          zap.NewNop(),
	}

	rx, err := regexp.Compile(DefaultStaticAssetRegex)
	require.NoError(t, err)
	s.staticAssetRx = rx

	return s
}

func requestWithCookies(path string, cookies ...*http.Cookie) *http.Request {
	req := httptest.NewRequest("GET", path, nil)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	return req
}

// TestSharedCacheSafety_CookieVariedIsNeverPublic is the core guard. It is
// parameterized over every configured cache_key_cookies pattern rather than a fixed
// prefix, so adding a cookie to that list in future cannot silently reintroduce a
// shared-cache leak without failing this test.
func TestSharedCacheSafety_CookieVariedIsNeverPublic(t *testing.T) {
	s := prodLikeSidekick(t)

	states := map[string]cacheState{
		"hit":          cacheStateHit,
		"miss":         cacheStateMiss,
		"bypass":       cacheStateBypass,
		"not-modified": cacheStateNotModified,
	}

	for _, pattern := range s.CacheKeyCookies {
		cookieName := strings.TrimSuffix(pattern, "*") + "abc123"

		for stateName, state := range states {
			t.Run(pattern+"/"+stateName, func(t *testing.T) {
				req := requestWithCookies("/some-page",
					&http.Cookie{Name: cookieName, Value: "session-value"})

				dims := s.computeKeyDims(req)
				require.True(t, dims.Cookies,
					"cookie %q matches configured pattern %q so it must vary the key",
					cookieName, pattern)

				hdr := http.Header{}
				s.applyDownstreamCacheHeaders(hdr, dims, state,
					&Metadata{Timestamp: 1, StateCode: 200})

				cc := hdr.Get("Cache-Control")
				assert.Equal(t, "private, no-store", cc)
				assert.NotContains(t, cc, "public",
					"a cookie-varied response must never be advertised as shareable")
				assert.Contains(t, hdr.Get("Vary"), "Cookie")
				assert.Empty(t, hdr.Get("Age"),
					"Age implies a shared-cache lifetime and must not be set")
			})
		}
	}
}

// TestSharedCacheSafety_SecCookieWithoutLoggedIn covers the specific gap that
// motivated this work: wordpress_sec_* is in cache_key_cookies but shouldBypass only
// checks the wordpress_logged_in prefix, so such a request is cached rather than
// bypassed. It must still never be marked public.
func TestSharedCacheSafety_SecCookieWithoutLoggedIn(t *testing.T) {
	s := prodLikeSidekick(t)

	req := requestWithCookies("/members/dashboard",
		&http.Cookie{Name: "wordpress_sec_abc123", Value: "session-value"})

	assert.False(t, s.shouldBypass(req),
		"precondition: this request is not bypassed, which is why the header matters")

	dims := s.computeKeyDims(req)
	require.True(t, dims.Cookies)

	hdr := http.Header{}
	s.applyDownstreamCacheHeaders(hdr, dims, cacheStateHit, &Metadata{Timestamp: 1})

	assert.Equal(t, "private, no-store", hdr.Get("Cache-Control"))
	assert.Contains(t, hdr.Get("Vary"), "Cookie")
}

// TestSharedCacheSafety_AnonymousStaysPublic confirms the fix does not cost hit rate
// where sharing is safe: with no matching cookie the response is still public and
// Vary stays free of Cookie, so CloudFront can serve it from one entry.
func TestSharedCacheSafety_AnonymousStaysPublic(t *testing.T) {
	s := prodLikeSidekick(t)

	req := requestWithCookies("/some-page",
		&http.Cookie{Name: "_ga", Value: "GA1.2.3"},
		&http.Cookie{Name: "cookie_consent", Value: "accepted"})

	dims := s.computeKeyDims(req)
	require.False(t, dims.Cookies, "non-configured cookies must not vary the key")

	hdr := http.Header{}
	s.applyDownstreamCacheHeaders(hdr, dims, cacheStateHit, &Metadata{Timestamp: 1})

	assert.Contains(t, hdr.Get("Cache-Control"), "public")
	assert.NotContains(t, hdr.Get("Vary"), "Cookie")
}

// TestVaryIsConsistentAcrossStates guards the shared-cache correctness rule that a
// URL must present the same Vary on every path. An intermediary that stores a HIT
// under one Vary and serves it for a request that would have produced another is
// serving the wrong variant.
func TestVaryIsConsistentAcrossStates(t *testing.T) {
	s := prodLikeSidekick(t)
	req := httptest.NewRequest("GET", "/some-page", nil)
	dims := s.computeKeyDims(req)

	var seen string
	for i, state := range []cacheState{
		cacheStateBypass, cacheStateMiss, cacheStateHit, cacheStateNotModified,
	} {
		hdr := http.Header{}
		s.applyDownstreamCacheHeaders(hdr, dims, state, &Metadata{Timestamp: 1})
		got := hdr.Get("Vary")

		assert.NotEmpty(t, got, "Vary must be set on every path")
		if i == 0 {
			seen = got
			continue
		}
		assert.Equal(t, seen, got, "Vary must be identical across cache states")
	}
}

// TestVaryIncludesConfiguredHeaders verifies Vary reflects the actual key
// dimensions, and that Host is excluded (it is implied by the request URL).
func TestVaryIncludesConfiguredHeaders(t *testing.T) {
	s := prodLikeSidekick(t)
	s.CacheKeyHeaders = []string{"Accept-Encoding", "X-Custom", "Host"}

	req := httptest.NewRequest("GET", "/some-page", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	req.Header.Set("X-Custom", "v1")

	hdr := http.Header{}
	s.applyDownstreamCacheHeaders(hdr, s.computeKeyDims(req), cacheStateHit,
		&Metadata{Timestamp: 1})

	vary := hdr.Get("Vary")
	assert.Contains(t, vary, "Accept-Encoding")
	assert.Contains(t, vary, "X-Custom")
	assert.NotContains(t, vary, "Host", "Host is implied by the URL and must not be in Vary")
	assert.Equal(t, 1, strings.Count(vary, "Accept-Encoding"),
		"Accept-Encoding must not be duplicated")
}

// TestStaticAssetExemption_LoggedInIsCachedAndShared covers the hit-rate half of this
// change. WordPress scopes wordpress_logged_in_<hash> to "/", so a logged-in visitor
// sends it with every request including stylesheets. Those bytes are identical for
// everyone, so the request must not bypass and the response must stay shareable.
func TestStaticAssetExemption_LoggedInIsCachedAndShared(t *testing.T) {
	s := prodLikeSidekick(t)
	loggedIn := &http.Cookie{Name: "wordpress_logged_in_abc123", Value: "session-value"}

	staticPaths := []string{
		"/wp-content/themes/x/style.css",
		"/wp-content/plugins/y/script.js",
		"/wp-includes/js/jquery.js",
		"/wp-content/uploads/2026/02/hero.webp",
		"/wp-content/themes/x/fonts/inter.woff2",
	}

	for _, path := range staticPaths {
		t.Run(path, func(t *testing.T) {
			req := requestWithCookies(path, loggedIn)

			assert.False(t, s.shouldBypass(req),
				"static assets must not bypass just because a login cookie is present")

			dims := s.computeKeyDims(req)
			assert.False(t, dims.Cookies,
				"static asset keys must be cookie-free so one entry serves everyone")

			hdr := http.Header{}
			s.applyDownstreamCacheHeaders(hdr, dims, cacheStateHit, &Metadata{Timestamp: 1})
			assert.Contains(t, hdr.Get("Cache-Control"), "public")
			assert.NotContains(t, hdr.Get("Vary"), "Cookie")
		})
	}
}

// TestStaticAssetExemption_GatedFormatsStayProtected is the safety counterpart. The
// default pattern deliberately excludes the formats that membership and
// download-protection plugins gate, so those keep bypassing for logged-in users.
func TestStaticAssetExemption_GatedFormatsStayProtected(t *testing.T) {
	s := prodLikeSidekick(t)
	loggedIn := &http.Cookie{Name: "wordpress_logged_in_abc123", Value: "session-value"}

	gatedPaths := []string{
		"/wp-content/uploads/private/report.pdf",
		"/wp-content/uploads/private/course.zip",
		"/wp-content/uploads/private/lesson.mp4",
		"/wp-content/uploads/private/audio.mp3",
		"/wp-content/plugins/members/download.php",
		"/members/dashboard",
		"/",
	}

	for _, path := range gatedPaths {
		t.Run(path, func(t *testing.T) {
			req := requestWithCookies(path, loggedIn)

			assert.True(t, s.shouldBypass(req),
				"non-static paths must still bypass for logged-in users")

			hdr := http.Header{}
			s.applyDownstreamCacheHeaders(hdr, s.computeKeyDims(req), cacheStateBypass, nil)
			assert.NotContains(t, hdr.Get("Cache-Control"), "public")
		})
	}
}

// TestStaticAssetExemption_ExplicitExclusionsWin verifies ordering: the nocache
// prefixes and nocache_regex are evaluated before the exemption, so an explicitly
// excluded path stays excluded even when it looks like a static asset.
func TestStaticAssetExemption_ExplicitExclusionsWin(t *testing.T) {
	s := prodLikeSidekick(t)
	rx, err := regexp.Compile(`^/private/.*\.css$`)
	require.NoError(t, err)
	s.pathRx = rx

	loggedIn := &http.Cookie{Name: "wordpress_logged_in_abc123", Value: "session-value"}

	// Matches a nocache prefix and also looks static.
	assert.True(t, s.shouldBypass(requestWithCookies("/wp-admin/load-styles.css", loggedIn)),
		"nocache prefix must win over the static-asset exemption")

	// Matches nocache_regex and also looks static.
	assert.True(t, s.shouldBypass(requestWithCookies("/private/secret.css", loggedIn)),
		"nocache_regex must win over the static-asset exemption")
}

// TestKeyDimsDriveBothKeyAndSharing asserts the structural invariant: the cache key
// and the shareability decision are derived from the same computeKeyDims result, so
// they cannot drift apart.
func TestKeyDimsDriveBothKeyAndSharing(t *testing.T) {
	s := prodLikeSidekick(t)
	loggedIn := &http.Cookie{Name: "wordpress_logged_in_abc123", Value: "a"}
	other := &http.Cookie{Name: "wordpress_logged_in_abc123", Value: "b"}

	// Dynamic path: cookie varies the key AND forces private.
	keyA, dimsA := s.buildCacheKey(requestWithCookies("/page", loggedIn))
	keyB, dimsB := s.buildCacheKey(requestWithCookies("/page", other))
	assert.NotEqual(t, keyA, keyB, "different cookie values must produce different keys")
	assert.True(t, dimsA.Cookies)
	assert.True(t, dimsB.Cookies)

	// Static path: cookie varies neither the key nor the sharing decision.
	keyC, dimsC := s.buildCacheKey(requestWithCookies("/app.css", loggedIn))
	keyD, dimsD := s.buildCacheKey(requestWithCookies("/app.css", other))
	assert.Equal(t, keyC, keyD, "static asset keys must not vary by cookie")
	assert.False(t, dimsC.Cookies)
	assert.False(t, dimsD.Cookies)
}

// TestApplyDownstreamCacheHeadersIsTheOnlyCacheControlWriter is the structural guard
// from the plan: if a future change writes downstream cache headers from a new code
// path, the leak this work closed could reappear without any other test failing.
//
// It resolves the enclosing function via the AST rather than matching on line
// content, so a write cannot hide by coincidentally resembling one of the
// chokepoint's own lines.
func TestApplyDownstreamCacheHeadersIsTheOnlyCacheControlWriter(t *testing.T) {
	// Headers that carry a shared-cacheability decision. Whoever writes these
	// decides whether an intermediary may store the response.
	guarded := map[string]bool{
		"Cache-Control": true,
		"Pragma":        true,
		"Age":           true,
		"Vary":          true,
	}
	const chokepoint = "applyDownstreamCacheHeaders"

	entries, err := os.ReadDir(".")
	require.NoError(t, err)

	var offenders []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}

		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, filepath.Clean(name), nil, 0)
		require.NoError(t, err)

		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Name.Name == chokepoint {
				continue
			}

			ast.Inspect(fn, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok || len(call.Args) == 0 {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || (sel.Sel.Name != "Set" && sel.Sel.Name != "Add") {
					return true
				}
				lit, ok := call.Args[0].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					return true
				}
				header := strings.Trim(lit.Value, `"`)
				if !guarded[header] {
					return true
				}
				offenders = append(offenders, fmt.Sprintf("%s writes %q at %s",
					fn.Name.Name, header, fset.Position(call.Pos())))
				return true
			})
		}
	}

	assert.Empty(t, offenders,
		"downstream cache headers must only be written by "+chokepoint+"; "+
			"route new paths through it so the cookie-varied check cannot be bypassed")
}
