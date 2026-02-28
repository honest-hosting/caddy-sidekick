package sidekick

import (
	"crypto/md5"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestUnmarshalCaddyfile_EmptyCacheKeyValues(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected func(*Sidekick)
	}{
		{
			name: "empty cache_key_queries",
			input: `sidekick {
				cache_key_queries ""
			}`,
			expected: func(s *Sidekick) {
				assert.Empty(t, s.CacheKeyQueries, "cache_key_queries should be empty when given empty string")
			},
		},
		{
			name: "empty cache_key_headers",
			input: `sidekick {
				cache_key_headers ""
			}`,
			expected: func(s *Sidekick) {
				assert.Empty(t, s.CacheKeyHeaders, "cache_key_headers should be empty when given empty string")
			},
		},
		{
			name: "empty cache_key_cookies",
			input: `sidekick {
				cache_key_cookies ""
			}`,
			expected: func(s *Sidekick) {
				assert.Empty(t, s.CacheKeyCookies, "cache_key_cookies should be empty when given empty string")
			},
		},
		{
			name: "mixed empty and non-empty values in cache_key_queries",
			input: `sidekick {
				cache_key_queries "page,,sort,"
			}`,
			expected: func(s *Sidekick) {
				assert.Equal(t, []string{"page", "sort"}, s.CacheKeyQueries, "should filter out empty values")
			},
		},
		{
			name: "non-empty cache_key_queries",
			input: `sidekick {
				cache_key_queries "page,sort,filter"
			}`,
			expected: func(s *Sidekick) {
				assert.Equal(t, []string{"page", "sort", "filter"}, s.CacheKeyQueries, "should preserve non-empty values")
			},
		},
		{
			name: "whitespace-only cache_key_headers",
			input: `sidekick {
				cache_key_headers "  ,  , "
			}`,
			expected: func(s *Sidekick) {
				assert.Empty(t, s.CacheKeyHeaders, "should filter out whitespace-only values")
			},
		},
		{
			name: "multiple arguments with some empty",
			input: `sidekick {
				cache_key_cookies "session" "" "auth"
			}`,
			expected: func(s *Sidekick) {
				assert.Equal(t, []string{"session", "auth"}, s.CacheKeyCookies, "should filter out empty arguments")
			},
		},
		{
			name: "all three with empty values",
			input: `sidekick {
				cache_key_queries ""
				cache_key_headers ""
				cache_key_cookies ""
				cache_dir "/var/cache"
			}`,
			expected: func(s *Sidekick) {
				assert.Empty(t, s.CacheKeyQueries, "cache_key_queries should be empty")
				assert.Empty(t, s.CacheKeyHeaders, "cache_key_headers should be empty")
				assert.Empty(t, s.CacheKeyCookies, "cache_key_cookies should be empty")
				assert.Equal(t, "/var/cache", s.CacheDir, "other settings should work normally")
			},
		},
		{
			name: "whitespace with commas",
			input: `sidekick {
				cache_key_queries " , , "
				cache_key_headers "Accept, ,Content-Type"
			}`,
			expected: func(s *Sidekick) {
				assert.Empty(t, s.CacheKeyQueries, "should be empty when only whitespace")
				assert.Equal(t, []string{"Accept", "Content-Type"}, s.CacheKeyHeaders, "should filter empty but keep valid")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := caddyfile.NewTestDispenser(tt.input)
			s := &Sidekick{}

			err := s.UnmarshalCaddyfile(d)
			assert.NoError(t, err, "UnmarshalCaddyfile should not error")

			tt.expected(s)
		})
	}
}

