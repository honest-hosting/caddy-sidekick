package sidekick

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"

	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// bypassReasonSidekick builds on the fully-wired range fixture (purge paths, storage,
// buffer pool) and layers on the cookie and bypass configuration this phase exercises.
func bypassReasonSidekick(t *testing.T) *Sidekick {
	t.Helper()

	s, _ := rangeTestSidekick(t)
	s.BypassCacheControl = bypassCacheControlPreserve
	s.CacheKeyCookies = []string{"wordpress_logged_in_*", "wordpress_sec_*"}
	s.NoCache = []string{"/wp-admin", "/wp-json", "/sitepro"}

	rx, err := regexp.Compile(`\.(mp4|webm|zip)$`)
	require.NoError(t, err)
	s.pathRx = rx
	s.NoCacheRegex = `\.(mp4|webm|zip)$`
	s.bypassDebugQuery = DefaultBypassDebugQuery

	return s
}

// TestShouldBypass_Classification is the heart of this phase: every bypass trigger
// must report WHY, because "Sidekick declines to store this" and "nobody may store
// this" need different headers.
func TestShouldBypass_Classification(t *testing.T) {
	s := bypassReasonSidekick(t)
	s.NoCacheHome = true

	tests := []struct {
		name   string
		path   string
		cookie *http.Cookie
		query  string
		want   bypassReason
	}{
		{name: "admin prefix is private", path: "/wp-admin/index.php", want: bypassPrivate},
		{name: "api prefix is private", path: "/wp-json/wp/v2/posts", want: bypassPrivate},
		{name: "sitepro prefix is private", path: "/sitepro/dashboard", want: bypassPrivate},
		{
			name:   "login cookie is private",
			path:   "/members/area",
			cookie: &http.Cookie{Name: "wordpress_logged_in_abc", Value: "v"},
			want:   bypassPrivate,
		},

		{name: "regex match is policy", path: "/downloads/file.zip", want: bypassPolicy},
		{name: "media regex match is policy", path: "/wp-content/uploads/a.mp4", want: bypassPolicy},
		{name: "home page is policy", path: "/", want: bypassPolicy},

		{name: "debug query", path: "/anything", query: DefaultBypassDebugQuery + "=1", want: bypassDebug},

		{name: "ordinary page does not bypass", path: "/about", want: bypassNone},
		{name: "static asset does not bypass", path: "/app.css", want: bypassNone},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			target := tc.path
			if tc.query != "" {
				target += "?" + tc.query
			}
			req := httptest.NewRequest("GET", target, nil)
			if tc.cookie != nil {
				req.AddCookie(tc.cookie)
			}
			assert.Equal(t, tc.want, s.shouldBypass(req))
		})
	}
}

// TestPolicyBypass_PreservesOriginCacheControl is the behavior change that matters for
// CDN hit rate: a media file Sidekick declines to store must still be cacheable by the
// browser and CloudFront.
func TestPolicyBypass_PreservesOriginCacheControl(t *testing.T) {
	s := bypassReasonSidekick(t)

	origin := caddyhttp.HandlerFunc(func(w http.ResponseWriter, r *http.Request) error {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		w.Header().Set("Content-Type", "video/mp4")
		w.WriteHeader(http.StatusOK)
		_, err := w.Write([]byte("video-bytes"))
		return err
	})

	req := httptest.NewRequest("GET", "/wp-content/uploads/a.mp4", nil)
	rec := httptest.NewRecorder()
	require.NoError(t, s.ServeHTTP(rec, req, origin))

	resp := rec.Result()
	assert.Equal(t, "BYPASS", resp.Header.Get(CacheHeaderName))
	assert.Equal(t, "public, max-age=31536000, immutable", resp.Header.Get("Cache-Control"),
		"a policy bypass must not overwrite the origin's own directives")
	assert.Empty(t, resp.Header.Get("Pragma"))
}

// TestPrivateBypass_IsLockedDown is the counterpart: the privacy guarantee from phase
// 0 must survive this change untouched.
func TestPrivateBypass_IsLockedDown(t *testing.T) {
	s := bypassReasonSidekick(t)

	origin := caddyhttp.HandlerFunc(func(w http.ResponseWriter, r *http.Request) error {
		// Even an origin claiming the response is public must not win here.
		w.Header().Set("Cache-Control", "public, max-age=600")
		w.WriteHeader(http.StatusOK)
		_, err := w.Write([]byte("secret"))
		return err
	})

	for _, path := range []string{"/wp-admin/index.php", "/wp-json/wp/v2/users"} {
		t.Run(path, func(t *testing.T) {
			req := httptest.NewRequest("GET", path, nil)
			rec := httptest.NewRecorder()
			require.NoError(t, s.ServeHTTP(rec, req, origin))

			resp := rec.Result()
			assert.Equal(t, "private, no-store", resp.Header.Get("Cache-Control"))
			assert.NotContains(t, resp.Header.Get("Cache-Control"), "public")
		})
	}
}

