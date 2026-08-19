package integration_test

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
)

// Helper function to make HTTP requests and return response with body as string
func makeRequest(t *testing.T, method, url string, body io.Reader, headers map[string]string) (*http.Response, string) {
	resp := makeRequestRaw(t, method, url, body, headers)
	defer func() { _ = resp.Body.Close() }()

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("Failed to read response body: %v", err)
	}

	return resp, string(bodyBytes)
}

// Helper function to make HTTP requests and return raw response (caller must close body)
func makeRequestRaw(t *testing.T, method, url string, body io.Reader, headers map[string]string) *http.Response {
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		t.Fatalf("Failed to create request: %v", err)
	}

	for key, value := range headers {
		req.Header.Set(key, value)
	}

	// Create a custom transport that disables automatic decompression
	// This allows us to test compression handling explicitly
	transport := &http.Transport{
		DisableCompression: true,
	}

	client := &http.Client{
		Timeout:   10 * time.Second,
		Transport: transport,
	}

	// If no Accept-Encoding header is specified, use identity (no compression)
	if _, ok := headers["Accept-Encoding"]; !ok {
		req.Header.Set("Accept-Encoding", "identity")
	}

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Failed to execute request: %v", err)
	}

	return resp
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

// Helper function to verify multiple cache HIT responses
func verifyCacheHits(t *testing.T, url string, body io.Reader, headers map[string]string, cfg *testConfig) []string {
	bodies := make([]string, cfg.verifyRequestCount)

	for i := 0; i < cfg.verifyRequestCount; i++ {
		resp, body := makeRequest(t, "GET", url, body, headers)
		if resp.StatusCode != 200 {
			t.Fatalf("Request %d: Expected status 200, got %d", i+1, resp.StatusCode)
		}
		checkCacheHeader(t, resp, "HIT")
		bodies[i] = body

		if i < cfg.verifyRequestCount-1 {
			time.Sleep(cfg.verifyRequestDelay)
		}
	}

	return bodies
}

// checkCacheControlHIT validates Cache-Control and Age headers for HIT responses.
// maxTTL is the configured CacheTTL in seconds.
func checkCacheControlHIT(t *testing.T, resp *http.Response, maxTTL int) {
	t.Helper()
	cc := resp.Header.Get("Cache-Control")
	if cc == "" {
		t.Error("Expected Cache-Control header on HIT, got empty")
		return
	}
	if !strings.HasPrefix(cc, "public, max-age=") {
		t.Errorf("Expected Cache-Control to start with 'public, max-age=', got: %s", cc)
		return
	}
	maxAgeStr := strings.TrimPrefix(cc, "public, max-age=")
	maxAge, err := strconv.Atoi(maxAgeStr)
	if err != nil {
		t.Errorf("Failed to parse max-age value %q: %v", maxAgeStr, err)
		return
	}
	if maxAge < 0 || maxAge > maxTTL {
		t.Errorf("max-age=%d is outside valid range [0, %d]", maxAge, maxTTL)
	}

	ageStr := resp.Header.Get("Age")
	if ageStr == "" {
		t.Error("Expected Age header on HIT, got empty")
		return
	}
	age, err := strconv.Atoi(ageStr)
	if err != nil {
		t.Errorf("Failed to parse Age value %q: %v", ageStr, err)
		return
	}
	if age < 0 {
		t.Errorf("Age=%d should be non-negative", age)
	}
}

// checkCacheControlMISS validates that a MISS is advertised as cacheable downstream.
//
// A MISS used to be stamped "no-cache, no-store, must-revalidate", which told the
// browser and CloudFront to discard a response they were entitled to keep — so the
// first viewer's copy was thrown away everywhere and the object was re-fetched from
// origin forever. A MISS is exactly as cacheable as a HIT; it simply was not in
// Sidekick's cache yet.
func checkCacheControlMISS(t *testing.T, resp *http.Response, ttl int) {
	t.Helper()

	cc := resp.Header.Get("Cache-Control")
	expected := fmt.Sprintf("public, max-age=%d", ttl)
	if cc != expected {
		t.Errorf("Expected Cache-Control: %s on MISS, got: %s", expected, cc)
	}

	if age := resp.Header.Get("Age"); age != "0" {
		t.Errorf("Expected Age: 0 on MISS, got: %q", age)
	}

	if pragma := resp.Header.Get("Pragma"); pragma != "" {
		t.Errorf("Pragma has no defined meaning in a response and should not be set on a MISS, got: %q", pragma)
	}
}

