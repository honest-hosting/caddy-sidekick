package integration_test

import (
	"io"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

const (
	baseURL    = "http://localhost:8989"
	purgeToken = "test-token-12345"
	purgeURL   = "/__sidekick/purge"
)

// Test configuration
const (
	// Default number of requests to verify cache HITs
	defaultVerifyRequestCount = 10
	// Default wait time between verification requests
	defaultVerifyRequestDelay = 50 * time.Millisecond
	// Default wait time after cache write
	defaultCacheWriteWait = 500 * time.Millisecond
)

// testConfig holds configuration for test execution
type testConfig struct {
	verifyRequestCount int
	verifyRequestDelay time.Duration
	cacheWriteWait     time.Duration
}

// newTestConfig creates a test configuration with defaults
func newTestConfig() *testConfig {
	return &testConfig{
		verifyRequestCount: defaultVerifyRequestCount,
		verifyRequestDelay: defaultVerifyRequestDelay,
		cacheWriteWait:     defaultCacheWriteWait,
	}
}

// TestIntegrationMemoryCacheBasic tests basic memory cache functionality
func TestIntegrationMemoryCacheBasic(t *testing.T) {
	cfg := newTestConfig()

	// Clear cache first
	purgeCache(t, nil)
	time.Sleep(1 * time.Second)

	// First request - should be MISS
	resp1, body1 := makeRequest(t, "GET", baseURL+"/cacheable", nil, nil)
	if resp1.StatusCode != 200 {
		t.Fatalf("Expected status 200, got %d", resp1.StatusCode)
	}
	checkCacheHeader(t, resp1, "MISS")

	// Wait for cache to be written
	time.Sleep(cfg.cacheWriteWait)

	// Verify multiple requests return HIT
	bodies := verifyCacheHits(t, baseURL+"/cacheable", nil, nil, cfg)

	// All cached bodies should be identical to the original
	for i, body := range bodies {
		if diff := cmp.Diff(body, body1); diff != "" {
			t.Errorf("Cached response body %d differs from original diff:", i+1)
			t.Error(diff)
		}
	}
}

// TestIntegrationCompressedResponses tests caching of compressed responses
func TestIntegrationCompressedResponses(t *testing.T) {
	cfg := newTestConfig()

	testCases := []struct {
		name     string
		encoding string
		decoder  func(io.Reader) (string, error)
	}{
		{
			name:     "gzip",
			encoding: "gzip",
			decoder:  decodeGzip,
		},
		{
			name:     "br",
			encoding: "br",
			decoder:  decodeBrotli,
		},
		{
			name:     "zstd",
			encoding: "zstd",
			decoder:  decodeZstd,
		},
		{
			name:     "identity",
			encoding: "identity",
			decoder:  decodeIdentity,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Clear cache for each test
			purgeCache(t, nil)
			time.Sleep(1 * time.Second)

			headers := map[string]string{
				"Accept-Encoding": tc.encoding,
			}

			// First request - should be MISS
			resp1 := makeRequestRaw(t, "GET", baseURL+"/cacheable", nil, headers)
			defer func() { _ = resp1.Body.Close() }()

			if resp1.StatusCode != 200 {
				t.Fatalf("Expected status 200, got %d", resp1.StatusCode)
			}
			checkCacheHeader(t, resp1, "MISS")

			// Decode the response body
			body1, err := tc.decoder(resp1.Body)
			if err != nil {
				t.Fatalf("Failed to decode %s response: %v", tc.encoding, err)
			}

			// Verify content encoding header if compressed
			if tc.encoding != "identity" {
				contentEncoding := resp1.Header.Get("Content-Encoding")
				if contentEncoding != tc.encoding {
					t.Errorf("Expected Content-Encoding: %s, got: %s", tc.encoding, contentEncoding)
				}
			}

			// Wait for cache to be written
			time.Sleep(cfg.cacheWriteWait)

			// Verify multiple requests return HIT with correct compression
			for i := 0; i < cfg.verifyRequestCount; i++ {
				resp := makeRequestRaw(t, "GET", baseURL+"/cacheable", nil, headers)
				defer func() { _ = resp.Body.Close() }()

				checkCacheHeader(t, resp, "HIT")

				// Decode and compare body
				body, err := tc.decoder(resp.Body)
				if err != nil {
					t.Fatalf("Request %d: Failed to decode %s response: %v", i+1, tc.encoding, err)
				}

				if diff := cmp.Diff(string(body), string(body1)); diff != "" {
					t.Errorf("Request %d: Cached %s response body differs from original, diff:", i+1, tc.encoding)
					t.Error(diff)
				}

				// Verify content encoding header consistency
				if tc.encoding != "identity" {
					contentEncoding := resp.Header.Get("Content-Encoding")
					if contentEncoding != tc.encoding {
						t.Errorf("Request %d: Expected Content-Encoding: %s, got: %s", i+1, tc.encoding, contentEncoding)
					}
				}

				if i < cfg.verifyRequestCount-1 {
					time.Sleep(cfg.verifyRequestDelay)
				}
			}
		})
	}
}

