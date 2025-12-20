package sidekick

import (
	"bytes"
	"crypto/md5"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2"
	"go.uber.org/zap"
)

// MockHandler implements caddyhttp.Handler for testing
type MockHandler struct {
	ServeHTTPFunc func(http.ResponseWriter, *http.Request) error
}

func (m *MockHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) error {
	if m.ServeHTTPFunc != nil {
		return m.ServeHTTPFunc(w, r)
	}
	w.WriteHeader(http.StatusOK)
	return nil
}

func TestPurgeConfiguration(t *testing.T) {
	tests := []struct {
		name        string
		envVars     map[string]string
		expectError bool
		expected    struct {
			purgeURI    string
			purgeHeader string
			purgeToken  string
		}
	}{
		{
			name: "default values",
			envVars: map[string]string{
				"SIDEKICK_CACHE_DIR":             t.TempDir(),
				"SIDEKICK_CACHE_MEMORY_MAX_SIZE": "1MB",
			},
			expectError: false,
			expected: struct {
				purgeURI    string
				purgeHeader string
				purgeToken  string
			}{
				purgeURI:    "/__sidekick/purge",
				purgeHeader: "X-Sidekick-Purge",
				purgeToken:  "dead-beef",
			},
		},
		{
			name: "custom values from env",
			envVars: map[string]string{
				"SIDEKICK_CACHE_DIR":             t.TempDir(),
				"SIDEKICK_PURGE_URI":             "/custom/purge",
				"SIDEKICK_PURGE_HEADER":          "X-Custom-Header",
				"SIDEKICK_PURGE_TOKEN":           "custom-token-123",
				"SIDEKICK_CACHE_MEMORY_MAX_SIZE": "1MB",
			},
			expectError: false,
			expected: struct {
				purgeURI    string
				purgeHeader string
				purgeToken  string
			}{
				purgeURI:    "/custom/purge",
				purgeHeader: "X-Custom-Header",
				purgeToken:  "custom-token-123",
			},
		},
		{
			name: "invalid purge URI - no leading slash",
			envVars: map[string]string{
				"SIDEKICK_CACHE_DIR":             t.TempDir(),
				"SIDEKICK_PURGE_URI":             "custom/purge",
				"SIDEKICK_CACHE_MEMORY_MAX_SIZE": "1MB",
			},
			expectError: true,
		},
		{
			name: "invalid purge URI - uppercase letters",
			envVars: map[string]string{
				"SIDEKICK_CACHE_DIR":             t.TempDir(),
				"SIDEKICK_PURGE_URI":             "/Custom/Purge",
				"SIDEKICK_CACHE_MEMORY_MAX_SIZE": "1MB",
			},
			expectError: true,
		},
		{
			name: "invalid purge URI - special characters",
			envVars: map[string]string{
				"SIDEKICK_CACHE_DIR":             t.TempDir(),
				"SIDEKICK_PURGE_URI":             "/custom_purge!",
				"SIDEKICK_CACHE_MEMORY_MAX_SIZE": "1MB",
			},
			expectError: true,
		},
		{
			name: "empty purge token uses default",
			envVars: map[string]string{
				"SIDEKICK_CACHE_DIR":             t.TempDir(),
				"SIDEKICK_CACHE_MEMORY_MAX_SIZE": "1MB",
				"SIDEKICK_PURGE_TOKEN":           "", // Empty token should use default
			},
			expectError: false,
			expected: struct {
				purgeURI    string
				purgeHeader string
				purgeToken  string
			}{
				purgeURI:    "/__sidekick/purge",
				purgeHeader: "X-Sidekick-Purge",
				purgeToken:  "dead-beef", // Should get default value
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Clear environment
			os.Clearenv()

			// Set test environment variables
			for k, v := range tt.envVars {
				_ = os.Setenv(k, v)
			}

			// Create Sidekick instance
			s := &Sidekick{}
			ctx := caddy.Context{}

			// Provision the module
			err := s.Provision(ctx)

			if tt.expectError {
				if err == nil {
					t.Errorf("Expected error but got none")
				}
			} else {
				if err != nil {
					t.Errorf("Unexpected error: %v", err)
				}

				// Verify configuration
				if s.PurgeURI != tt.expected.purgeURI {
					t.Errorf("PurgeURI: expected %s, got %s", tt.expected.purgeURI, s.PurgeURI)
				}
				if s.PurgeHeader != tt.expected.purgeHeader {
					t.Errorf("PurgeHeader: expected %s, got %s", tt.expected.purgeHeader, s.PurgeHeader)
				}
				if s.PurgeToken != tt.expected.purgeToken {
					t.Errorf("PurgeToken: expected %s, got %s", tt.expected.purgeToken, s.PurgeToken)
				}
			}
		})
	}
}