// TestProvision_DefaultCacheKeyOptions tests that default cache key options are set when not configured
func TestProvision_DefaultCacheKeyOptions(t *testing.T) {
	tests := []struct {
		name            string
		sidekick        *Sidekick
		expectedQueries []string
		expectedHeaders []string
		expectedCookies []string
	}{
		{
			name:            "nil cache key options get defaults",
			sidekick:        &Sidekick{},
			expectedQueries: []string{"p", "page", "paged", "s", "category", "tag", "author"},
			expectedHeaders: []string{"Accept-Encoding"},
			expectedCookies: []string{"wordpress_logged_in_*", "wordpress_sec_*", "wp-settings-*"},
		},
		{
			name: "empty cache key options remain empty",
			sidekick: &Sidekick{
				CacheKeyQueries: []string{},
				CacheKeyHeaders: []string{},
				CacheKeyCookies: []string{},
			},
			expectedQueries: []string{},
			expectedHeaders: []string{},
			expectedCookies: []string{},
		},
		{
			name: "existing cache key options are preserved",
			sidekick: &Sidekick{
				CacheKeyQueries: []string{"custom"},
				CacheKeyHeaders: []string{"X-Custom"},
				CacheKeyCookies: []string{"session_*"},
			},
			expectedQueries: []string{"custom"},
			expectedHeaders: []string{"X-Custom"},
			expectedCookies: []string{"session_*"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Create a minimal context with logger
			ctx := caddy.Context{}

			// Provision the sidekick
			err := tc.sidekick.Provision(ctx)
			require.NoError(t, err)

			// Check the results
			assert.Equal(t, tc.expectedQueries, tc.sidekick.CacheKeyQueries, "CacheKeyQueries mismatch")
			assert.Equal(t, tc.expectedHeaders, tc.sidekick.CacheKeyHeaders, "CacheKeyHeaders mismatch")
			assert.Equal(t, tc.expectedCookies, tc.sidekick.CacheKeyCookies, "CacheKeyCookies mismatch")
		})
	}
}

// TestBuildCacheKey_WildcardCookies tests wildcard cookie matching in cache key generation
func TestBuildCacheKey_WildcardCookies(t *testing.T) {
	tests := []struct {
		name            string
		cookies         []*http.Cookie
		cacheKeyCookies []string
		expectDifferent bool // Whether cache keys should be different
	}{
		{
			name: "wildcard matches prefix",
			cookies: []*http.Cookie{
				{Name: "wordpress_logged_in_abc123", Value: "user1"},
				{Name: "other_cookie", Value: "value"},
			},
			cacheKeyCookies: []string{"wordpress_logged_in_*"},
			expectDifferent: false, // Same cookie pattern should produce same key
		},
		{
			name: "wildcard matches multiple cookies",
			cookies: []*http.Cookie{
				{Name: "wordpress_logged_in_abc", Value: "user1"},
				{Name: "wordpress_sec_xyz", Value: "secure"},
				{Name: "wp-settings-1", Value: "settings"},
			},
			cacheKeyCookies: []string{"wordpress_logged_in_*", "wordpress_sec_*", "wp-settings-*"},
			expectDifferent: false,
		},
		{
			name: "exact match only matches exact name",
			cookies: []*http.Cookie{
				{Name: "session_id", Value: "12345"},
				{Name: "session_id_extra", Value: "67890"},
			},
			cacheKeyCookies: []string{"session_id"}, // No wildcard, exact match only
			expectDifferent: false,
		},
		{
			name: "no matching cookies",
			cookies: []*http.Cookie{
				{Name: "other_cookie", Value: "value"},
			},
			cacheKeyCookies: []string{"wordpress_*"},
			expectDifferent: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := &Sidekick{
				CacheKeyCookies: tc.cacheKeyCookies,
				logger:          zap.NewNop(),
			}

			// Create two requests with the same cookies
			req1 := httptest.NewRequest("GET", "/test", nil)
			req2 := httptest.NewRequest("GET", "/test", nil)

			for _, cookie := range tc.cookies {
				req1.AddCookie(cookie)
				req2.AddCookie(cookie)
			}

			key1 := s.buildCacheKey(req1)
			key2 := s.buildCacheKey(req2)

			if tc.expectDifferent {
				assert.NotEqual(t, key1, key2, "Cache keys should be different")
			} else {
				assert.Equal(t, key1, key2, "Cache keys should be the same")
			}

			// Test that wildcard actually includes the cookie value in the key
			// Only test this if there are cookies that match the pattern
			if len(tc.cookies) > 0 && len(tc.cacheKeyCookies) > 0 {
				// Check if any cookies actually match the pattern
				hasMatchingCookie := false
				for _, cookiePattern := range tc.cacheKeyCookies {
					for _, cookie := range tc.cookies {
						if strings.Contains(cookiePattern, "*") {
							prefix := strings.TrimSuffix(cookiePattern, "*")
							if strings.HasPrefix(cookie.Name, prefix) {
								hasMatchingCookie = true
								break
							}
						} else if cookie.Name == cookiePattern {
							hasMatchingCookie = true
							break
						}
					}
					if hasMatchingCookie {
						break
					}
				}

				if hasMatchingCookie {
					req3 := httptest.NewRequest("GET", "/test", nil)
					// Add cookies with different values
					for _, cookie := range tc.cookies {
						modifiedCookie := &http.Cookie{
							Name:  cookie.Name,
							Value: cookie.Value + "_modified",
						}
						req3.AddCookie(modifiedCookie)
					}

					key3 := s.buildCacheKey(req3)
					// Keys should be different when cookie values change
					assert.NotEqual(t, key1, key3, "Cache keys should differ when cookie values change")
				}
			}
		})
	}
}