// TestOriginNoStoreIsRespected covers a bug this phase had to fix before advertising
// MISS responses as public: nothing previously looked at the origin's own
// Cache-Control, so a response the application marked no-store was cached anyway.
func TestOriginNoStoreIsRespected(t *testing.T) {
	for _, cc := range []string{"no-store", "private", "private, max-age=0", "no-cache, no-store"} {
		t.Run(cc, func(t *testing.T) {
			s, _ := rangeTestSidekick(t)

			origin := caddyhttp.HandlerFunc(func(w http.ResponseWriter, r *http.Request) error {
				w.Header().Set("Cache-Control", cc)
				w.Header().Set("Content-Type", "text/html")
				w.WriteHeader(http.StatusOK)
				_, err := w.Write([]byte("account details"))
				return err
			})

			req := httptest.NewRequest("GET", "/my-account", nil)
			rec := httptest.NewRecorder()
			require.NoError(t, s.ServeHTTP(rec, req, origin))

			resp := rec.Result()
			assert.Equal(t, "BYPASS", resp.Header.Get(CacheHeaderName),
				"an origin-private response must not be cached by Sidekick")
			assert.Equal(t, "private, no-store", resp.Header.Get("Cache-Control"))

			// And it really is absent from the cache.
			key, _ := s.buildCacheKey(req)
			_, _, err := s.Storage.Get(key)
			assert.Error(t, err, "the response must not have been stored")
		})
	}
}

// TestMissIsAdvertisedAsCacheable is the headline fix for CDN behavior: the first
// viewer's copy is no longer thrown away by every downstream cache.
func TestMissIsAdvertisedAsCacheable(t *testing.T) {
	s, origin := rangeTestSidekick(t)

	resp := doGet(t, s, origin, "/wp-content/uploads/first.mp4", nil)
	readAll(t, resp)

	assert.Equal(t, "MISS", resp.Header.Get(CacheHeaderName))
	assert.Contains(t, resp.Header.Get("Cache-Control"), "public")
	assert.NotContains(t, resp.Header.Get("Cache-Control"), "no-store")
	assert.Empty(t, resp.Header.Get("Pragma"))

	// A MISS and a HIT must look the same to an intermediary apart from Age.
	resp2 := doGet(t, s, origin, "/wp-content/uploads/first.mp4", nil)
	readAll(t, resp2)
	assert.Equal(t, "HIT", resp2.Header.Get(CacheHeaderName))
	assert.Contains(t, resp2.Header.Get("Cache-Control"), "public")
	assert.Equal(t, resp.Header.Get("Vary"), resp2.Header.Get("Vary"))
}

// TestCookieVariedStillPrivateAcrossAllStates re-asserts the phase 0 invariant under
// the new classification, including the legacy escape hatch. This is the thing phase 5
// most needed not to break.
func TestCookieVariedStillPrivateAcrossAllStates(t *testing.T) {
	for _, mode := range []string{bypassCacheControlPreserve, bypassCacheControlNoStore} {
		t.Run(mode, func(t *testing.T) {
			s := prodLikeSidekick(t)
			s.BypassCacheControl = mode

			req := requestWithCookies("/members/dashboard",
				&http.Cookie{Name: "wordpress_sec_abc", Value: "v"})
			dims := s.computeKeyDims(req)
			require.True(t, dims.Cookies)

			for _, state := range []cacheState{
				cacheStateHit, cacheStateMiss, cacheStateNotModified,
				cacheStateBypassPrivate, cacheStateBypassPolicy, cacheStateBypassDebug,
			} {
				hdr := http.Header{}
				hdr.Set("Cache-Control", "public, max-age=600") // origin tries to make it public
				s.applyDownstreamCacheHeaders(hdr, dims, state, &Metadata{Timestamp: 1})

				assert.NotContains(t, hdr.Get("Cache-Control"), "public",
					"cookie-varied responses must never be shareable, state %v", state)
			}
		})
	}
}

func TestBypassState_UnknownReasonIsPrivate(t *testing.T) {
	// Defensive: an unrecognized reason must take the safe branch rather than
	// silently becoming publicly cacheable.
	assert.Equal(t, cacheStateBypassPrivate, bypassState(bypassReason(99)))
	assert.Equal(t, cacheStateBypassPrivate, bypassState(bypassPrivate))
	assert.Equal(t, cacheStateBypassPolicy, bypassState(bypassPolicy))
	assert.Equal(t, cacheStateBypassDebug, bypassState(bypassDebug))
}
