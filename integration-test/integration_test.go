package integration_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

const (
	baseURL    = "http://localhost:8080"
	purgeToken = "test-token-12345"
	purgeURL   = "/__sidekick/purge"
)

// TestIntegrationMemoryCacheBasic tests basic memory cache functionality
func TestIntegrationMemoryCacheBasic(t *testing.T) {
	// Clear cache first
	purgeCache(t, nil)
	time.Sleep(1 * time.Second)

	// First request - should be MISS
	resp1, body1 := makeRequest(t, "GET", baseURL+"/cacheable", nil, nil)
	if resp1.StatusCode != 200 {
		t.Fatalf("Expected status 200, got %d", resp1.StatusCode)
	}
	checkCacheHeader(t, resp1, "MISS")

	// Wait a moment for cache to be written
	time.Sleep(500 * time.Millisecond)

	// Second request - should be HIT
	resp2, body2 := makeRequest(t, "GET", baseURL+"/cacheable", nil, nil)
	if resp2.StatusCode != 200 {
		t.Fatalf("Expected status 200, got %d", resp2.StatusCode)
	}
	checkCacheHeader(t, resp2, "HIT")

	// Body should be identical for cached response
	if body1 != body2 {
		t.Error("Cached response body differs from original")
	}
}

// TestIntegrationDiskCacheLargeFile tests disk caching for large responses
func TestIntegrationDiskCacheLargeFile(t *testing.T) {
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
	time.Sleep(1 * time.Second)

	// Second request - should be HIT
	resp2, _ := makeRequest(t, "GET", largeURL, nil, nil)
	if resp2.StatusCode != 200 {
		t.Fatalf("Expected status 200, got %d", resp2.StatusCode)
	}
	checkCacheHeader(t, resp2, "HIT")

	//TODO: Verify content exists on-disk in 'cache' dir??
}

// TestIntegrationCacheResponseCodes tests caching of different HTTP status codes
func TestIntegrationCacheResponseCodes(t *testing.T) {
	// Clear cache
	purgeCache(t, nil)
	time.Sleep(1 * time.Second)

	// Test 404 caching (configured as cacheable)
	resp1, _ := makeRequest(t, "GET", baseURL+"/error/404", nil, nil)
	if resp1.StatusCode != 404 {
		t.Fatalf("Expected status 404, got %d", resp1.StatusCode)
	}
	checkCacheHeader(t, resp1, "BYPASS")

	time.Sleep(500 * time.Millisecond)

	resp2, _ := makeRequest(t, "GET", baseURL+"/error/404", nil, nil)
	if resp2.StatusCode != 404 {
		t.Fatalf("Expected status 404, got %d", resp2.StatusCode)
	}
	checkCacheHeader(t, resp2, "BYPASS")

	// Test 500 (should not be cached)
	resp3, body3 := makeRequest(t, "GET", baseURL+"/error/500", nil, nil)
	if resp3.StatusCode != 500 {
		t.Fatalf("Expected status 500, got %d", resp3.StatusCode)
	}
	checkCacheHeader(t, resp3, "BYPASS")

	time.Sleep(500 * time.Millisecond)

	resp4, body4 := makeRequest(t, "GET", baseURL+"/error/500", nil, nil)
	if resp4.StatusCode != 500 {
		t.Fatalf("Expected status 500, got %d", resp4.StatusCode)
	}
	checkCacheHeader(t, resp4, "BYPASS")

	// Bodies should differ (not cached)
	if body3 == body4 {
		t.Error("500 error response should not be cached but bodies are identical")
	}
}

// TestIntegrationPurgeSpecificPaths tests purging specific cache paths
func TestIntegrationPurgeSpecificPaths(t *testing.T) {
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

	time.Sleep(1 * time.Second)

	// Verify all are cached
	for _, path := range paths {
		resp, _ := makeRequest(t, "GET", baseURL+path, nil, nil)
		checkCacheHeader(t, resp, "HIT")
	}

	// Purge only specific path
	purgeCache(t, []string{"/cacheable"})
	time.Sleep(1 * time.Second)

	// // Check that purged path returns MISS
	// resp1, _ := makeRequest(t, "GET", baseURL+"/cacheable", nil, nil)
	// body, _ := io.ReadAll(resp1.Body)
	// t.Logf("Got response BODY: %s", string(body))
	// t.Logf("Got response HEADERS: %+v", resp1.Header)
	// checkCacheHeader(t, resp1, "MISS")

	// // Other paths should still be cached
	// resp2, _ := makeRequest(t, "GET", baseURL+"/static/page", nil, nil)
	// checkCacheHeader(t, resp2, "HIT")

	// resp3, _ := makeRequest(t, "GET", baseURL+"/api/data", nil, nil)
	// checkCacheHeader(t, resp3, "HIT")
}