// TestBuildCacheKey_WildcardCookieValues tests that cookie values are properly included
func TestBuildCacheKey_WildcardCookieValues(t *testing.T) {
	s := &Sidekick{
		CacheKeyCookies: []string{"test_*"},
		logger:          zap.NewNop(),
	}

	// Create requests with different cookie values
	req1 := httptest.NewRequest("GET", "/test", nil)
	req1.AddCookie(&http.Cookie{Name: "test_cookie", Value: "value1"})

	req2 := httptest.NewRequest("GET", "/test", nil)
	req2.AddCookie(&http.Cookie{Name: "test_cookie", Value: "value2"})

	key1 := s.buildCacheKey(req1)
	key2 := s.buildCacheKey(req2)

	// Keys should be different for different cookie values
	assert.NotEqual(t, key1, key2, "Different cookie values should produce different cache keys")

	// Verify the cookie value is actually used in the hash
	h := md5.New()
	h.Write([]byte("/test"))
	h.Write([]byte("test_cookie=value1"))
	expectedKey1 := fmt.Sprintf("%x", h.Sum(nil))
	assert.Equal(t, expectedKey1, key1, "Cache key should include cookie name and value")
}

// TestShouldBypass_PrefixMatching tests that nocache paths use prefix matching
func TestShouldBypass_PrefixMatching(t *testing.T) {
	tests := []struct {
		name         string
		nocachePaths []string
		requestPath  string
		shouldBypass bool
	}{
		{
			name:         "exact path match",
			nocachePaths: []string{"/wp-admin"},
			requestPath:  "/wp-admin",
			shouldBypass: true,
		},
		{
			name:         "prefix match with trailing path",
			nocachePaths: []string{"/wp-admin"},
			requestPath:  "/wp-admin/index.php",
			shouldBypass: true,
		},
		{
			name:         "prefix match with deep path",
			nocachePaths: []string{"/wp-json"},
			requestPath:  "/wp-json/wp/v2/posts",
			shouldBypass: true,
		},
		{
			name:         "prefix match without slash separator",
			nocachePaths: []string{"/wp-json"},
			requestPath:  "/wp-json-custom",
			shouldBypass: true,
		},
		{
			name:         "no match - different prefix",
			nocachePaths: []string{"/wp-admin"},
			requestPath:  "/content/page",
			shouldBypass: false,
		},
		{
			name:         "no match - prefix not at start",
			nocachePaths: []string{"/admin"},
			requestPath:  "/wp-admin",
			shouldBypass: false,
		},
		{
			name:         "multiple prefixes",
			nocachePaths: []string{"/wp-admin", "/wp-json", "/api"},
			requestPath:  "/api/v1/users",
			shouldBypass: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := &Sidekick{
				NoCache: tc.nocachePaths,
				logger:  zap.NewNop(),
			}

			req := httptest.NewRequest("GET", tc.requestPath, nil)
			bypass := s.shouldBypass(req)

			assert.Equal(t, tc.shouldBypass, bypass, "Bypass result mismatch for path %s", tc.requestPath)
		})
	}
}