// TestIntegrationMixedCompressionRequests tests that the same content can be served with different encodings from cache
func TestIntegrationMixedCompressionRequests(t *testing.T) {
	cfg := newTestConfig()
	cfg.verifyRequestCount = 5 // Fewer requests for this test

	// Clear cache
	purgeCache(t, nil)
	time.Sleep(1 * time.Second)

	// Request with gzip
	gzipHeaders := map[string]string{"Accept-Encoding": "gzip"}
	resp1 := makeRequestRaw(t, "GET", baseURL+"/cacheable", nil, gzipHeaders)
	defer func() { _ = resp1.Body.Close() }()
	checkCacheHeader(t, resp1, "MISS")
	gzipBody1, _ := decodeGzip(resp1.Body)

	time.Sleep(cfg.cacheWriteWait)

	// Request with br
	brHeaders := map[string]string{"Accept-Encoding": "br"}
	resp2 := makeRequestRaw(t, "GET", baseURL+"/cacheable", nil, brHeaders)
	defer func() { _ = resp2.Body.Close() }()
	checkCacheHeader(t, resp2, "HIT") // Should be HIT - same content, different encoding
	brBody1, _ := decodeBrotli(resp2.Body)

	time.Sleep(cfg.cacheWriteWait)

	// Request with zstd
	zstdHeaders := map[string]string{"Accept-Encoding": "zstd"}
	resp3 := makeRequestRaw(t, "GET", baseURL+"/cacheable", nil, zstdHeaders)
	defer func() { _ = resp3.Body.Close() }()
	checkCacheHeader(t, resp3, "HIT") // Should be HIT - same content, different encoding
	zstdBody1, _ := decodeZstd(resp3.Body)

	time.Sleep(cfg.cacheWriteWait)

	// Verify each encoding is cached separately
	for i := 0; i < cfg.verifyRequestCount; i++ {
		// Verify gzip cache HIT
		respGzip := makeRequestRaw(t, "GET", baseURL+"/cacheable", nil, gzipHeaders)
		defer func() { _ = respGzip.Body.Close() }()
		checkCacheHeader(t, respGzip, "HIT")
		gzipBody, _ := decodeGzip(respGzip.Body)
		if gzipBody != gzipBody1 {
			t.Errorf("Iteration %d: Gzip cached body differs", i+1)
		}

		// Verify br cache HIT
		respBr := makeRequestRaw(t, "GET", baseURL+"/cacheable", nil, brHeaders)
		defer func() { _ = respBr.Body.Close() }()
		checkCacheHeader(t, respBr, "HIT")
		brBody, _ := decodeBrotli(respBr.Body)
		if brBody != brBody1 {
			t.Errorf("Iteration %d: Brotli cached body differs", i+1)
		}

		// Verify zstd cache HIT
		respZstd := makeRequestRaw(t, "GET", baseURL+"/cacheable", nil, zstdHeaders)
		defer func() { _ = respZstd.Body.Close() }()
		checkCacheHeader(t, respZstd, "HIT")
		zstdBody, _ := decodeZstd(respZstd.Body)
		if zstdBody != zstdBody1 {
			t.Errorf("Iteration %d: Zstd cached body differs", i+1)
		}

		if i < cfg.verifyRequestCount-1 {
			time.Sleep(cfg.verifyRequestDelay)
		}
	}
}

