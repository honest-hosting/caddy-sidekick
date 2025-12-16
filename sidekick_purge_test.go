package sidekick

import (
	"bytes"
	"encoding/json"
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
	s.Storage = NewStorage(s.CacheDir, s.CacheTTL, 10*1024*1024, 100, 0, 0, 0, s.logger)

	// Store some test data in cache
	testMetadata := &Metadata{
		StateCode: 200,
		Header: [][]string{
			{"Content-Type", "text/plain"},
		},
		Timestamp: time.Now().Unix(),
	}
	testData := []byte("test content")

	_ = s.Storage.Set("/test-path-1", testMetadata, testData)
	_ = s.Storage.Set("/test-path-2", testMetadata, testData)
	_ = s.Storage.Set("/blog/post-1", testMetadata, testData)
	_ = s.Storage.Set("/blog/post-2", testMetadata, testData)

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
				// These should be purged (paths directly used as keys in this test)
				if _, _, err := storage.Get("/test-path-1", "none"); err == nil {
					t.Error("Expected /test-path-1 to be purged")
				}
				if _, _, err := storage.Get("/blog/post-1", "none"); err == nil {
					t.Error("Expected /blog/post-1 to be purged")
				}
				// These should remain
				if _, _, err := storage.Get("/test-path-2", "none"); err != nil {
					t.Error("Expected /test-path-2 to remain in cache")
				}
				if _, _, err := storage.Get("/blog/post-2", "none"); err != nil {
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
				// Blog posts should be purged
				if _, _, err := storage.Get("/blog/post-1", "none"); err == nil {
					t.Error("Expected /blog/post-1 to be purged")
				}
				if _, _, err := storage.Get("/blog/post-2", "none"); err == nil {
					t.Error("Expected /blog/post-2 to be purged")
				}
				// Test paths should remain
				if _, _, err := storage.Get("/test-path-1", "none"); err != nil {
					t.Error("Expected /test-path-1 to remain in cache")
				}
				if _, _, err := storage.Get("/test-path-2", "none"); err != nil {
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
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Reset cache for each test
			_ = s.Storage.Flush()
			// Use SetWithKey to directly set the paths as keys (simulating what would be stored)
			_ = s.Storage.SetWithKey("/test-path-1", testMetadata, testData)
			_ = s.Storage.SetWithKey("/test-path-2", testMetadata, testData)
			_ = s.Storage.SetWithKey("/blog/post-1", testMetadata, testData)
			_ = s.Storage.SetWithKey("/blog/post-2", testMetadata, testData)

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