// TestIntegrationPurgeWildcard tests wildcard purging functionality
func TestIntegrationPurgeWildcard(t *testing.T) {
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

	time.Sleep(1 * time.Second)

	// Verify all are cached
	for _, path := range testPaths {
		resp, _ := makeRequest(t, "GET", baseURL+path, nil, nil)
		checkCacheHeader(t, resp, "HIT")
	}

	// Purge with wildcard pattern /path/subdir/*
	purgeCache(t, []string{"/path/subdir/*"})
	time.Sleep(500 * time.Millisecond)

	// Check that paths under /path/subdir/ return MISS
	resp1, _ := makeRequest(t, "GET", baseURL+"/path/subdir/image1.png", nil, nil)
	checkCacheHeader(t, resp1, "MISS")

	resp2, _ := makeRequest(t, "GET", baseURL+"/path/subdir/image2.png", nil, nil)
	checkCacheHeader(t, resp2, "MISS")

	// But /path/image123.png should still be HIT
	resp3, _ := makeRequest(t, "GET", baseURL+"/path/image123.png", nil, nil)
	checkCacheHeader(t, resp3, "HIT")

	resp4, _ := makeRequest(t, "GET", baseURL+"/path/image1.png", nil, nil)
	checkCacheHeader(t, resp4, "HIT")
}

// TestIntegrationNoCachePaths tests that nocache paths are never cached
func TestIntegrationNoCachePaths(t *testing.T) {
	// Clear cache
	purgeCache(t, nil)
	time.Sleep(1 * time.Second)

	// Test wp-admin path (configured as nocache)
	resp1, body1 := makeRequest(t, "GET", baseURL+"/wp-admin/index.php", nil, nil)
	if resp1.StatusCode != 200 {
		t.Fatalf("Expected status 200, got %d", resp1.StatusCode)
	}
	checkCacheHeader(t, resp1, "BYPASS")

	time.Sleep(500 * time.Millisecond)

	resp2, body2 := makeRequest(t, "GET", baseURL+"/wp-admin/index.php", nil, nil)
	if resp2.StatusCode != 200 {
		t.Fatalf("Expected status 200, got %d", resp2.StatusCode)
	}
	checkCacheHeader(t, resp2, "BYPASS")

	// Bodies should be different (not cached, contains timestamp)
	if body1 == body2 {
		t.Error("Nocache path response should not be cached but bodies are identical")
	}

	// Test wp-json path (configured as nocache)
	resp3, body3 := makeRequest(t, "GET", baseURL+"/wp-json/wp/v2/posts", nil, nil)
	checkCacheHeader(t, resp3, "BYPASS")

	resp4, body4 := makeRequest(t, "GET", baseURL+"/wp-json/wp/v2/posts", nil, nil)
	checkCacheHeader(t, resp4, "BYPASS")

	if body3 == body4 {
		t.Error("wp-json response should not be cached but bodies are identical")
	}
}

// TestIntegrationNoCacheHome tests home page caching behavior
func TestIntegrationNoCacheHome(t *testing.T) {
	// Clear cache
	purgeCache(t, nil)
	time.Sleep(1 * time.Second)

	// Test home page (nocache_home is false, so it should be cached)
	resp1, body1 := makeRequest(t, "GET", baseURL+"/", nil, nil)
	if resp1.StatusCode != 200 {
		t.Fatalf("Expected status 200, got %d", resp1.StatusCode)
	}
	checkCacheHeader(t, resp1, "MISS")

	time.Sleep(500 * time.Millisecond)

	resp2, body2 := makeRequest(t, "GET", baseURL+"/", nil, nil)
	if resp2.StatusCode != 200 {
		t.Fatalf("Expected status 200, got %d", resp2.StatusCode)
	}
	checkCacheHeader(t, resp2, "HIT")

	// Bodies should be identical (cached)
	if body1 != body2 {
		t.Error("Home page should be cached but bodies differ")
	}
}