// TestIntegrationCompressedBypassPaths tests that bypass paths work correctly with compression
func TestIntegrationCompressedBypassPaths(t *testing.T) {
	cfg := newTestConfig()
	cfg.verifyRequestCount = 5

	// Clear cache
	purgeCache(t, nil)
	time.Sleep(1 * time.Second)

	encodings := []string{"gzip", "br", "zstd", "identity"}

	for _, encoding := range encodings {
		t.Run(encoding, func(t *testing.T) {
			headers := map[string]string{"Accept-Encoding": encoding}

			// Test nocache path with compression
			for i := 0; i < cfg.verifyRequestCount; i++ {
				resp, _ := makeRequest(t, "GET", baseURL+"/wp-admin/index.php", nil, headers)
				checkCacheHeader(t, resp, "BYPASS")

				if i < cfg.verifyRequestCount-1 {
					time.Sleep(cfg.verifyRequestDelay)
				}
			}
		})
	}
}

// TestIntegrationDiskCacheLargeFile tests disk caching for large responses
func TestIntegrationDiskCacheLargeFile(t *testing.T) {
	cfg := newTestConfig()

	// Clear cache first
	purgeCache(t, nil)
	time.Sleep(1 * time.Second)

	// Request a large file (2MB) that should go to disk
	largeURL := baseURL + "/large?size=2097152"

	// First request - should be MISS
	resp1, _ := makeRequest(t, "GET", largeURL, nil, nil)
	if resp1.StatusCode != 200 {
		t.Fatalf("Expected status 200, got %d", resp1.StatusCode)
	}
	checkCacheHeader(t, resp1, "MISS")

	// Wait for cache write
	time.Sleep(1 * time.Second) // Longer wait for large file

	// Verify multiple requests return HIT
	verifyCacheHits(t, largeURL, nil, nil, cfg)
}

// TestIntegrationCacheResponseCodes tests caching of different HTTP status codes
func TestIntegrationCacheResponseCodes(t *testing.T) {
	cfg := newTestConfig()

	// Clear cache
	purgeCache(t, nil)
	time.Sleep(1 * time.Second)

	// Test 404 caching (configured as NOT cacheable - not in cache_response_codes)
	for i := 0; i < cfg.verifyRequestCount; i++ {
		resp, _ := makeRequest(t, "GET", baseURL+"/error/404", nil, nil)
		if resp.StatusCode != 404 {
			t.Fatalf("Expected status 404, got %d", resp.StatusCode)
		}
		checkCacheHeader(t, resp, "BYPASS")

		if i < cfg.verifyRequestCount-1 {
			time.Sleep(cfg.verifyRequestDelay)
		}
	}

	// Test 500 (should not be cached)
	bodies := make([]string, cfg.verifyRequestCount)
	for i := 0; i < cfg.verifyRequestCount; i++ {
		resp, body := makeRequest(t, "GET", baseURL+"/error/500", nil, nil)
		if resp.StatusCode != 500 {
			t.Fatalf("Expected status 500, got %d", resp.StatusCode)
		}
		checkCacheHeader(t, resp, "BYPASS")
		bodies[i] = body

		if i < cfg.verifyRequestCount-1 {
			time.Sleep(cfg.verifyRequestDelay)
		}
	}

	// Bodies should differ (not cached, contains timestamp)
	allSame := true
	for i := 1; i < len(bodies); i++ {
		if bodies[i] != bodies[0] {
			allSame = false
			break
		}
	}
	if allSame {
		t.Error("500 error responses should not be cached but all bodies are identical")
	}
}