func TestSetCacheControlHeaders(t *testing.T) {
	tests := []struct {
		name           string
		cacheTTL       int
		timestamp      int64
		expectCC       string // expected Cache-Control value, "" means not set
		expectAge      string // expected Age value, "" means not set
	}{
		{
			name:      "normal HIT with remaining TTL",
			cacheTTL:  300,
			timestamp: time.Now().Unix() - 100,
			expectCC:  "public, max-age=200",
			expectAge: "100",
		},
		{
			name:      "freshly cached",
			cacheTTL:  300,
			timestamp: time.Now().Unix(),
			expectCC:  "public, max-age=300",
			expectAge: "0",
		},
		{
			name:      "expired entry clamps to zero",
			cacheTTL:  300,
			timestamp: time.Now().Unix() - 350,
			expectCC:  "public, max-age=0",
			expectAge: "350",
		},
		{
			name:      "exactly at TTL boundary",
			cacheTTL:  300,
			timestamp: time.Now().Unix() - 300,
			expectCC:  "public, max-age=0",
			expectAge: "300",
		},
		{
			name:      "TTL disabled (0) - no headers",
			cacheTTL:  0,
			timestamp: time.Now().Unix() - 100,
			expectCC:  "",
			expectAge: "",
		},
		{
			name:      "negative TTL - no headers",
			cacheTTL:  -1,
			timestamp: time.Now().Unix() - 100,
			expectCC:  "",
			expectAge: "",
		},
		{
			name:      "zero timestamp - no headers",
			cacheTTL:  300,
			timestamp: 0,
			expectCC:  "",
			expectAge: "",
		},
		{
			name:      "nil-safe - no headers when TTL disabled",
			cacheTTL:  0,
			timestamp: 0,
			expectCC:  "",
			expectAge: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := &Sidekick{
				CacheTTL: tc.cacheTTL,
			}
			meta := &Metadata{
				Timestamp: tc.timestamp,
			}

			hdr := http.Header{}
			s.setCacheControlHeaders(hdr, meta)

			if tc.expectCC == "" {
				assert.Empty(t, hdr.Get("Cache-Control"), "Cache-Control should not be set")
				assert.Empty(t, hdr.Get("Age"), "Age should not be set")
			} else {
				assert.Equal(t, tc.expectCC, hdr.Get("Cache-Control"))
				assert.Equal(t, tc.expectAge, hdr.Get("Age"))
			}
		})
	}
}

func TestSetCacheControlHeaders_NilMetadata(t *testing.T) {
	s := &Sidekick{CacheTTL: 300}
	hdr := http.Header{}
	s.setCacheControlHeaders(hdr, nil)
	assert.Empty(t, hdr.Get("Cache-Control"), "should not set Cache-Control with nil metadata")
	assert.Empty(t, hdr.Get("Age"), "should not set Age with nil metadata")
}

func TestSetNoCacheHeaders(t *testing.T) {
	hdr := http.Header{}
	setNoCacheHeaders(hdr)
	assert.Equal(t, "no-cache, no-store, must-revalidate", hdr.Get("Cache-Control"))
	assert.Equal(t, "no-cache", hdr.Get("Pragma"))
}

func TestCacheControlHeaders_NotOverwrittenByMetadata(t *testing.T) {
	// Simulate serving a HIT where cached metadata contains origin Cache-Control
	s := &Sidekick{
		CacheTTL: 300,
		logger:   zap.NewNop(),
	}

	meta := &Metadata{
		Timestamp: time.Now().Unix() - 60,
		StateCode: 200,
		Header: [][]string{
			{"Content-Type", "text/html"},
			{"Cache-Control", "max-age=3600"}, // origin value that should be skipped
			{"Age", "999"},                     // origin value that should be skipped
			{"Pragma", "no-cache"},             // origin value that should be skipped
		},
	}

	hdr := http.Header{}

	// Set computed cache-control headers (as done in HIT path)
	s.setCacheControlHeaders(hdr, meta)

	// Simulate the metadata restoration loop with the skip logic
	for _, kv := range meta.Header {
		if len(kv) != 2 {
			continue
		}
		if kv[0] == "Content-Encoding" || kv[0] == "Content-Length" ||
			kv[0] == "Cache-Control" || kv[0] == "Age" || kv[0] == "Pragma" {
			continue
		}
		hdr.Set(kv[0], kv[1])
	}

	// Verify our computed values survived
	assert.Equal(t, "public, max-age=240", hdr.Get("Cache-Control"),
		"Cache-Control should be the computed value, not the origin value")
	assert.Equal(t, "60", hdr.Get("Age"),
		"Age should be the computed value, not the origin value")
	assert.Equal(t, "text/html", hdr.Get("Content-Type"),
		"Content-Type should still be restored from metadata")
}