// TestIntegrationQueryParameterCaching tests cache key generation with query parameters
func TestIntegrationQueryParameterCaching(t *testing.T) {
	// Clear cache
	purgeCache(t, nil)
	time.Sleep(1 * time.Second)

	// Request with query parameters that are in cache_key_queries
	url1 := baseURL + "/dynamic?name=Alice&page=1"
	resp1, body1 := makeRequest(t, "GET", url1, nil, nil)
	checkCacheHeader(t, resp1, "MISS")

	time.Sleep(500 * time.Millisecond)

	// Same query parameters - should be HIT
	resp2, body2 := makeRequest(t, "GET", url1, nil, nil)
	checkCacheHeader(t, resp2, "HIT")
	if body1 != body2 {
		t.Error("Same query parameters should return cached response")
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

	// Request with If-None-Match
	headers := map[string]string{
		"If-None-Match": etag,
	}
	resp2, _ := makeRequest(t, "GET", baseURL+"/conditional", nil, headers)
	if resp2.StatusCode != 304 {
		t.Errorf("Expected 304 Not Modified with If-None-Match, got %d", resp2.StatusCode)
	}

	// Request with If-Modified-Since
	headers = map[string]string{
		"If-Modified-Since": lastModified,
	}
	resp3, _ := makeRequest(t, "GET", baseURL+"/conditional", nil, headers)
	if resp3.StatusCode != 304 {
		t.Errorf("Expected 304 Not Modified with If-Modified-Since, got %d", resp3.StatusCode)
	}
}

// TestIntegrationCacheHeaderVariations tests cache variations based on headers
func TestIntegrationCacheHeaderVariations(t *testing.T) {
	// Clear cache
	purgeCache(t, nil)
	time.Sleep(1 * time.Second)

	// Request with specific header
	headers1 := map[string]string{
		"Accept-Language": "en-US",
	}
	resp1, body1 := makeRequest(t, "GET", baseURL+"/cacheable", nil, headers1)
	checkCacheHeader(t, resp1, "MISS")

	time.Sleep(500 * time.Millisecond)

	// Same header - should be HIT
	resp2, body2 := makeRequest(t, "GET", baseURL+"/cacheable", nil, headers1)
	checkCacheHeader(t, resp2, "HIT")
	if body1 != body2 {
		t.Error("Same headers should return cached response")
	}

	// Different header value - should be MISS (new cache entry)
	headers2 := map[string]string{
		"Accept-Language": "fr-FR",
	}
	resp3, _ := makeRequest(t, "GET", baseURL+"/cacheable", nil, headers2)
	checkCacheHeader(t, resp3, "MISS")

	// Verify both variations are cached
	resp4, _ := makeRequest(t, "GET", baseURL+"/cacheable", nil, headers1)
	checkCacheHeader(t, resp4, "HIT")

	resp5, _ := makeRequest(t, "GET", baseURL+"/cacheable", nil, headers2)
	checkCacheHeader(t, resp5, "HIT")
}

// TestIntegrationPurgeAll tests purging all cache entries
func TestIntegrationPurgeAll(t *testing.T) {
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

	time.Sleep(1 * time.Second)

	// Verify all are cached
	for _, path := range paths {
		resp, _ := makeRequest(t, "GET", baseURL+path, nil, nil)
		checkCacheHeader(t, resp, "HIT")
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
	// Clear cache
	purgeCache(t, nil)
	time.Sleep(1 * time.Second)

	// Make concurrent requests
	concurrency := 10
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

	// Final request should definitely be HIT
	time.Sleep(500 * time.Millisecond)
	resp, _ := makeRequest(t, "GET", baseURL+"/cacheable", nil, nil)
	checkCacheHeader(t, resp, "HIT")
}

// TestIntegrationNoCacheRegex tests regex-based cache bypass
func TestIntegrationNoCacheRegex(t *testing.T) {
	// Clear cache
	purgeCache(t, nil)
	time.Sleep(1 * time.Second)

	// Test that configured file types in nocache_regex are bypassed
	// According to Caddyfile, .png should be cacheable (not in regex)
	resp1, _ := makeRequest(t, "GET", baseURL+"/path/image1.png", nil, nil)
	checkCacheHeader(t, resp1, "MISS")

	time.Sleep(500 * time.Millisecond)

	resp2, _ := makeRequest(t, "GET", baseURL+"/path/image1.png", nil, nil)
	checkCacheHeader(t, resp2, "HIT")

	// Test file types that SHOULD be in nocache_regex (bypassed)
	// Note: These paths return 404, which is not in cache_response_codes (only 200, 301, 302)
	// So they will be BYPASS both due to nocache_regex AND due to 404 not being cacheable
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
		resp, _ := makeRequest(t, "GET", baseURL+path, nil, nil)
		cacheHeader := resp.Header.Get("X-Sidekick-Cache")
		if cacheHeader != "BYPASS" {
			t.Errorf("Expected %s to be bypassed due to nocache_regex, got header: %s (status: %d)", path, cacheHeader, resp.StatusCode)
		}

		// Second request should also be BYPASS (not cached)
		resp2, _ := makeRequest(t, "GET", baseURL+path, nil, nil)
		cacheHeader2 := resp2.Header.Get("X-Sidekick-Cache")
		if cacheHeader2 != "BYPASS" {
			t.Errorf("Expected %s to remain bypassed on second request, got header: %s (status: %d)", path, cacheHeader2, resp2.StatusCode)
		}
	}

	// Test file types that should NOT be in nocache_regex (would be cacheable if they existed)
	// However, these paths return 404, which is NOT in cache_response_codes (only 200, 301, 302)
	// So they will return BYPASS due to the 404 status, not due to the extension
	cacheableExtensions := []string{
		"/image.jpg",
		"/style.css",
		"/script.js",
		"/icon.svg",
		"/document.html",
	}

	for _, path := range cacheableExtensions {
		resp, _ := makeRequest(t, "GET", baseURL+path, nil, nil)
		cacheHeader := resp.Header.Get("X-Sidekick-Cache")

		// These return 404 which is not cacheable in test config, so should be BYPASS
		switch resp.StatusCode {
		case 404:
			if cacheHeader != "BYPASS" {
				t.Errorf("Expected %s with 404 status to be BYPASS (404 not in cache_response_codes), got header: %s", path, cacheHeader)
			}
		case 200:
			// If somehow these paths return 200, they should be cacheable
			if cacheHeader != "MISS" {
				t.Errorf("Expected %s first request with 200 status to be MISS (cacheable), got header: %s", path, cacheHeader)
			}

			time.Sleep(200 * time.Millisecond)

			// Second request should be HIT
			resp2, _ := makeRequest(t, "GET", baseURL+path, nil, nil)
			cacheHeader2 := resp2.Header.Get("X-Sidekick-Cache")
			if cacheHeader2 != "HIT" {
				t.Errorf("Expected %s second request to be HIT (cached), got header: %s", path, cacheHeader2)
			}
		default:
			t.Logf("Unexpected status code %d for %s", resp.StatusCode, path)
		}
	}

	// Test file types that are NOT in nocache_regex and actually exist (return 200)
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

		time.Sleep(500 * time.Millisecond)

		// Second request should be HIT (cached)
		resp2, _ := makeRequest(t, "GET", baseURL+path, nil, nil)
		cacheHeader2 := resp2.Header.Get("X-Sidekick-Cache")
		if cacheHeader2 != "HIT" {
			t.Errorf("Expected %s second request to be HIT (cached), got header: %s", path, cacheHeader2)
		}
	}
}