// TestIntegrationPurgeSpecificPaths tests purging specific cache paths
func TestIntegrationPurgeSpecificPaths(t *testing.T) {
	cfg := newTestConfig()
	cfg.verifyRequestCount = 3 // Fewer for this test

	// Clear cache
	purgeCache(t, nil)
	time.Sleep(1 * time.Second)

	// Cache multiple paths
	paths := []string{"/cacheable", "/static/page", "/api/data"}
	for _, path := range paths {
		resp, _ := makeRequest(t, "GET", baseURL+path, nil, nil)
		if resp.StatusCode != 200 {
			t.Fatalf("Failed to cache %s, status: %d", path, resp.StatusCode)
		}
		checkCacheHeader(t, resp, "MISS")
	}

	time.Sleep(cfg.cacheWriteWait)

	// Verify all are cached with multiple requests
	for _, path := range paths {
		for i := 0; i < cfg.verifyRequestCount; i++ {
			resp, _ := makeRequest(t, "GET", baseURL+path, nil, nil)
			checkCacheHeader(t, resp, "HIT")
		}
	}

	// Purge only specific path
	purgeCache(t, []string{"/cacheable"})
	time.Sleep(1 * time.Second)

	// Check that purged path returns MISS then HIT
	resp1, _ := makeRequest(t, "GET", baseURL+"/cacheable", nil, nil)
	checkCacheHeader(t, resp1, "MISS")

	time.Sleep(cfg.cacheWriteWait)

	// Verify it's cached again
	for i := 0; i < cfg.verifyRequestCount; i++ {
		resp, _ := makeRequest(t, "GET", baseURL+"/cacheable", nil, nil)
		checkCacheHeader(t, resp, "HIT")
	}

	// Other paths should still be cached
	for _, path := range []string{"/static/page", "/api/data"} {
		for i := 0; i < cfg.verifyRequestCount; i++ {
			resp, _ := makeRequest(t, "GET", baseURL+path, nil, nil)
			checkCacheHeader(t, resp, "HIT")
		}
	}
}

// TestIntegrationPurgeWildcard tests wildcard purging functionality
func TestIntegrationPurgeWildcard(t *testing.T) {
	cfg := newTestConfig()
	cfg.verifyRequestCount = 3

	// Clear cache
	purgeCache(t, nil)
	time.Sleep(1 * time.Second)

	// Cache multiple paths with hierarchy
	testPaths := []string{
		"/path/subdir/image1.png",
		"/path/subdir/image2.png",
		"/path/image123.png",
		"/path/image1.png",
	}

	for _, path := range testPaths {
		resp, _ := makeRequest(t, "GET", baseURL+path, nil, nil)
		if resp.StatusCode != 200 {
			t.Fatalf("Failed to cache %s, status: %d", path, resp.StatusCode)
		}
		checkCacheHeader(t, resp, "MISS")
	}

	time.Sleep(cfg.cacheWriteWait)

	// Verify all are cached
	for _, path := range testPaths {
		for i := 0; i < cfg.verifyRequestCount; i++ {
			resp, _ := makeRequest(t, "GET", baseURL+path, nil, nil)
			checkCacheHeader(t, resp, "HIT")
		}
	}

	// Purge with wildcard pattern /path/subdir/*
	purgeCache(t, []string{"/path/subdir/*"})
	time.Sleep(500 * time.Millisecond)

	// Check that paths under /path/subdir/ return MISS
	for _, path := range []string{"/path/subdir/image1.png", "/path/subdir/image2.png"} {
		resp, _ := makeRequest(t, "GET", baseURL+path, nil, nil)
		checkCacheHeader(t, resp, "MISS")
	}

	// But other paths should still be HIT
	for _, path := range []string{"/path/image123.png", "/path/image1.png"} {
		for i := 0; i < cfg.verifyRequestCount; i++ {
			resp, _ := makeRequest(t, "GET", baseURL+path, nil, nil)
			checkCacheHeader(t, resp, "HIT")
		}
	}
}