// checkPrivateHeaders validates that a response is locked out of every cache. Used
// for private bypasses: the WordPress login cookie, the nocache path prefixes
// (/wp-admin, /wp-json, ...), and anything the origin itself marked no-store.
func checkPrivateHeaders(t *testing.T, resp *http.Response) {
	t.Helper()

	cc := resp.Header.Get("Cache-Control")
	if cc != "private, no-store" {
		t.Errorf("Expected Cache-Control: private, no-store, got: %s", cc)
	}
	if strings.Contains(cc, "public") {
		t.Errorf("A private response must never be advertised as shareable, got: %s", cc)
	}
	if pragma := resp.Header.Get("Pragma"); pragma != "no-cache" {
		t.Errorf("Expected Pragma: no-cache, got: %s", pragma)
	}
}

// checkPolicyBypassHeaders validates that a policy bypass leaves the origin's own
// cacheability directives alone. Sidekick declining to store a response says nothing
// about whether the browser or a CDN may.
func checkPolicyBypassHeaders(t *testing.T, resp *http.Response) {
	t.Helper()

	cc := resp.Header.Get("Cache-Control")
	if cc == "private, no-store" || cc == "no-cache, no-store, must-revalidate" {
		t.Errorf("A policy bypass must not stamp its own no-store directives, got: %s", cc)
	}
	if resp.Header.Get("Vary") == "" {
		t.Error("Vary must be set on every path, including policy bypasses")
	}
}

// Compression decoders

// decodeIdentity returns the content as-is (no compression)
func decodeIdentity(r io.Reader) (string, error) {
	bodyBytes, err := io.ReadAll(r)
	if err != nil {
		return "", fmt.Errorf("failed to read body: %w", err)
	}
	return string(bodyBytes), nil
}

// decodeGzip decompresses gzip-encoded content
func decodeGzip(r io.Reader) (string, error) {
	// Read all data first to inspect it
	allData, err := io.ReadAll(r)
	if err != nil {
		return "", fmt.Errorf("failed to read response body: %w", err)
	}

	// Check if this is actually gzip data
	if len(allData) < 2 {
		// Too small to be gzip, return as-is
		return string(allData), nil
	}

	// Check for gzip magic bytes
	if allData[0] != 0x1f || allData[1] != 0x8b {
		// Not gzip compressed, return as-is
		// This might happen if encode module didn't compress on cache MISS
		return string(allData), nil
	}

	// It's gzip, decompress it
	gr, err := gzip.NewReader(bytes.NewReader(allData))
	if err != nil {
		return "", fmt.Errorf("failed to create gzip reader: %w", err)
	}
	defer func() { _ = gr.Close() }()

	bodyBytes, err := io.ReadAll(gr)
	if err != nil {
		return "", fmt.Errorf("failed to read gzip body: %w", err)
	}
	return string(bodyBytes), nil
}

// decodeBrotli decompresses brotli-encoded content
func decodeBrotli(r io.Reader) (string, error) {
	br := brotli.NewReader(r)

	bodyBytes, err := io.ReadAll(br)
	if err != nil {
		return "", fmt.Errorf("failed to read brotli body: %w", err)
	}
	return string(bodyBytes), nil
}

// decodeZstd decompresses zstd-encoded content
func decodeZstd(r io.Reader) (string, error) {
	zr, err := zstd.NewReader(r)
	if err != nil {
		return "", fmt.Errorf("failed to create zstd reader: %w", err)
	}
	defer zr.Close()

	bodyBytes, err := io.ReadAll(zr)
	if err != nil {
		return "", fmt.Errorf("failed to read zstd body: %w", err)
	}
	return string(bodyBytes), nil
}