func TestPurgeHandler(t *testing.T) {
	// Setup test environment
	tmpDir := t.TempDir()

	s := &Sidekick{
		CacheDir:               tmpDir,
		CacheTTL:               60,
		PurgeURI:               "/__sidekick/purge",
		PurgeHeader:            "X-Sidekick-Purge",
		PurgeToken:             "test-token",
		CacheMemoryItemMaxSize: 1024 * 1024,      // 1MB
		CacheMemoryMaxSize:     10 * 1024 * 1024, // 10MB
		CacheMemoryMaxCount:    100,
		CacheResponseCodes:     []string{"200"},
	}

	// Initialize logger
	s.logger = zap.NewNop()

	// Initialize sync handler
	s.syncHandler = &SyncHandler{
		inFlight: make(map[string]*sync.Once),
	}

	// Initialize storage
	s.Storage = NewStorage(s.CacheDir, s.CacheTTL, 1*1024*1024, 10*1024*1024, 100, 0, 0, 0, s.logger)

	// Helper function to generate cache key like the real implementation
	generateCacheKey := func(path string) string {
		h := md5.New()
		h.Write([]byte(path))
		return fmt.Sprintf("%x", h.Sum(nil))
	}

	// Helper function to store data with proper metadata including path
	storeTestData := func(path string) {
		metadata := &Metadata{
			StateCode: 200,
			Header: [][]string{
				{"Content-Type", "text/plain"},
			},
			Timestamp: time.Now().Unix(),
			Path:      path, // Include the path in metadata for purging
		}
		testData := []byte("test content for " + path)
		key := generateCacheKey(path)
		_ = s.Storage.SetWithKey(key, metadata, testData)
	}

	tests := []struct {
		name           string
		method         string
		path           string
		headers        map[string]string
		body           interface{}
		expectedStatus int
		checkCache     func(t *testing.T, storage *Storage)
	}{
		{
			name:           "GET request not allowed",
			method:         "GET",
			path:           "/__sidekick/purge",
			headers:        map[string]string{"X-Sidekick-Purge": "test-token"},
			expectedStatus: http.StatusMethodNotAllowed,
		},
		{
			name:           "PUT request not allowed",
			method:         "PUT",
			path:           "/__sidekick/purge",
			headers:        map[string]string{"X-Sidekick-Purge": "test-token"},
			expectedStatus: http.StatusMethodNotAllowed,
		},
		{
			name:           "POST without token unauthorized",
			method:         "POST",
			path:           "/__sidekick/purge",
			headers:        map[string]string{},
			expectedStatus: http.StatusUnauthorized,
		},
		{
			name:           "POST with wrong token unauthorized",
			method:         "POST",
			path:           "/__sidekick/purge",
			headers:        map[string]string{"X-Sidekick-Purge": "wrong-token"},
			expectedStatus: http.StatusUnauthorized,
		},
		{
			name:           "POST with empty body purges all",
			method:         "POST",
			path:           "/__sidekick/purge",
			headers:        map[string]string{"X-Sidekick-Purge": "test-token"},
			body:           nil,
			expectedStatus: http.StatusOK,
			checkCache: func(t *testing.T, storage *Storage) {
				list := storage.List()
				memCount := len(list["mem"])
				diskCount := len(list["disk"])
				totalCount := memCount + diskCount
				if totalCount != 0 {
					t.Errorf("Expected cache to be empty after flush, got %d items (mem: %d, disk: %d)", totalCount, memCount, diskCount)
				}
			},
		},
		{
			name:           "POST with empty JSON purges all",
			method:         "POST",
			path:           "/__sidekick/purge",
			headers:        map[string]string{"X-Sidekick-Purge": "test-token"},
			body:           map[string]interface{}{},
			expectedStatus: http.StatusOK,
			checkCache: func(t *testing.T, storage *Storage) {
				list := storage.List()
				memCount := len(list["mem"])
				diskCount := len(list["disk"])
				totalCount := memCount + diskCount
				if totalCount != 0 {
					t.Errorf("Expected cache to be empty after flush, got %d items (mem: %d, disk: %d)", totalCount, memCount, diskCount)
				}
			},
		},
		{
			name:   "POST with specific paths",
			method: "POST",
			path:   "/__sidekick/purge",
			headers: map[string]string{
				"X-Sidekick-Purge": "test-token",
				"Content-Type":     "application/json",
			},
			body: map[string]interface{}{
				"paths": []string{"/test-path-1", "/blog/post-1"},
			},
			expectedStatus: http.StatusOK,
			checkCache: func(t *testing.T, storage *Storage) {
				// Check using the actual MD5 cache keys
				key1 := generateCacheKey("/test-path-1")
				key2 := generateCacheKey("/test-path-2")
				key3 := generateCacheKey("/blog/post-1")
				key4 := generateCacheKey("/blog/post-2")

				// These should be purged (matched by path in metadata)
				if _, _, err := storage.Get(key1); err == nil {
					t.Error("Expected /test-path-1 to be purged")
				}
				if _, _, err := storage.Get(key3); err == nil {
					t.Error("Expected /blog/post-1 to be purged")
				}
				// These should remain
				if _, _, err := storage.Get(key2); err != nil {
					t.Error("Expected /test-path-2 to remain in cache")
				}
				if _, _, err := storage.Get(key4); err != nil {
					t.Error("Expected /blog/post-2 to remain in cache")
				}
			},
		},
		{
			name:   "POST with wildcard paths",
			method: "POST",
			path:   "/__sidekick/purge",
			headers: map[string]string{
				"X-Sidekick-Purge": "test-token",
				"Content-Type":     "application/json",
			},
			body: map[string]interface{}{
				"paths": []string{"/blog/*"},
			},
			expectedStatus: http.StatusOK,
			checkCache: func(t *testing.T, storage *Storage) {
				key1 := generateCacheKey("/test-path-1")
				key2 := generateCacheKey("/test-path-2")
				key3 := generateCacheKey("/blog/post-1")
				key4 := generateCacheKey("/blog/post-2")

				// Blog posts should be purged (matched by wildcard pattern in metadata)
				if _, _, err := storage.Get(key3); err == nil {
					t.Error("Expected /blog/post-1 to be purged")
				}
				if _, _, err := storage.Get(key4); err == nil {
					t.Error("Expected /blog/post-2 to be purged")
				}
				// Test paths should remain
				if _, _, err := storage.Get(key1); err != nil {
					t.Error("Expected /test-path-1 to remain in cache")
				}
				if _, _, err := storage.Get(key2); err != nil {
					t.Error("Expected /test-path-2 to remain in cache")
				}
			},
		},
		{
			name:   "POST with invalid JSON treated as empty",
			method: "POST",
			path:   "/__sidekick/purge",
			headers: map[string]string{
				"X-Sidekick-Purge": "test-token",
				"Content-Type":     "application/json",
			},
			body:           "not valid json",
			expectedStatus: http.StatusOK,
			checkCache: func(t *testing.T, storage *Storage) {
				list := storage.List()
				memCount := len(list["mem"])
				diskCount := len(list["disk"])
				totalCount := memCount + diskCount
				if totalCount != 0 {
					t.Errorf("Expected cache to be empty after flush, got %d items (mem: %d, disk: %d)", totalCount, memCount, diskCount)
				}
			},
		},
		{
			name:   "POST with paths not matching metadata",
			method: "POST",
			path:   "/__sidekick/purge",
			headers: map[string]string{
				"X-Sidekick-Purge": "test-token",
				"Content-Type":     "application/json",
			},
			body: map[string]interface{}{
				"paths": []string{"/non-existent-path"},
			},
			expectedStatus: http.StatusOK,
			checkCache: func(t *testing.T, storage *Storage) {
				// All items should remain since no paths match
				key1 := generateCacheKey("/test-path-1")
				key2 := generateCacheKey("/test-path-2")
				key3 := generateCacheKey("/blog/post-1")
				key4 := generateCacheKey("/blog/post-2")

				if _, _, err := storage.Get(key1); err != nil {
					t.Error("Expected /test-path-1 to remain in cache")
				}
				if _, _, err := storage.Get(key2); err != nil {
					t.Error("Expected /test-path-2 to remain in cache")
				}
				if _, _, err := storage.Get(key3); err != nil {
					t.Error("Expected /blog/post-1 to remain in cache")
				}
				if _, _, err := storage.Get(key4); err != nil {
					t.Error("Expected /blog/post-2 to remain in cache")
				}
			},
		},
		{
			name:   "POST with multiple wildcard patterns",
			method: "POST",
			path:   "/__sidekick/purge",
			headers: map[string]string{
				"X-Sidekick-Purge": "test-token",
				"Content-Type":     "application/json",
			},
			body: map[string]interface{}{
				"paths": []string{"/test-path-*", "/blog/post-1"},
			},
			expectedStatus: http.StatusOK,
			checkCache: func(t *testing.T, storage *Storage) {
				key1 := generateCacheKey("/test-path-1")
				key2 := generateCacheKey("/test-path-2")
				key3 := generateCacheKey("/blog/post-1")
				key4 := generateCacheKey("/blog/post-2")

				// test-path-* and /blog/post-1 should be purged
				if _, _, err := storage.Get(key1); err == nil {
					t.Error("Expected /test-path-1 to be purged")
				}
				if _, _, err := storage.Get(key2); err == nil {
					t.Error("Expected /test-path-2 to be purged")
				}
				if _, _, err := storage.Get(key3); err == nil {
					t.Error("Expected /blog/post-1 to be purged")
				}
				// /blog/post-2 should remain
				if _, _, err := storage.Get(key4); err != nil {
					t.Error("Expected /blog/post-2 to remain in cache")
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Reset cache for each test
			_ = s.Storage.Flush()
			// Store test data with proper metadata including paths
			storeTestData("/test-path-1")
			storeTestData("/test-path-2")
			storeTestData("/blog/post-1")
			storeTestData("/blog/post-2")

			// Prepare request body
			var bodyBytes []byte
			if tt.body != nil {
				switch v := tt.body.(type) {
				case string:
					bodyBytes = []byte(v)
				case map[string]interface{}:
					bodyBytes, _ = json.Marshal(v)
				}
			}

			// Create request
			req := httptest.NewRequest(tt.method, tt.path, bytes.NewReader(bodyBytes))
			for k, v := range tt.headers {
				req.Header.Set(k, v)
			}

			// Create response recorder
			w := httptest.NewRecorder()

			// Handle the request
			err := s.handlePurgeRequest(w, req, s.Storage)
			if err != nil {
				t.Errorf("Unexpected error: %v", err)
			}

			// Check status code
			if w.Code != tt.expectedStatus {
				t.Errorf("Expected status %d, got %d", tt.expectedStatus, w.Code)
			}

			// Check cache state if provided
			if tt.checkCache != nil {
				tt.checkCache(t, s.Storage)
			}
		})
	}
}

func TestPurgePathVariations(t *testing.T) {
	// Test that purging a path removes all cache variations
	// (different query params, headers, cookies resulting in different cache keys)
	tmpDir := t.TempDir()

	s := &Sidekick{
		CacheDir:               tmpDir,
		CacheTTL:               60,
		PurgeURI:               "/__sidekick/purge",
		PurgeHeader:            "X-Sidekick-Purge",
		PurgeToken:             "test-token",
		CacheMemoryItemMaxSize: 1024 * 1024,      // 1MB
		CacheMemoryMaxSize:     10 * 1024 * 1024, // 10MB
		CacheMemoryMaxCount:    100,
		CacheResponseCodes:     []string{"200"},
		CacheKeyQueries:        []string{"page", "sort"},
		CacheKeyHeaders:        []string{"Accept-Language"},
		CacheKeyCookies:        []string{"session"},
	}

	// Initialize logger
	s.logger = zap.NewNop()

	// Initialize sync handler
	s.syncHandler = &SyncHandler{
		inFlight: make(map[string]*sync.Once),
	}

	// Initialize storage
	s.Storage = NewStorage(s.CacheDir, s.CacheTTL, 1*1024*1024, 10*1024*1024, 100, 0, 0, 0, s.logger)

	// Helper to generate cache keys with variations
	generateVariationKey := func(path string, queries map[string]string, headers map[string]string, cookies map[string]string) string {
		h := md5.New()
		h.Write([]byte(path))

		// Include query params like the real implementation
		for _, q := range s.CacheKeyQueries {
			if val, ok := queries[q]; ok {
				h.Write([]byte(q + "=" + val))
			}
		}

		// Include headers
		for _, hdr := range s.CacheKeyHeaders {
			if val, ok := headers[hdr]; ok {
				h.Write([]byte(hdr + ":" + val))
			}
		}

		// Include cookies
		for _, cookie := range s.CacheKeyCookies {
			if val, ok := cookies[cookie]; ok {
				h.Write([]byte(cookie + "=" + val))
			}
		}

		return fmt.Sprintf("%x", h.Sum(nil))
	}

	// Store multiple variations of /api/users
	variations := []struct {
		queries map[string]string
		headers map[string]string
		cookies map[string]string
		desc    string
	}{
		{
			queries: nil,
			headers: nil,
			cookies: nil,
			desc:    "no variations",
		},
		{
			queries: map[string]string{"page": "1"},
			headers: nil,
			cookies: nil,
			desc:    "with page query",
		},
		{
			queries: map[string]string{"page": "2", "sort": "name"},
			headers: nil,
			cookies: nil,
			desc:    "with page and sort queries",
		},
		{
			queries: nil,
			headers: map[string]string{"Accept-Language": "en-US"},
			cookies: nil,
			desc:    "with Accept-Language header",
		},
		{
			queries: map[string]string{"page": "1"},
			headers: map[string]string{"Accept-Language": "de-DE"},
			cookies: map[string]string{"session": "abc123"},
			desc:    "with all variations",
		},
	}

	// Store all variations
	path := "/api/users"
	storedKeys := []string{}
	for _, v := range variations {
		key := generateVariationKey(path, v.queries, v.headers, v.cookies)
		storedKeys = append(storedKeys, key)

		metadata := &Metadata{
			StateCode: 200,
			Header: [][]string{
				{"Content-Type", "application/json"},
			},
			Timestamp: time.Now().Unix(),
			Path:      path, // All variations have the same path
		}
		testData := []byte(fmt.Sprintf("data for %s: %s", path, v.desc))
		_ = s.Storage.SetWithKey(key, metadata, testData)
	}

	// Also store a different path
	otherPath := "/api/posts"
	otherKey := generateVariationKey(otherPath, nil, nil, nil)
	otherMetadata := &Metadata{
		StateCode: 200,
		Header: [][]string{
			{"Content-Type", "application/json"},
		},
		Timestamp: time.Now().Unix(),
		Path:      otherPath,
	}
	_ = s.Storage.SetWithKey(otherKey, otherMetadata, []byte("data for /api/posts"))

	// Verify all variations are in cache
	for i, key := range storedKeys {
		if _, _, err := s.Storage.Get(key); err != nil {
			t.Errorf("Expected variation %d (%s) to be in cache before purge", i, variations[i].desc)
		}
	}
	if _, _, err := s.Storage.Get(otherKey); err != nil {
		t.Error("Expected /api/posts to be in cache before purge")
	}

	// Create purge request for /api/users
	body := map[string]interface{}{
		"paths": []string{"/api/users"},
	}
	bodyBytes, _ := json.Marshal(body)
	req := httptest.NewRequest("POST", "/__sidekick/purge", bytes.NewReader(bodyBytes))
	req.Header.Set("X-Sidekick-Purge", "test-token")
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	err := s.handlePurgeRequest(w, req, s.Storage)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	// Check that all variations of /api/users are purged
	for i, key := range storedKeys {
		if _, _, err := s.Storage.Get(key); err == nil {
			t.Errorf("Expected variation %d (%s) to be purged", i, variations[i].desc)
		}
	}

	// Check that /api/posts remains
	if _, _, err := s.Storage.Get(otherKey); err != nil {
		t.Error("Expected /api/posts to remain in cache after purging /api/users")
	}

	// Verify response
	if w.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d", w.Code)
	}

	var response map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &response); err == nil {
		if response["status"] != "ok" {
			t.Errorf("Expected response status 'ok', got '%s'", response["status"])
		}
	}
}