// TestIntegrationNoCachePaths tests that nocache paths are never cached
func TestIntegrationNoCachePaths(t *testing.T) {
	cfg := newTestConfig()

	// Clear cache
	purgeCache(t, nil)
	time.Sleep(1 * time.Second)

	// Test wp-admin path (configured as nocache)
	bodies := make([]string, cfg.verifyRequestCount)
	for i := 0; i < cfg.verifyRequestCount; i++ {
		resp, body := makeRequest(t, "GET", baseURL+"/wp-admin/index.php", nil, nil)
		if resp.StatusCode != 200 {
			t.Fatalf("Request %d: Expected status 200, got %d", i+1, resp.StatusCode)
		}
		checkCacheHeader(t, resp, "BYPASS")
		bodies[i] = body

		if i < cfg.verifyRequestCount-1 {
			time.Sleep(cfg.verifyRequestDelay)
		}
	}

	// Bodies should be different (not cached, contains timestamp)
	allSame := true
	for i := 1; i < len(bodies); i++ {
		if bodies[i] != bodies[0] {
			allSame = false
			break
		}
	}
	if allSame {
		t.Error("Nocache path responses should not be cached but all bodies are identical")
	}

	// Test wp-json path (configured as nocache)
	bodies2 := make([]string, cfg.verifyRequestCount)
	for i := 0; i < cfg.verifyRequestCount; i++ {
		resp, body := makeRequest(t, "GET", baseURL+"/wp-json/wp/v2/posts", nil, nil)
		checkCacheHeader(t, resp, "BYPASS")
		bodies2[i] = body

		if i < cfg.verifyRequestCount-1 {
			time.Sleep(cfg.verifyRequestDelay)
		}
	}

	allSame = true
	for i := 1; i < len(bodies2); i++ {
		if bodies2[i] != bodies2[0] {
			allSame = false
			break
		}
	}
	if allSame {
		t.Error("wp-json responses should not be cached but all bodies are identical")
	}
}

// TestIntegrationNoCacheHome tests home page caching behavior
func TestIntegrationNoCacheHome(t *testing.T) {
	cfg := newTestConfig()

	// Clear cache
	purgeCache(t, nil)
	time.Sleep(1 * time.Second)

	// Test home page (nocache_home is false, so it should be cached)
	resp1, body1 := makeRequest(t, "GET", baseURL+"/", nil, nil)
	if resp1.StatusCode != 200 {
		t.Fatalf("Expected status 200, got %d", resp1.StatusCode)
	}
	checkCacheHeader(t, resp1, "MISS")

	time.Sleep(cfg.cacheWriteWait)

	// Verify multiple requests return HIT
	bodies := verifyCacheHits(t, baseURL+"/", nil, nil, cfg)

	// All should be identical (cached)
	for i, body := range bodies {
		if body != body1 {
			t.Errorf("Request %d: Home page should be cached but body differs", i+1)
		}
	}
}

// TestIntegrationQueryParameterCaching tests cache key generation with query parameters
func TestIntegrationQueryParameterCaching(t *testing.T) {
	cfg := newTestConfig()
	cfg.verifyRequestCount = 5

	// Clear cache
	purgeCache(t, nil)
	time.Sleep(1 * time.Second)

	// Request with query parameters that are in cache_key_queries
	url1 := baseURL + "/dynamic?name=Alice&page=1"
	resp1, body1 := makeRequest(t, "GET", url1, nil, nil)
	checkCacheHeader(t, resp1, "MISS")

	time.Sleep(cfg.cacheWriteWait)

	// Verify multiple requests return HIT
	bodies := verifyCacheHits(t, url1, nil, nil, cfg)
	for _, body := range bodies {
		if body != body1 {
			t.Error("Same query parameters should return cached response")
		}
	}

	// Different query parameter value - should be MISS
	url2 := baseURL + "/dynamic?name=Bob&page=1"
	resp3, _ := makeRequest(t, "GET", url2, nil, nil)
	checkCacheHeader(t, resp3, "MISS")

	// Parameter not in cache_key_queries - should still hit cache for first URL
	url3 := baseURL + "/dynamic?name=Alice&page=1&ignored=value"
	resp4, body4 := makeRequest(t, "GET", url3, nil, nil)
	checkCacheHeader(t, resp4, "HIT")
	if body1 != body4 {
		t.Error("Ignored query parameter should not affect cache key")
	}
}

