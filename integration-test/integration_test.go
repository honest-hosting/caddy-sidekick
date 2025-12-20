package integration_test

import (
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
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

	// Test 404 caching (configured as cacheable - in cache_response_codes)
	for i := 0; i < cfg.verifyRequestCount; i++ {
		resp, _ := makeRequest(t, "GET", baseURL+"/error/404", nil, nil)
		if resp.StatusCode != 404 {
			t.Fatalf("Expected status 404, got %d", resp.StatusCode)
		}

		if i == 0 {
			checkCacheHeader(t, resp, "MISS")
		} else {
			checkCacheHeader(t, resp, "HIT")
		}

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

// TestIntegrationMetrics tests that metrics are properly exposed and incremented
func TestIntegrationMetrics(t *testing.T) {
	metricsURL := "http://localhost:9443/metrics/sidekick"
	cfg := newTestConfig()

	// Clear cache first
	purgeCache(t, nil)
	time.Sleep(1 * time.Second)

	// Helper function to fetch metrics
	fetchMetrics := func(t *testing.T) string {
		resp := makeRequestRaw(t, "GET", metricsURL, nil, nil)
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		return string(body)
	}

	// Helper to check if metric labels match wanted labels
	matchesLabels := func(metricLabels []*dto.LabelPair, wantLabels map[string]string) bool {
		if len(wantLabels) == 0 {
			return len(metricLabels) == 0
		}

		foundLabels := make(map[string]string)
		for _, label := range metricLabels {
			if label.Name != nil && label.Value != nil {
				foundLabels[*label.Name] = *label.Value
			}
		}

		for k, v := range wantLabels {
			if foundLabels[k] != v {
				return false
			}
		}
		return true
	}

	// Helper function to parse metrics and get value by name and labels
	getMetricValue := func(metricsText string, metricName string, labelPairs ...string) (float64, bool) {
		// Create parser with validation scheme to avoid panic
		parser := expfmt.NewTextParser(model.LegacyValidation)
		metricFamilies, err := parser.TextToMetricFamilies(strings.NewReader(metricsText))
		if err != nil {
			t.Logf("Failed to parse metrics: %v", err)
			return 0, false
		}

		family, ok := metricFamilies[metricName]
		if !ok {
			return 0, false
		}

		// Build label map from pairs
		wantLabels := make(map[string]string)
		for i := 0; i < len(labelPairs)-1; i += 2 {
			wantLabels[labelPairs[i]] = labelPairs[i+1]
		}

		// Find matching metric
		for _, metric := range family.Metric {
			if matchesLabels(metric.Label, wantLabels) {
				switch family.GetType() {
				case dto.MetricType_COUNTER:
					if metric.Counter != nil && metric.Counter.Value != nil {
						return *metric.Counter.Value, true
					}
				case dto.MetricType_GAUGE:
					if metric.Gauge != nil && metric.Gauge.Value != nil {
						return *metric.Gauge.Value, true
					}
				case dto.MetricType_HISTOGRAM:
					if metric.Histogram != nil && metric.Histogram.SampleCount != nil {
						return float64(*metric.Histogram.SampleCount), true
					}
				}
			}
		}
		return 0, false
	}

	// Test 1: Verify metrics endpoint is accessible
	t.Run("MetricsEndpointAccessible", func(t *testing.T) {
		metrics := fetchMetrics(t)
		if !strings.Contains(metrics, "# HELP") {
			t.Error("Metrics endpoint doesn't return Prometheus format")
		}
		if !strings.Contains(metrics, "# TYPE") {
			t.Error("Metrics endpoint missing TYPE declarations")
		}
	})

	// Test 2: Verify sidekick metrics are registered
	t.Run("SidekickMetricsRegistered", func(t *testing.T) {
		metrics := fetchMetrics(t)

		requiredMetrics := []string{
			"caddy_sidekick_cache_used_bytes",
			"caddy_sidekick_cache_limit_bytes",
			"caddy_sidekick_cache_used_count",
			"caddy_sidekick_cache_limit_count",

			"caddy_sidekick_cache_requests_total",
			"caddy_sidekick_response_time_ms",
		}

		for _, metric := range requiredMetrics {
			if !strings.Contains(metrics, metric) {
				t.Errorf("Required metric %s not found in output", metric)
			}
		}
	})

	// Test 3: Verify cache operations increment correctly
	t.Run("CacheOperationsIncrement", func(t *testing.T) {
		// Get initial metrics
		initialMetrics := fetchMetrics(t)
		initialHits, _ := getMetricValue(initialMetrics, "caddy_sidekick_cache_requests_total", "result", "hit", "cache_type", "total")
		initialMisses, _ := getMetricValue(initialMetrics, "caddy_sidekick_cache_requests_total", "result", "miss", "cache_type", "total")

		// Make a request that should be a MISS
		resp1, _ := makeRequest(t, "GET", baseURL+"/metrics-test-1", nil, nil)
		defer func() { _ = resp1.Body.Close() }()

		// Check it was a MISS
		if resp1.Header.Get("X-Sidekick-Cache") != "MISS" {
			t.Errorf("Expected MISS, got %s", resp1.Header.Get("X-Sidekick-Cache"))
		}

		time.Sleep(cfg.cacheWriteWait)

		// Make the same request again - should be a HIT
		resp2, _ := makeRequest(t, "GET", baseURL+"/metrics-test-1", nil, nil)
		defer func() { _ = resp2.Body.Close() }()

		// Check it was a HIT
		if resp2.Header.Get("X-Sidekick-Cache") != "HIT" {
			t.Errorf("Expected HIT, got %s", resp2.Header.Get("X-Sidekick-Cache"))
		}

		// Get updated metrics
		time.Sleep(2250 * time.Millisecond) // Allow metrics to update
		updatedMetrics := fetchMetrics(t)
		updatedHits, _ := getMetricValue(updatedMetrics, "caddy_sidekick_cache_requests_total", "result", "hit", "cache_type", "total")
		updatedMisses, _ := getMetricValue(updatedMetrics, "caddy_sidekick_cache_requests_total", "result", "miss", "cache_type", "total")

		// Verify counters increased
		if initialHits >= updatedHits {
			t.Errorf("Hit counter did not increase after cache HIT: initial=%f, updated=%f", initialHits, updatedHits)
		}
		if initialMisses >= updatedMisses {
			t.Errorf("Miss counter did not increase after cache MISS: initial=%f, updated=%f", initialMisses, updatedMisses)
		}
	})

	// Test 4: Verify memory and disk cache metrics
	t.Run("CacheStorageMetrics", func(t *testing.T) {
		// Clear cache first to start fresh
		purgeCache(t, nil)
		time.Sleep(1 * time.Second)

		// cache_memory_item_max_size is set to 1MB in Caddyfile
		// Files larger than 1MB should go to disk

		// Make requests with small responses that should stay in memory (<1MB)
		for i := 0; i < 3; i++ {
			// Create paths that generate small responses
			path := "/small-test-" + string(rune('a'+i))
			resp, body := makeRequest(t, "GET", baseURL+path, nil, nil)
			defer func() { _ = resp.Body.Close() }()

			// Log the actual response size for debugging
			t.Logf("Small response size for %s: %d bytes", path, len(body))

			if resp.Header.Get("X-Sidekick-Cache") != "MISS" {
				t.Errorf("Expected MISS on first request, got %s", resp.Header.Get("X-Sidekick-Cache"))
			}
		}

		// Now make a request for a file that's larger than 1MB and should go to disk
		largePath := "/test-generate-large-file-mb?size=1.2"
		resp, body := makeRequest(t, "GET", baseURL+largePath, nil, nil)
		defer func() { _ = resp.Body.Close() }()
		t.Logf("Large file response size: %d bytes (should go to disk)", len(body))

		if resp.Header.Get("X-Sidekick-Cache") != "MISS" {
			t.Errorf("Expected MISS on first request for large file, got %s", resp.Header.Get("X-Sidekick-Cache"))
		}

		// Wait for cache writes and metrics to update
		time.Sleep(2 * time.Second)

		metrics := fetchMetrics(t)

		// Check memory cache metrics
		memUsed, memFound := getMetricValue(metrics, "caddy_sidekick_cache_used_bytes", "type", "memory", "server", "default")
		memCount, memCountFound := getMetricValue(metrics, "caddy_sidekick_cache_used_count", "type", "memory", "server", "default")

		if !memFound {
			t.Error("Memory cache used bytes metric not found")
		} else {
			t.Logf("Memory cache used: %.0f bytes", memUsed)
			// Should have the small responses in memory
			if memCount > 0 && memUsed == 0 {
				t.Error("Memory cache count > 0 but bytes is 0")
			}
		}

		if !memCountFound {
			t.Error("Memory cache used count metric not found")
		} else {
			t.Logf("Memory cache items: %.0f", memCount)
			// We made 3 small requests that should be in memory
			if memCount < 3 {
				t.Errorf("Expected at least 3 items in memory cache, got %.0f", memCount)
			}
		}

		// Check memory limit is set correctly (from Caddyfile: 64MB = 64000000 bytes using decimal/SI units)
		if memLimit, found := getMetricValue(metrics, "caddy_sidekick_cache_limit_bytes", "type", "memory", "server", "default"); found {
			expectedMemLimit := float64(64 * 1000 * 1000) // 64MB in bytes (decimal/SI)
			if memLimit != expectedMemLimit {
				t.Errorf("Memory limit bytes unexpected: %f (expected %f)", memLimit, expectedMemLimit)
			}
		} else {
			t.Error("Memory cache limit bytes metric not found")
		}

		// Check disk metrics - should have the large file that exceeds 1MB memory limit
		diskUsed, diskFound := getMetricValue(metrics, "caddy_sidekick_cache_used_bytes", "type", "disk", "server", "default")
		diskCount, diskCountFound := getMetricValue(metrics, "caddy_sidekick_cache_used_count", "type", "disk", "server", "default")

		if !diskFound {
			t.Error("Disk cache used bytes metric not found")
		} else {
			t.Logf("Disk cache used: %.0f bytes", diskUsed)
			// The 1.2MB file should be on disk (compressed)
			if diskUsed == 0 {
				t.Error("Disk cache bytes is 0, but the 1.2MB file should have been stored on disk")
			}
			// Due to compression, disk usage will be less than 1.2MB
			t.Logf("Disk cache is using %.0f bytes (compressed from 1.2MB original)", diskUsed)
		}

		if !diskCountFound {
			t.Error("Disk cache used count metric not found")
		} else {
			t.Logf("Disk cache items: %.0f", diskCount)
			if diskCount < 1 {
				t.Errorf("Expected at least 1 item in disk cache (the large file), got %.0f", diskCount)
			}
		}

		// Check disk limit is set correctly (from Caddyfile: 256MB = 256000000 bytes using decimal/SI units)
		if diskLimit, found := getMetricValue(metrics, "caddy_sidekick_cache_limit_bytes", "type", "disk", "server", "default"); found {
			expectedDiskLimit := float64(256 * 1000 * 1000) // 256MB in bytes (decimal/SI)
			if diskLimit != expectedDiskLimit {
				t.Errorf("Disk limit bytes unexpected: %f (expected %f)", diskLimit, expectedDiskLimit)
			}
		} else {
			t.Error("Disk cache limit bytes metric not found")
		}

		// Check total metrics (should be sum of memory + disk)
		totalUsed, totalFound := getMetricValue(metrics, "caddy_sidekick_cache_used_bytes", "type", "total", "server", "default")
		if !totalFound {
			t.Error("Total cache used bytes metric not found")
		} else {
			expectedTotal := memUsed + diskUsed
			// Allow for small differences due to timing
			if totalUsed < expectedTotal*0.9 || totalUsed > expectedTotal*1.1 {
				t.Errorf("Total cache bytes (%f) doesn't match sum of memory (%f) + disk (%f)", totalUsed, memUsed, diskUsed)
			}
		}
	})

	// Test 5: Verify bypass metrics
	t.Run("BypassMetrics", func(t *testing.T) {
		initialMetrics := fetchMetrics(t)
		initialBypass, _ := getMetricValue(initialMetrics, "caddy_sidekick_cache_requests_total", "result", "bypass", "cache_type", "total")

		// Make a request to a bypass path
		resp, _ := makeRequest(t, "GET", baseURL+"/wp-admin/test", nil, nil)
		defer func() { _ = resp.Body.Close() }()

		if resp.Header.Get("X-Sidekick-Cache") != "BYPASS" {
			t.Errorf("Expected BYPASS for /wp-admin path, got %s", resp.Header.Get("X-Sidekick-Cache"))
		}

		// Check bypass counter increased
		time.Sleep(500 * time.Millisecond)
		updatedMetrics := fetchMetrics(t)
		updatedBypass, _ := getMetricValue(updatedMetrics, "caddy_sidekick_cache_requests_total", "result", "bypass", "cache_type", "total")

		if initialBypass >= updatedBypass {
			t.Errorf("Bypass counter did not increase: initial=%f, updated=%f", initialBypass, updatedBypass)
		}
	})

	// Test 6: Verify purge metrics
	t.Run("PurgeMetrics", func(t *testing.T) {
		// Note: purge operations are no longer tracked in request metrics since they're not requests
		// We'll just verify the purge completes successfully

		// Perform a purge
		purgeCache(t, nil)

		// Check purge counter increased
		time.Sleep(500 * time.Millisecond)
		// Purge operations are not tracked in the simplified metrics
		// Just verify purge completed without error
	})

	// Test 7: Verify response time histogram
	t.Run("ResponseTimeHistogram", func(t *testing.T) {
		// Make several requests
		for i := 0; i < 5; i++ {
			resp, _ := makeRequest(t, "GET", baseURL+"/metrics-timing-test", nil, nil)
			_ = resp.Body.Close()
			time.Sleep(100 * time.Millisecond)
		}

		metrics := fetchMetrics(t)

		// Check histogram exists by looking for sample count
		if count, found := getMetricValue(metrics, "caddy_sidekick_response_time_ms", "cache_status", "miss", "server", "default"); !found || count == 0 {
			t.Error("Response time histogram for cache misses not found or has no samples")
		}
	})

	// Test 9: Verify size distribution histogram
	t.Run("SizeDistributionHistogram", func(t *testing.T) {
		metrics := fetchMetrics(t)

		// Check size distribution histogram exists
		if !strings.Contains(metrics, "caddy_sidekick_cache_size_distribution_bytes_bucket") {
			t.Error("Size distribution histogram not found")
		}

		// Look for different bucket sizes
		buckets := []string{`le="1024"`, `le="4096"`, `le="16384"`, `le="65536"`}
		for _, bucket := range buckets {
			if !strings.Contains(metrics, bucket) {
				t.Errorf("Size distribution bucket %s not found", bucket)
			}
		}
	})

	// Test 11: Verify large files go to disk cache (exceed cache_memory_item_max_size)
	t.Run("LargeFileDiskCache", func(t *testing.T) {
		// Clear cache first to start fresh
		purgeCache(t, nil)
		time.Sleep(1 * time.Second)

		// cache_memory_item_max_size is 1MB in Caddyfile
		// Generate a 1.5MB response that should exceed this limit
		sizeMB := 1.5
		path := fmt.Sprintf("/test-generate-large-file-mb?size=%.1f", sizeMB)

		// First request should be a MISS
		resp1, body1 := makeRequest(t, "GET", baseURL+path, nil, nil)
		defer func() { _ = resp1.Body.Close() }()

		actualSize := len(body1)
		expectedSize := int(sizeMB * 1000 * 1000) // Decimal MB
		t.Logf("Large file response size: %d bytes (expected ~%d bytes)", actualSize, expectedSize)

		// Verify size is approximately what we requested (allow 1% variance for HTML overhead)
		if actualSize < int(float64(expectedSize)*0.99) || actualSize > int(float64(expectedSize)*1.01) {
			t.Errorf("Response size %d is not within 1%% of expected %d", actualSize, expectedSize)
		}

		if resp1.Header.Get("X-Sidekick-Cache") != "MISS" {
			t.Errorf("Expected MISS on first request, got %s", resp1.Header.Get("X-Sidekick-Cache"))
		}

		// Extract the unique ID from the response to verify caching
		uniqueIDPrefix := "Unique ID: "
		uniqueIDIndex := strings.Index(body1, uniqueIDPrefix)
		var uniqueID1 string
		if uniqueIDIndex > 0 {
			endIndex := strings.Index(body1[uniqueIDIndex:], "</p>")
			if endIndex > 0 {
				uniqueID1 = body1[uniqueIDIndex+len(uniqueIDPrefix) : uniqueIDIndex+endIndex]
			}
		}
		t.Logf("First request unique ID: %s", uniqueID1)

		// Wait for cache write to complete
		time.Sleep(2 * time.Second)

		// Second request should be a HIT from disk cache
		resp2, body2 := makeRequest(t, "GET", baseURL+path, nil, nil)
		defer func() { _ = resp2.Body.Close() }()

		if resp2.Header.Get("X-Sidekick-Cache") != "HIT" {
			t.Errorf("Expected HIT on second request, got %s", resp2.Header.Get("X-Sidekick-Cache"))
		}

		// Extract unique ID from second response
		var uniqueID2 string
		uniqueIDIndex2 := strings.Index(body2, uniqueIDPrefix)
		if uniqueIDIndex2 > 0 {
			endIndex := strings.Index(body2[uniqueIDIndex2:], "</p>")
			if endIndex > 0 {
				uniqueID2 = body2[uniqueIDIndex2+len(uniqueIDPrefix) : uniqueIDIndex2+endIndex]
			}
		}
		t.Logf("Second request unique ID: %s", uniqueID2)

		// Verify the cached response has the same unique ID (proving it was cached)
		if uniqueID1 != uniqueID2 {
			t.Errorf("Unique IDs don't match - response was not cached properly (ID1: %s, ID2: %s)", uniqueID1, uniqueID2)
		}

		// Fetch metrics to verify it's in disk cache, not memory cache
		metrics := fetchMetrics(t)

		// Check memory cache - the large file should NOT be there
		memUsed, _ := getMetricValue(metrics, "caddy_sidekick_cache_used_bytes", "type", "memory", "server", "default")
		memCount, _ := getMetricValue(metrics, "caddy_sidekick_cache_used_count", "type", "memory", "server", "default")

		// Memory should not contain the 1.5MB file
		if memUsed > 1000000 { // 1MB threshold
			t.Errorf("Memory cache contains %f bytes - large file should not be in memory (max item size is 1MB)", memUsed)
		}

		// Check disk cache - the large file SHOULD be there
		diskUsed, diskFound := getMetricValue(metrics, "caddy_sidekick_cache_used_bytes", "type", "disk", "server", "default")
		diskCount, diskCountFound := getMetricValue(metrics, "caddy_sidekick_cache_used_count", "type", "disk", "server", "default")

		if !diskFound {
			t.Error("Disk cache used bytes metric not found")
		} else {
			t.Logf("Disk cache used: %.0f bytes", diskUsed)
			// The file is compressed on disk, so we just check it's non-zero
			// The test data appears to be highly compressible (7KB compressed from 1.5MB)
			if diskUsed == 0 {
				t.Error("Disk cache has 0 bytes, but should contain the compressed large file")
			} else {
				compressionRatio := (float64(expectedSize) - diskUsed) / float64(expectedSize) * 100
				t.Logf("File compressed from %d bytes to %.0f bytes (%.1f%% compression)", expectedSize, diskUsed, compressionRatio)
			}
		}

		if !diskCountFound {
			t.Error("Disk cache used count metric not found")
		} else {
			t.Logf("Disk cache items: %.0f", diskCount)
			if diskCount < 1 {
				t.Error("Disk cache should contain at least 1 item (the large file)")
			}
		}

		t.Logf("Memory cache: %.0f bytes, %.0f items", memUsed, memCount)
		t.Logf("Disk cache: %.0f bytes, %.0f items", diskUsed, diskCount)

		// Verify we can still get a cache HIT after some time
		time.Sleep(1 * time.Second)
		resp3, _ := makeRequest(t, "GET", baseURL+path, nil, nil)
		defer func() { _ = resp3.Body.Close() }()

		if resp3.Header.Get("X-Sidekick-Cache") != "HIT" {
			t.Errorf("Expected HIT on third request, got %s", resp3.Header.Get("X-Sidekick-Cache"))
		}
	})

}