func TestPurgeBackwardCompatibility(t *testing.T) {
	// Test that entries without Path in metadata are not affected by path-based purging
	// This ensures backward compatibility with cached items from before the Path field was added
	tmpDir := t.TempDir()

	s := &Sidekick{
		CacheDir:               tmpDir,
		CacheTTL:               60,
		PurgeURI:               "/__sidekick/purge",
		PurgeHeader:            "X-Sidekick-Purge",
		PurgeToken:             "test-token",
		CacheMemoryItemMaxSize: 1024 * 1024,      // 1MB
		CacheMemoryMaxSize:     10 * 1024 * 1024, // 10MB
		CacheMemoryMaxCount:    100,
		CacheResponseCodes:     []string{"200"},
	}

	// Initialize logger
	s.logger = zap.NewNop()

	// Initialize sync handler
	s.syncHandler = &SyncHandler{
		inFlight: make(map[string]*sync.Once),
	}

	// Initialize storage
	s.Storage = NewStorage(s.CacheDir, s.CacheTTL, 1*1024*1024, 10*1024*1024, 100, 0, 0, 0, s.logger)

	// Store entries with Path metadata (new style)
	withPathKey1 := "key_with_path_1"
	withPathMetadata1 := &Metadata{
		StateCode: 200,
		Header: [][]string{
			{"Content-Type", "text/html"},
		},
		Timestamp: time.Now().Unix(),
		Path:      "/api/users", // Has path
	}
	_ = s.Storage.SetWithKey(withPathKey1, withPathMetadata1, []byte("data with path 1"))

	withPathKey2 := "key_with_path_2"
	withPathMetadata2 := &Metadata{
		StateCode: 200,
		Header: [][]string{
			{"Content-Type", "text/html"},
		},
		Timestamp: time.Now().Unix(),
		Path:      "/api/posts", // Has path
	}
	_ = s.Storage.SetWithKey(withPathKey2, withPathMetadata2, []byte("data with path 2"))

	// Store entries WITHOUT Path metadata (old style - backward compatibility)
	withoutPathKey1 := "key_without_path_1"
	withoutPathMetadata1 := &Metadata{
		StateCode: 200,
		Header: [][]string{
			{"Content-Type", "text/html"},
		},
		Timestamp: time.Now().Unix(),
		// Path field is empty - simulating old cached entries
	}
	_ = s.Storage.SetWithKey(withoutPathKey1, withoutPathMetadata1, []byte("data without path 1"))

	withoutPathKey2 := "key_without_path_2"
	withoutPathMetadata2 := &Metadata{
		StateCode: 200,
		Header: [][]string{
			{"Content-Type", "text/html"},
		},
		Timestamp: time.Now().Unix(),
		// Path field is empty - simulating old cached entries
	}
	_ = s.Storage.SetWithKey(withoutPathKey2, withoutPathMetadata2, []byte("data without path 2"))

	// Verify all entries are in cache before purge
	if _, _, err := s.Storage.Get(withPathKey1); err != nil {
		t.Error("Expected entry with path 1 to be in cache")
	}
	if _, _, err := s.Storage.Get(withPathKey2); err != nil {
		t.Error("Expected entry with path 2 to be in cache")
	}
	if _, _, err := s.Storage.Get(withoutPathKey1); err != nil {
		t.Error("Expected entry without path 1 to be in cache")
	}
	if _, _, err := s.Storage.Get(withoutPathKey2); err != nil {
		t.Error("Expected entry without path 2 to be in cache")
	}

	// Purge /api/users
	body := map[string]interface{}{
		"paths": []string{"/api/users"},
	}
	bodyBytes, _ := json.Marshal(body)
	req := httptest.NewRequest("POST", "/__sidekick/purge", bytes.NewReader(bodyBytes))
	req.Header.Set("X-Sidekick-Purge", "test-token")
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	err := s.handlePurgeRequest(w, req, s.Storage)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	// Check results:
	// - Entry with path="/api/users" should be purged
	if _, _, err := s.Storage.Get(withPathKey1); err == nil {
		t.Error("Expected entry with path=/api/users to be purged")
	}

	// - Entry with path="/api/posts" should remain
	if _, _, err := s.Storage.Get(withPathKey2); err != nil {
		t.Error("Expected entry with path=/api/posts to remain in cache")
	}

	// - Entries without Path should remain (backward compatibility)
	if _, _, err := s.Storage.Get(withoutPathKey1); err != nil {
		t.Error("Expected entry without path 1 to remain in cache (backward compatibility)")
	}
	if _, _, err := s.Storage.Get(withoutPathKey2); err != nil {
		t.Error("Expected entry without path 2 to remain in cache (backward compatibility)")
	}

	// Purge with wildcard should also not affect entries without Path
	_ = s.Storage.SetWithKey(withPathKey1, withPathMetadata1, []byte("data with path 1")) // Re-add for next test

	body = map[string]interface{}{
		"paths": []string{"/api/*"},
	}
	bodyBytes, _ = json.Marshal(body)
	req = httptest.NewRequest("POST", "/__sidekick/purge", bytes.NewReader(bodyBytes))
	req.Header.Set("X-Sidekick-Purge", "test-token")
	req.Header.Set("Content-Type", "application/json")

	w = httptest.NewRecorder()
	err = s.handlePurgeRequest(w, req, s.Storage)
	if err != nil {
		t.Fatalf("Unexpected error in wildcard purge: %v", err)
	}

	// Both /api/users and /api/posts should be purged
	if _, _, err := s.Storage.Get(withPathKey1); err == nil {
		t.Error("Expected entry with path=/api/users to be purged by wildcard")
	}
	if _, _, err := s.Storage.Get(withPathKey2); err == nil {
		t.Error("Expected entry with path=/api/posts to be purged by wildcard")
	}

	// Entries without Path should still remain
	if _, _, err := s.Storage.Get(withoutPathKey1); err != nil {
		t.Error("Expected entry without path 1 to remain after wildcard purge")
	}
	if _, _, err := s.Storage.Get(withoutPathKey2); err != nil {
		t.Error("Expected entry without path 2 to remain after wildcard purge")
	}

	// Purge all should remove everything
	body = map[string]interface{}{}
	bodyBytes, _ = json.Marshal(body)
	req = httptest.NewRequest("POST", "/__sidekick/purge", bytes.NewReader(bodyBytes))
	req.Header.Set("X-Sidekick-Purge", "test-token")
	req.Header.Set("Content-Type", "application/json")

	w = httptest.NewRecorder()
	err = s.handlePurgeRequest(w, req, s.Storage)
	if err != nil {
		t.Fatalf("Unexpected error in purge all: %v", err)
	}

	// All entries should be gone
	list := s.Storage.List()
	totalCount := len(list["mem"]) + len(list["disk"])
	if totalCount != 0 {
		t.Errorf("Expected all entries to be purged, but found %d items", totalCount)
	}
}

