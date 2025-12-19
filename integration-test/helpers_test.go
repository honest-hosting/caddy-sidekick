package integration_test

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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