// TestIntegrationConditionalRequests tests ETag and Last-Modified support
func TestIntegrationConditionalRequests(t *testing.T) {
	cfg := newTestConfig()

	// Clear cache
	purgeCache(t, nil)
	time.Sleep(1 * time.Second)

	// First request to get ETag
	resp1, _ := makeRequest(t, "GET", baseURL+"/conditional", nil, nil)
	if resp1.StatusCode != 200 {
		t.Fatalf("Expected status 200, got %d", resp1.StatusCode)
	}
	etag := resp1.Header.Get("ETag")
	lastModified := resp1.Header.Get("Last-Modified")

	if etag == "" {
		t.Fatal("No ETag header in response")
	}

	// Request with If-None-Match multiple times
	headers := map[string]string{
		"If-None-Match": etag,
	}
	for i := 0; i < cfg.verifyRequestCount; i++ {
		resp, _ := makeRequest(t, "GET", baseURL+"/conditional", nil, headers)
		if resp.StatusCode != 304 {
			t.Errorf("Request %d: Expected 304 Not Modified with If-None-Match, got %d", i+1, resp.StatusCode)
		}
	}

	// Request with If-Modified-Since multiple times
	headers = map[string]string{
		"If-Modified-Since": lastModified,
	}
	for i := 0; i < cfg.verifyRequestCount; i++ {
		resp, _ := makeRequest(t, "GET", baseURL+"/conditional", nil, headers)
		if resp.StatusCode != 304 {
			t.Errorf("Request %d: Expected 304 Not Modified with If-Modified-Since, got %d", i+1, resp.StatusCode)
		}
	}
}

// TestIntegrationCacheHeaderVariations tests cache variations based on headers
func TestIntegrationCacheHeaderVariations(t *testing.T) {
	cfg := newTestConfig()
	cfg.verifyRequestCount = 5

	// Clear cache
	purgeCache(t, nil)
	time.Sleep(1 * time.Second)

	// Request with specific header
	headers1 := map[string]string{
		"Accept-Language": "en-US",
	}
	resp1, body1 := makeRequest(t, "GET", baseURL+"/cacheable", nil, headers1)
	checkCacheHeader(t, resp1, "MISS")

	time.Sleep(cfg.cacheWriteWait)

	// Verify multiple requests with same header return HIT
	bodies := verifyCacheHits(t, baseURL+"/cacheable", nil, headers1, cfg)
	for _, body := range bodies {
		if body != body1 {
			t.Error("Same headers should return cached response")
		}
	}

	// Different header value - should be MISS (new cache entry)
	headers2 := map[string]string{
		"Accept-Language": "fr-FR",
	}
	resp3, _ := makeRequest(t, "GET", baseURL+"/cacheable", nil, headers2)
	checkCacheHeader(t, resp3, "MISS")

	time.Sleep(cfg.cacheWriteWait)

	// Verify both variations are cached
	for i := 0; i < cfg.verifyRequestCount; i++ {
		resp4, _ := makeRequest(t, "GET", baseURL+"/cacheable", nil, headers1)
		checkCacheHeader(t, resp4, "HIT")

		resp5, _ := makeRequest(t, "GET", baseURL+"/cacheable", nil, headers2)
		checkCacheHeader(t, resp5, "HIT")
	}
}

// TestIntegrationPurgeAll tests purging all cache entries
func TestIntegrationPurgeAll(t *testing.T) {
	cfg := newTestConfig()
	cfg.verifyRequestCount = 3

	// Clear cache first
	purgeCache(t, nil)
	time.Sleep(1 * time.Second)

	// Cache multiple different paths
	paths := []string{
		"/cacheable",
		"/static/page",
		"/api/data",
		"/path/image1.png",
		"/dynamic?name=test&page=1",
	}

	for _, path := range paths {
		resp, _ := makeRequest(t, "GET", baseURL+path, nil, nil)
		if resp.StatusCode != 200 {
			t.Fatalf("Failed to cache %s, status: %d", path, resp.StatusCode)
		}
	}

	time.Sleep(cfg.cacheWriteWait)

	// Verify all are cached
	for _, path := range paths {
		for i := 0; i < cfg.verifyRequestCount; i++ {
			resp, _ := makeRequest(t, "GET", baseURL+path, nil, nil)
			checkCacheHeader(t, resp, "HIT")
		}
	}

	// Purge all cache
	purgeCache(t, nil)
	time.Sleep(1 * time.Second)

	// Verify all return MISS
	for _, path := range paths {
		resp, _ := makeRequest(t, "GET", baseURL+path, nil, nil)
		if strings.Contains(path, "wp-admin") || strings.Contains(path, "wp-json") {
			checkCacheHeader(t, resp, "BYPASS")
		} else {
			checkCacheHeader(t, resp, "MISS")
		}
	}
}