func TestPurgeURIValidation(t *testing.T) {
	tests := []struct {
		name        string
		purgeURI    string
		shouldError bool
	}{
		{
			name:        "valid simple path",
			purgeURI:    "/purge",
			shouldError: false,
		},
		{
			name:        "valid path with hyphens",
			purgeURI:    "/sidekick-purge",
			shouldError: false,
		},
		{
			name:        "valid nested path",
			purgeURI:    "/api/v1/cache/purge",
			shouldError: false,
		},
		{
			name:        "invalid - no leading slash",
			purgeURI:    "purge",
			shouldError: true,
		},
		{
			name:        "invalid - uppercase letters",
			purgeURI:    "/PURGE",
			shouldError: true,
		},
		{
			name:        "valid path with underscore",
			purgeURI:    "/purge_cache",
			shouldError: false,
		},
		{
			name:        "invalid - special characters",
			purgeURI:    "/purge!",
			shouldError: true,
		},
		{
			name:        "invalid - spaces",
			purgeURI:    "/purge cache",
			shouldError: true,
		},
		{
			name:        "invalid - dots",
			purgeURI:    "/purge.php",
			shouldError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &Sidekick{
				CacheDir:            t.TempDir(),
				PurgeURI:            tt.purgeURI,
				PurgeHeader:         "X-Sidekick-Purge",
				PurgeToken:          "test-token",
				CacheMemoryMaxSize:  1024 * 1024,
				CacheMemoryMaxCount: 100,
			}

			ctx := caddy.Context{}

			err := s.Provision(ctx)

			if tt.shouldError && err == nil {
				t.Errorf("Expected error for purge_uri '%s', but got none", tt.purgeURI)
			}
			if !tt.shouldError && err != nil {
				t.Errorf("Unexpected error for purge_uri '%s': %v", tt.purgeURI, err)
			}
		})
	}
}