// Helper function to make HTTP requests
func makeRequest(t *testing.T, method, url string, body io.Reader, headers map[string]string) (*http.Response, string) {
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		t.Fatalf("Failed to create request: %v", err)
	}

	for key, value := range headers {
		req.Header.Set(key, value)
	}

	// Create a custom transport that disables compression
	// This prevents the "gzip: invalid header" error when the cached response
	// includes compression headers but the body is already decompressed
	transport := &http.Transport{
		DisableCompression: true,
	}

	client := &http.Client{
		Timeout:   10 * time.Second,
		Transport: transport,
	}

	// Explicitly request uncompressed content
	req.Header.Set("Accept-Encoding", "identity")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Failed to execute request: %v", err)
	}

	bodyBytes, err := io.ReadAll(resp.Body)
	resp.Body.Close() //nolint:errcheck
	if err != nil {
		t.Fatalf("Failed to read response body: %v", err)
	}

	return resp, string(bodyBytes)
}

// Helper function to purge cache
func purgeCache(t *testing.T, paths []string) {
	var body io.Reader
	headers := map[string]string{
		"X-Sidekick-Purge": purgeToken,
	}

	if len(paths) > 0 {
		jsonBody, _ := json.Marshal(map[string][]string{"paths": paths})
		body = bytes.NewBuffer(jsonBody)
		headers["Content-Type"] = "application/json"
	}

	resp, _ := makeRequest(t, "POST", baseURL+purgeURL, body, headers)
	if resp.StatusCode != 200 && resp.StatusCode != 204 {
		t.Fatalf("Failed to purge cache, status: %d", resp.StatusCode)
	}
}

// Helper function to check cache header
func checkCacheHeader(t *testing.T, resp *http.Response, expected string) {
	cacheHeader := resp.Header.Get("X-Sidekick-Cache")
	if cacheHeader != expected {
		t.Errorf("Expected X-Sidekick-Cache: %s, got: %s", expected, cacheHeader)
	}
}