// TestIntegrationConcurrentRequests tests cache behavior under concurrent load
func TestIntegrationConcurrentRequests(t *testing.T) {
	cfg := newTestConfig()

	// Clear cache
	purgeCache(t, nil)
	time.Sleep(1 * time.Second)

	// Make concurrent requests
	concurrency := 20
	done := make(chan bool, concurrency)

	for i := 0; i < concurrency; i++ {
		go func(id int) {
			resp, _ := makeRequest(t, "GET", baseURL+"/cacheable", nil, nil)
			if resp.StatusCode != 200 {
				t.Errorf("Goroutine %d: Expected status 200, got %d", id, resp.StatusCode)
			}
			// First few might be MISS, rest should be HIT
			cacheHeader := resp.Header.Get("X-Sidekick-Cache")
			if cacheHeader != "HIT" && cacheHeader != "MISS" {
				t.Errorf("Goroutine %d: Unexpected cache header: %s", id, cacheHeader)
			}
			done <- true
		}(i)
	}

	// Wait for all goroutines
	for i := 0; i < concurrency; i++ {
		<-done
	}

	// Final requests should definitely be HIT
	time.Sleep(cfg.cacheWriteWait)

	verifyCacheHits(t, baseURL+"/cacheable", nil, nil, cfg)
}

// TestIntegrationNoCacheRegex tests regex-based cache bypass
func TestIntegrationNoCacheRegex(t *testing.T) {
	cfg := newTestConfig()
	cfg.verifyRequestCount = 3

	// Clear cache
	purgeCache(t, nil)
	time.Sleep(1 * time.Second)

	// Test that configured file types in nocache_regex are bypassed
	// According to Caddyfile, .png should be cacheable (not in regex)
	resp1, _ := makeRequest(t, "GET", baseURL+"/path/image1.png", nil, nil)
	checkCacheHeader(t, resp1, "MISS")

	time.Sleep(cfg.cacheWriteWait)

	verifyCacheHits(t, baseURL+"/path/image1.png", nil, nil, cfg)

	// Test file types that SHOULD be in nocache_regex (bypassed)
	bypassExtensions := []string{
		"/test.pdf",
		"/file.mp4",
		"/audio.mp3",
		"/archive.zip",
		"/package.tar",
		"/compressed.gz",
		"/program.exe",
	}

	for _, path := range bypassExtensions {
		for i := 0; i < cfg.verifyRequestCount; i++ {
			resp, _ := makeRequest(t, "GET", baseURL+path, nil, nil)
			cacheHeader := resp.Header.Get("X-Sidekick-Cache")
			if cacheHeader != "BYPASS" {
				t.Errorf("Request %d for %s: Expected BYPASS due to nocache_regex, got header: %s (status: %d)",
					i+1, path, cacheHeader, resp.StatusCode)
			}

			if i < cfg.verifyRequestCount-1 {
				time.Sleep(cfg.verifyRequestDelay)
			}
		}
	}

	// Test file types that should NOT be in nocache_regex and actually exist (return 200)
	// These should be properly cached
	existingCacheablePaths := []string{
		"/cacheable",   // A known endpoint that returns 200
		"/static/page", // Another endpoint that returns 200
	}

	for _, path := range existingCacheablePaths {
		// Clear cache for this specific test
		purgeCache(t, []string{path})
		time.Sleep(500 * time.Millisecond)

		// First request should be MISS
		resp1, _ := makeRequest(t, "GET", baseURL+path, nil, nil)
		if resp1.StatusCode != 200 {
			t.Errorf("Expected %s to return 200, got %d", path, resp1.StatusCode)
			continue
		}
		cacheHeader1 := resp1.Header.Get("X-Sidekick-Cache")
		if cacheHeader1 != "MISS" {
			t.Errorf("Expected %s first request to be MISS, got header: %s", path, cacheHeader1)
		}

		time.Sleep(cfg.cacheWriteWait)

		// Verify multiple requests return HIT
		verifyCacheHits(t, baseURL+path, nil, nil, cfg)
	}
}
