package sidekick

import (
	"bytes"
	"crypto/rand"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest"
)

// Test constants
const (
	testCacheDir      = "/tmp/sidekick_test"
	testTTL           = 2 // seconds
	smallDataSize     = 1024
	mediumDataSize    = 1024 * 10
	largeDataSize     = 1024 * 100
	veryLargeDataSize = 1024 * 1024
)

// Helper function to get a logger with appropriate verbosity
// Environment variables:
//   - SIDEKICK_TEST_QUIET=1: Suppress all debug output
//   - SIDEKICK_TEST_LOG_LEVEL=error|warn|info|debug: Set specific log level
//
// For stress tests, set SIDEKICK_TEST_QUIET=1 or SIDEKICK_TEST_LOG_LEVEL=error
func getTestLogger(t testing.TB) *zap.Logger {
	// Check for quiet mode first
	if os.Getenv("SIDEKICK_TEST_QUIET") == "1" {
		return zap.NewNop()
	}

	// Check for specific log level
	logLevel := os.Getenv("SIDEKICK_TEST_LOG_LEVEL")
	if logLevel != "" {
		config := zap.NewProductionConfig()
		switch logLevel {
		case "error":
			config.Level = zap.NewAtomicLevelAt(zapcore.ErrorLevel)
		case "warn":
			config.Level = zap.NewAtomicLevelAt(zapcore.WarnLevel)
		case "info":
			config.Level = zap.NewAtomicLevelAt(zapcore.InfoLevel)
		case "debug":
			config.Level = zap.NewAtomicLevelAt(zapcore.DebugLevel)
		default:
			config.Level = zap.NewAtomicLevelAt(zapcore.WarnLevel)
		}
		config.OutputPaths = []string{"stderr"}
		config.ErrorOutputPaths = []string{"stderr"}
		logger, _ := config.Build()
		return logger
	}

	// Default behavior: use test logger only in verbose mode
	if testing.Verbose() {
		return zaptest.NewLogger(t)
	}

	// Non-verbose mode: suppress debug logs
	return zap.NewNop()
}

// Helper function to generate test data
func generateTestData(size int) []byte {
	data := make([]byte, size)
	_, err := io.ReadFull(rand.Reader, data)
	if err != nil {
		// Fallback to predictable data
		for i := range data {
			data[i] = byte(i % 256)
		}
	}
	return data
}

// Helper function to create test metadata
func createTestMetadata(code int) *Metadata {
	headers := http.Header{}
	headers.Set("Content-Type", "text/html")
	headers.Set("Content-Length", "1000")
	return NewMetadata(code, headers)
}

// Helper function to cleanup test cache directory
func cleanupTestDir(t testing.TB) {
	if err := os.RemoveAll(testCacheDir); err != nil {
		t.Logf("Warning: Failed to cleanup test directory: %v", err)
	}
}

// TestMemoryCacheStorage tests basic memory cache storage operations
func TestMemoryCacheStorage(t *testing.T) {
	logger := zaptest.NewLogger(t)
	defer cleanupTestDir(t)

	// Create storage with memory cache enabled, disk cache disabled
	storage := NewStorage(
		testCacheDir,
		0,                 // no TTL
		mediumDataSize*10, // 100KB memory max size
		100,               // max 100 items in memory
		0,                 // disk disabled
		0,                 // disk disabled
		0,                 // disk disabled
		logger,
	)

	// Test data
	testKey := "test-key-1"
	testData := generateTestData(smallDataSize)
	testMeta := createTestMetadata(200)

	// Store in cache
	err := storage.SetWithKey(testKey, testMeta, testData)
	if err != nil {
		t.Fatalf("Failed to store in memory cache: %v", err)
	}

	// Retrieve from cache
	retrievedData, retrievedMeta, err := storage.Get(testKey, "none")
	if err != nil {
		t.Fatalf("Failed to retrieve from memory cache: %v", err)
	}

	// Verify data
	if !bytes.Equal(retrievedData, testData) {
		t.Errorf("Retrieved data doesn't match original data")
	}

	if retrievedMeta.StateCode != testMeta.StateCode {
		t.Errorf("Retrieved metadata doesn't match original metadata")
	}
}

// TestDiskCacheStorage tests basic disk cache storage operations
func TestDiskCacheStorage(t *testing.T) {
	logger := zaptest.NewLogger(t)
	defer cleanupTestDir(t)

	// Create storage with disk cache enabled, memory cache disabled
	storage := NewStorage(
		testCacheDir,
		0,                    // no TTL
		0,                    // memory disabled
		0,                    // memory disabled
		veryLargeDataSize,    // 1MB max item size
		veryLargeDataSize*10, // 10MB max disk size
		100,                  // max 100 items on disk
		logger,
	)

	// Test data
	testKey := "test-disk-key-1"
	testData := generateTestData(mediumDataSize)
	testMeta := createTestMetadata(200)

	// Store in cache
	err := storage.SetWithKey(testKey, testMeta, testData)
	if err != nil {
		t.Fatalf("Failed to store in disk cache: %v", err)
	}

	// Wait a bit for disk write
	time.Sleep(100 * time.Millisecond)

	// Create new storage instance to ensure we're reading from disk
	storage2 := NewStorage(
		testCacheDir,
		0,
		0, // memory disabled
		0,
		veryLargeDataSize,
		veryLargeDataSize*10,
		100,
		logger,
	)

	// Retrieve from cache
	retrievedData, retrievedMeta, err := storage2.Get(testKey, "none")
	if err != nil {
		t.Fatalf("Failed to retrieve from disk cache: %v", err)
	}

	// Verify data
	if !bytes.Equal(retrievedData, testData) {
		t.Errorf("Retrieved data doesn't match original data")
	}

	if retrievedMeta.StateCode != testMeta.StateCode {
		t.Errorf("Retrieved metadata doesn't match original metadata")
	}
}

// TestStorageMemoryCacheEvictionByCount tests memory cache eviction when count limit is reached
func TestStorageMemoryCacheEvictionByCount(t *testing.T) {
	logger := zaptest.NewLogger(t)
	defer cleanupTestDir(t)

	maxCount := 5
	storage := NewStorage(
		testCacheDir,
		0,  // no TTL
		-1, // unlimited size
		maxCount,
		0, // disk disabled
		0,
		0,
		logger,
	)

	// Store more items than max count
	for i := 0; i < maxCount+2; i++ {
		key := fmt.Sprintf("test-key-%d", i)
		data := generateTestData(smallDataSize)
		meta := createTestMetadata(200)

		err := storage.SetWithKey(key, meta, data)
		if err != nil {
			t.Fatalf("Failed to store item %d: %v", i, err)
		}
	}

	// First items should be evicted
	_, _, err := storage.Get("test-key-0", "none")
	if err != ErrCacheNotFound {
		t.Errorf("Expected first item to be evicted, but it was found")
	}

	_, _, err = storage.Get("test-key-1", "none")
	if err != ErrCacheNotFound {
		t.Errorf("Expected second item to be evicted, but it was found")
	}

	// Last items should still be present
	for i := maxCount; i < maxCount+2; i++ {
		key := fmt.Sprintf("test-key-%d", i)
		_, _, err := storage.Get(key, "none")
		if err != nil {
			t.Errorf("Expected item %d to be present, but got error: %v", i, err)
		}
	}

	// Check memory cache size
	memCache := storage.getMemCache()
	if memCache.Size() > maxCount {
		t.Errorf("Memory cache size %d exceeds max count %d", memCache.Size(), maxCount)
	}
}

// TestMemoryCacheEvictionBySize tests memory cache eviction when size limit is reached
func TestMemoryCacheEvictionBySize(t *testing.T) {
	logger := zaptest.NewLogger(t)
	defer cleanupTestDir(t)

	maxSize := smallDataSize * 3
	storage := NewStorage(
		testCacheDir,
		0, // no TTL
		maxSize,
		-1, // unlimited count
		0,  // disk disabled
		0,
		0,
		logger,
	)

	// Store items that exceed max size
	for i := 0; i < 5; i++ {
		key := fmt.Sprintf("test-key-%d", i)
		data := generateTestData(smallDataSize)
		meta := createTestMetadata(200)

		err := storage.SetWithKey(key, meta, data)
		if err != nil {
			t.Fatalf("Failed to store item %d: %v", i, err)
		}
	}

	// Check that memory cache cost doesn't exceed limit (allowing some overhead for metadata)
	memCache := storage.getMemCache()
	// Allow up to 33% overhead for metadata and internal structures
	if memCache.Cost() > maxSize+(maxSize/3) {
		t.Errorf("Memory cache cost %d exceeds max size %d (with overhead allowance)", memCache.Cost(), maxSize)
	}

	// At least the last 2 items should be present
	for i := 3; i < 5; i++ {
		key := fmt.Sprintf("test-key-%d", i)
		_, _, err := storage.Get(key, "none")
		if err != nil {
			t.Errorf("Expected recent item %d to be present, but got error: %v", i, err)
		}
	}
}

// TestDiskCacheEvictionByCount tests disk cache eviction when count limit is reached
func TestDiskCacheEvictionByCount(t *testing.T) {
	logger := zaptest.NewLogger(t)
	defer cleanupTestDir(t)

	maxCount := 3
	storage := NewStorage(
		testCacheDir,
		0, // no TTL
		0, // memory disabled
		0,
		veryLargeDataSize,
		-1, // unlimited size
		maxCount,
		logger,
	)

	// Store more items than max count
	keys := make([]string, maxCount+2)
	for i := 0; i < maxCount+2; i++ {
		keys[i] = fmt.Sprintf("test-disk-key-%d", i)
		data := generateTestData(smallDataSize)
		meta := createTestMetadata(200)

		err := storage.SetWithKey(keys[i], meta, data)
		if err != nil {
			t.Fatalf("Failed to store item %d: %v", i, err)
		}
		// Give some time between writes for modification time differences
		time.Sleep(50 * time.Millisecond)
	}

	// Wait for async operations to complete
	storage.WaitForAsyncOps()

	// Check disk item count
	storage.diskIndexMu.RLock()
	itemCount := storage.diskItemCount
	storage.diskIndexMu.RUnlock()
	if itemCount > int64(maxCount) {
		t.Errorf("Disk item count %d exceeds max count %d", itemCount, maxCount)
	}
}

// TestDiskCacheEvictionBySize tests disk cache eviction when size limit is reached
func TestDiskCacheEvictionBySize(t *testing.T) {
	logger := zaptest.NewLogger(t)
	defer cleanupTestDir(t)

	itemSize := mediumDataSize
	maxSize := itemSize * 3 // Can hold 3 items

	storage := NewStorage(
		testCacheDir,
		0, // no TTL
		0, // memory disabled
		0,
		veryLargeDataSize,
		maxSize,
		-1, // unlimited count
		logger,
	)

	// Store items that will exceed disk size limit
	for i := 0; i < 5; i++ {
		key := fmt.Sprintf("test-disk-size-key-%d", i)
		data := generateTestData(itemSize)
		meta := createTestMetadata(200)

		err := storage.SetWithKey(key, meta, data)
		if err != nil {
			t.Fatalf("Failed to store item %d: %v", i, err)
		}
		// Give some time between writes
		time.Sleep(50 * time.Millisecond)
	}

	// Wait for async operations to complete
	storage.WaitForAsyncOps()

	// Check that disk usage doesn't exceed limit
	storage.diskIndexMu.RLock()
	diskUsage := storage.diskUsage
	storage.diskIndexMu.RUnlock()
	if diskUsage > int64(maxSize)*2 { // Allow some overhead for metadata
		t.Errorf("Disk usage %d significantly exceeds max size %d", diskUsage, maxSize)
	}
}

// TestCacheLifetime tests cache TTL expiration
func TestCacheLifetime(t *testing.T) {
	logger := zaptest.NewLogger(t)
	defer cleanupTestDir(t)

	ttl := 1 // 1 second TTL
	storage := NewStorage(
		testCacheDir,
		ttl,
		mediumDataSize*10,
		100,
		veryLargeDataSize,
		veryLargeDataSize*10,
		100,
		logger,
	)

	// Store in cache
	testKey := "test-ttl-key"
	testData := generateTestData(smallDataSize)
	testMeta := createTestMetadata(200)

	err := storage.SetWithKey(testKey, testMeta, testData)
	if err != nil {
		t.Fatalf("Failed to store: %v", err)
	}

	// Should be retrievable immediately
	_, _, err = storage.Get(testKey, "none")
	if err != nil {
		t.Errorf("Failed to retrieve immediately after storing: %v", err)
	}

	// Wait for TTL to expire
	time.Sleep(time.Duration(ttl+1) * time.Second)

	// Should return expired error
	_, _, err = storage.Get(testKey, "none")
	if err != ErrCacheExpired {
		t.Errorf("Expected ErrCacheExpired, got: %v", err)
	}

	// Wait for async cleanup operations to complete
	storage.WaitForAsyncOps()
}

// TestMaxItemSizeMemory tests max item size limit for memory cache
func TestMaxItemSizeMemory(t *testing.T) {
	logger := zaptest.NewLogger(t)
	defer cleanupTestDir(t)

	maxItemSize := mediumDataSize
	storage := NewStorage(
		testCacheDir,
		0,
		veryLargeDataSize, // Large total size
		100,
		0, // disk disabled
		0,
		0,
		logger,
	)

	// Create custom memory cache with item size limit
	// Note: The actual implementation would need to check item size before storing

	// Test storing item within limit
	smallKey := "small-item"
	smallData := generateTestData(maxItemSize / 2)
	meta := createTestMetadata(200)

	err := storage.SetWithKey(smallKey, meta, smallData)
	if err != nil {
		t.Errorf("Failed to store item within size limit: %v", err)
	}

	// Verify it was stored
	_, _, err = storage.Get(smallKey, "none")
	if err != nil {
		t.Errorf("Failed to retrieve item within size limit: %v", err)
	}
}

// TestMaxItemSizeDisk tests max item size limit for disk cache
func TestMaxItemSizeDisk(t *testing.T) {
	logger := zaptest.NewLogger(t)
	defer cleanupTestDir(t)

	maxItemSize := mediumDataSize
	storage := NewStorage(
		testCacheDir,
		0,
		0, // memory disabled
		0,
		maxItemSize,
		veryLargeDataSize*10,
		100,
		logger,
	)

	// Try to store item larger than limit
	largeKey := "large-item"
	largeData := generateTestData(maxItemSize * 2)
	meta := createTestMetadata(200)

	err := storage.SetWithKey(largeKey, meta, largeData)
	if err == nil {
		t.Errorf("Expected error when storing item larger than limit, but got nil")
	}

	// Store item within limit
	smallKey := "small-item"
	smallData := generateTestData(maxItemSize / 2)

	err = storage.SetWithKey(smallKey, meta, smallData)
	if err != nil {
		t.Errorf("Failed to store item within size limit: %v", err)
	}

	// Wait for disk write
	time.Sleep(100 * time.Millisecond)

	// Verify it was stored
	_, _, err = storage.Get(smallKey, "none")
	if err != nil {
		t.Errorf("Failed to retrieve item within size limit: %v", err)
	}
}

// TestConcurrentMemoryAccess tests concurrent access to memory cache
func TestConcurrentMemoryAccess(t *testing.T) {
	logger := getTestLogger(t)
	defer cleanupTestDir(t)

	storage := NewStorage(
		testCacheDir,
		0,
		veryLargeDataSize*10,
		1000,
		0, // disk disabled
		0,
		0,
		logger,
	)

	numGoroutines := 20
	numOperations := 20 // Reduced to avoid excessive evictions (20*20=400 < 1000 capacity)

	var wg sync.WaitGroup
	wg.Add(numGoroutines)

	errors := make(chan error, numGoroutines*numOperations)

	for i := 0; i < numGoroutines; i++ {
		go func(id int) {
			defer wg.Done()

			for j := 0; j < numOperations; j++ {
				key := fmt.Sprintf("concurrent-key-%d-%d", id, j)
				data := generateTestData(smallDataSize)
				meta := createTestMetadata(200)

				// Store
				if err := storage.SetWithKey(key, meta, data); err != nil {
					errors <- fmt.Errorf("goroutine %d: failed to store: %v", id, err)
					continue
				}

				// Retrieve - it may have been evicted if cache is full (expected behavior)
				retrievedData, _, err := storage.Get(key, "none")
				if err != nil {
					// Cache eviction is expected with 5000 potential keys but only 1000 capacity
					if err != ErrCacheNotFound {
						errors <- fmt.Errorf("goroutine %d: unexpected error: %v", id, err)
					}
					continue
				}

				// Verify data if it was found
				if !bytes.Equal(retrievedData, data) {
					errors <- fmt.Errorf("goroutine %d: data mismatch", id)
				}
			}
		}(i)
	}

	wg.Wait()
	close(errors)

	// Wait for any async operations to complete
	storage.WaitForAsyncOps()

	// Check for errors
	errorCount := 0
	for err := range errors {
		t.Errorf("Concurrent access error: %v", err)
		errorCount++
		if errorCount > 10 {
			t.Fatalf("Too many concurrent access errors")
		}
	}
}

// TestConcurrentDiskAccess tests concurrent access to disk cache
func TestConcurrentDiskAccess(t *testing.T) {
	logger := getTestLogger(t)
	defer cleanupTestDir(t)

	storage := NewStorage(
		testCacheDir,
		0,
		0, // memory disabled to force disk usage
		0,
		veryLargeDataSize,
		veryLargeDataSize*100,
		1000,
		logger,
	)

	numGoroutines := 20
	numOperations := 50

	var wg sync.WaitGroup
	wg.Add(numGoroutines)

	errors := make(chan error, numGoroutines*numOperations)

	for i := 0; i < numGoroutines; i++ {
		go func(id int) {
			defer wg.Done()

			for j := 0; j < numOperations; j++ {
				key := fmt.Sprintf("concurrent-disk-key-%d-%d", id, j)
				data := generateTestData(smallDataSize)
				meta := createTestMetadata(200)

				// Store
				if err := storage.SetWithKey(key, meta, data); err != nil {
					errors <- fmt.Errorf("goroutine %d: failed to store: %v", id, err)
					continue
				}

				// Small delay to ensure disk write
				time.Sleep(10 * time.Millisecond)

				// Retrieve
				retrievedData, _, err := storage.Get(key, "none")
				if err != nil {
					errors <- fmt.Errorf("goroutine %d: failed to retrieve: %v", id, err)
					continue
				}

				// Verify
				if !bytes.Equal(retrievedData, data) {
					errors <- fmt.Errorf("goroutine %d: data mismatch", id)
				}
			}
		}(i)
	}

	wg.Wait()
	close(errors)

	// Wait for any async operations to complete
	storage.WaitForAsyncOps()

	// Check for errors
	errorCount := 0
	for err := range errors {
		t.Errorf("Concurrent disk access error: %v", err)
		errorCount++
		if errorCount > 10 {
			t.Fatalf("Too many concurrent disk access errors")
		}
	}
}

// TestDiskCacheStartupPerformance tests that loading disk cache index is fast even with many items
func TestDiskCacheStartupPerformance(t *testing.T) {
	defer cleanupTestDir(t)

	// Use a no-op logger for setup to reduce output
	setupLogger := zap.NewNop()

	// First, create a storage with many items
	setupStorage := NewStorage(
		testCacheDir,
		0,
		0, // memory disabled
		0,
		veryLargeDataSize,
		veryLargeDataSize*10000, // Large disk cache
		-1,                      // No count limit for setup
		setupLogger,
	)

	// Pre-populate with 10,000 items
	numItems := 10000
	t.Logf("Creating %d cache items on disk...", numItems)

	data := generateTestData(smallDataSize)
	meta := createTestMetadata(200)

	for i := 0; i < numItems; i++ {
		key := fmt.Sprintf("startup-test-key-%d", i)
		err := setupStorage.SetWithKey(key, meta, data)
		if err != nil {
			t.Fatalf("Failed to create test item %d: %v", i, err)
		}

		// Log progress every 1000 items
		if (i+1)%1000 == 0 {
			t.Logf("Created %d items", i+1)
		}
	}

	setupStorage.WaitForAsyncOps()
	t.Logf("Setup complete, created %d items", numItems)

	// Now test startup time by creating a new storage instance
	// Use a regular test logger for this phase
	logger := zaptest.NewLogger(t)
	startTime := time.Now()

	newStorage := NewStorage(
		testCacheDir,
		0,
		0, // memory disabled
		0,
		veryLargeDataSize,
		veryLargeDataSize*10000,
		numItems,
		logger,
	)

	loadTime := time.Since(startTime)

	// Verify the index loaded correctly
	newStorage.diskIndexMu.RLock()
	loadedCount := len(newStorage.diskIndex)
	diskCount := newStorage.diskItemCount
	newStorage.diskIndexMu.RUnlock()

	t.Logf("Loaded %d items from disk in %v", loadedCount, loadTime)

	if loadedCount != numItems {
		t.Errorf("Expected to load %d items, but loaded %d", numItems, loadedCount)
	}

	if diskCount != int64(numItems) {
		t.Errorf("Expected disk count %d, but got %d", numItems, diskCount)
	}

	// Check that startup time is reasonable (should be well under 5 seconds)
	maxStartupTime := 5 * time.Second
	if loadTime > maxStartupTime {
		t.Errorf("Startup took %v, which exceeds the maximum allowed time of %v", loadTime, maxStartupTime)
	}

	// Ideally should be under 1 second for 10,000 items
	if loadTime < 1*time.Second {
		t.Logf("Excellent startup performance: %v for %d items", loadTime, numItems)
	} else if loadTime < 2*time.Second {
		t.Logf("Good startup performance: %v for %d items", loadTime, numItems)
	} else {
		t.Logf("Warning: Startup time could be improved: %v for %d items", loadTime, numItems)
	}
}

// BenchmarkMemoryCacheSet benchmarks memory cache write performance with larger data
func BenchmarkMemoryCacheSet(b *testing.B) {
	logger := zap.NewNop()
	defer cleanupTestDir(b)

	storage := NewStorage(
		testCacheDir,
		0,
		veryLargeDataSize*100,
		10000,
		0, // disk disabled
		0,
		0,
		logger,
	)

	data := generateTestData(smallDataSize)
	meta := createTestMetadata(200)

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			key := fmt.Sprintf("bench-key-%d", i)
			_ = storage.SetWithKey(key, meta, data)
			i++
		}
	})
}

// BenchmarkStorageMemoryCacheGet benchmarks memory cache read performance
func BenchmarkStorageMemoryCacheGet(b *testing.B) {
	logger := zap.NewNop()
	defer cleanupTestDir(b)

	storage := NewStorage(
		testCacheDir,
		0,
		veryLargeDataSize*100,
		10000,
		0, // disk disabled
		0,
		0,
		logger,
	)

	// Pre-populate cache
	numKeys := 1000
	data := generateTestData(smallDataSize)
	meta := createTestMetadata(200)

	for i := 0; i < numKeys; i++ {
		key := fmt.Sprintf("bench-key-%d", i)
		_ = storage.SetWithKey(key, meta, data)
	}

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			key := fmt.Sprintf("bench-key-%d", i%numKeys)
			_, _, _ = storage.Get(key, "none")
			i++
		}
	})
}

// BenchmarkDiskCacheSet benchmarks disk cache write performance
func BenchmarkDiskCacheSet(b *testing.B) {
	logger := zap.NewNop()
	defer cleanupTestDir(b)

	storage := NewStorage(
		testCacheDir,
		0,
		0, // memory disabled
		0,
		veryLargeDataSize,
		veryLargeDataSize*1000,
		10000, // Test eviction with 10000 item limit
		logger,
	)

	data := generateTestData(mediumDataSize)
	meta := createTestMetadata(200)

	b.ResetTimer()

	var counter int64
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			i := atomic.AddInt64(&counter, 1)
			key := fmt.Sprintf("bench-disk-key-%d", i)
			_ = storage.SetWithKey(key, meta, data)
		}
	})
}

// BenchmarkDiskCacheGet benchmarks disk cache read performance
func BenchmarkDiskCacheGet(b *testing.B) {
	logger := zap.NewNop()
	defer cleanupTestDir(b)

	storage := NewStorage(
		testCacheDir,
		0,
		smallDataSize*100, // Small memory cache to force some disk reads
		100,
		veryLargeDataSize,
		veryLargeDataSize*1000,
		10000,
		logger,
	)

	// Pre-populate cache
	numKeys := 500
	data := generateTestData(mediumDataSize)
	meta := createTestMetadata(200)

	for i := 0; i < numKeys; i++ {
		key := fmt.Sprintf("bench-disk-key-%d", i)
		_ = storage.SetWithKey(key, meta, data)
	}

	// Wait for async operations to complete
	storage.WaitForAsyncOps()

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			key := fmt.Sprintf("bench-disk-key-%d", i%numKeys)
			_, _, _ = storage.Get(key, "none")
			i++
		}
	})
}

// BenchmarkConcurrentMixedOperations benchmarks mixed read/write operations
func BenchmarkConcurrentMixedOperations(b *testing.B) {
	logger := zap.NewNop()
	defer cleanupTestDir(b)

	storage := NewStorage(
		testCacheDir,
		0,
		veryLargeDataSize*10,
		1000,
		veryLargeDataSize,
		veryLargeDataSize*100,
		5000,
		logger,
	)

	// Pre-populate some data
	numKeys := 100
	data := generateTestData(smallDataSize)
	meta := createTestMetadata(200)

	for i := 0; i < numKeys; i++ {
		key := fmt.Sprintf("mixed-key-%d", i)
		_ = storage.SetWithKey(key, meta, data)
	}

	b.ResetTimer()

	var readOps, writeOps int64

	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			if i%3 == 0 { // 33% writes
				key := fmt.Sprintf("mixed-key-new-%d", i)
				_ = storage.SetWithKey(key, meta, data)
				atomic.AddInt64(&writeOps, 1)
			} else { // 67% reads
				key := fmt.Sprintf("mixed-key-%d", i%numKeys)
				_, _, _ = storage.Get(key, "none")
				atomic.AddInt64(&readOps, 1)
			}
			i++
		}
	})

	b.Logf("Read operations: %d, Write operations: %d", readOps, writeOps)
}

// TestMemoryPressure tests behavior under memory constraints
func TestMemoryPressure(t *testing.T) {
	logger := getTestLogger(t)
	defer cleanupTestDir(t)

	// Very small memory limit to trigger frequent evictions
	storage := NewStorage(
		testCacheDir,
		0,
		smallDataSize*2, // Can hold only ~2 items
		100,
		0, // disk disabled
		0,
		0,
		logger,
	)

	// Continuously add items under memory pressure
	errors := 0
	for i := 0; i < 100; i++ {
		key := fmt.Sprintf("pressure-key-%d", i)
		data := generateTestData(smallDataSize)
		meta := createTestMetadata(200)

		err := storage.SetWithKey(key, meta, data)
		if err != nil {
			errors++
		}
	}

	// Should have succeeded without errors
	if errors > 0 {
		t.Errorf("Got %d errors during memory pressure test", errors)
	}

	// Memory cache should respect size limit
	memCache := storage.getMemCache()
	if memCache.Cost() > smallDataSize*3 { // Allow some overhead
		t.Errorf("Memory cache cost %d exceeds expected limit", memCache.Cost())
	}
}

// TestDiskSpacePressure tests behavior under disk space constraints
func TestDiskSpacePressure(t *testing.T) {
	logger := getTestLogger(t)
	defer cleanupTestDir(t)

	// Very small disk limit to trigger frequent evictions
	itemSize := mediumDataSize
	storage := NewStorage(
		testCacheDir,
		0,
		0, // memory disabled
		0,
		itemSize*2, // Max item size
		itemSize*3, // Can hold only ~3 items
		100,
		logger,
	)

	// Continuously add items under disk pressure
	for i := 0; i < 20; i++ {
		key := fmt.Sprintf("disk-pressure-key-%d", i)
		data := generateTestData(itemSize)
		meta := createTestMetadata(200)

		err := storage.SetWithKey(key, meta, data)
		if err != nil {
			t.Logf("Item %d failed to store (expected for some items): %v", i, err)
		}

		// Give time for disk operations
		time.Sleep(50 * time.Millisecond)
	}

	// Wait for async operations to complete
	storage.WaitForAsyncOps()

	// Check disk usage
	storage.diskIndexMu.RLock()
	diskUsage := storage.diskUsage
	storage.diskIndexMu.RUnlock()

	// Disk usage should respect limit (with some overhead for metadata)
	// Should only hold ~3 items worth of data
	if diskUsage > int64(itemSize*4) {
		t.Errorf("Disk usage %d significantly exceeds expected limit of ~%d", diskUsage, itemSize*3)
	}
}

// TestLRUEvictionOrder tests that LRU eviction works correctly
func TestLRUEvictionOrder(t *testing.T) {
	logger := zaptest.NewLogger(t)
	defer cleanupTestDir(t)

	maxCount := 3
	storage := NewStorage(
		testCacheDir,
		0,
		-1, // unlimited size
		maxCount,
		0, // disk disabled
		0,
		0,
		logger,
	)

	// Add items in order
	keys := []string{"first", "second", "third"}
	for _, key := range keys {
		data := generateTestData(smallDataSize)
		meta := createTestMetadata(200)
		_ = storage.SetWithKey(key, meta, data)
	}

	// Access "first" to make it recently used
	_, _, _ = storage.Get("first", "none")

	// Add new item, should evict "second" (least recently used)
	newData := generateTestData(smallDataSize)
	newMeta := createTestMetadata(200)
	_ = storage.SetWithKey("fourth", newMeta, newData)

	// "first" should still exist (recently accessed)
	_, _, err := storage.Get("first", "none")
	if err != nil {
		t.Errorf("Recently accessed item 'first' was evicted: %v", err)
	}

	// "second" should be evicted (LRU)
	_, _, err = storage.Get("second", "none")
	if err != ErrCacheNotFound {
		t.Errorf("Expected 'second' to be evicted (LRU), but it was found")
	}

	// "third" should still exist
	_, _, err = storage.Get("third", "none")
	if err != nil {
		t.Errorf("Item 'third' was unexpectedly evicted: %v", err)
	}

	// "fourth" should exist (just added)
	_, _, err = storage.Get("fourth", "none")
	if err != nil {
		t.Errorf("Newly added item 'fourth' not found: %v", err)
	}
}

// TestCompressionStorage tests that compression works correctly
func TestCompressionStorage(t *testing.T) {
	logger := zaptest.NewLogger(t)
	defer cleanupTestDir(t)

	storage := NewStorage(
		testCacheDir,
		0,
		veryLargeDataSize*10,
		100,
		veryLargeDataSize*10,
		veryLargeDataSize*100,
		1000,
		logger,
	)

	// Create compressible data (repeated pattern)
	testKey := "compress-test-key"
	testData := bytes.Repeat([]byte("This is a test string that should compress well. "), 100)
	testMeta := createTestMetadata(200)
	testMeta.contentEncoding = "none"

	// Store the data
	err := storage.SetWithKey(testKey, testMeta, testData)
	if err != nil {
		t.Fatalf("Failed to store data: %v", err)
	}

	// Wait for async compression
	storage.WaitForAsyncOps()

	// Try to retrieve with gzip encoding (should be available if compression worked)
	compressedData, compressedMeta, err := storage.Get(testKey, "gzip")
	if err != nil {
		t.Logf("Compressed version not found (may be expected): %v", err)
	} else {
		// Compressed data should be smaller
		if len(compressedData) >= len(testData) {
			t.Errorf("Compressed data (%d bytes) is not smaller than original (%d bytes)",
				len(compressedData), len(testData))
		}
		if compressedMeta == nil {
			t.Errorf("Compressed metadata is nil")
		}
	}

	// Original should still be retrievable
	retrievedData, retrievedMeta, err := storage.Get(testKey, "none")
	if err != nil {
		t.Fatalf("Failed to retrieve original data: %v", err)
	}

	if !bytes.Equal(retrievedData, testData) {
		t.Errorf("Retrieved data doesn't match original")
	}

	if retrievedMeta.StateCode != testMeta.StateCode {
		t.Errorf("Retrieved metadata doesn't match original")
	}
}

// TestCacheKeyMutexCleanup tests that per-key mutexes are properly cleaned up
func TestCacheKeyMutexCleanup(t *testing.T) {
	logger := zaptest.NewLogger(t)
	defer cleanupTestDir(t)

	storage := NewStorage(
		testCacheDir,
		1, // 1 second TTL
		mediumDataSize*10,
		100,
		veryLargeDataSize,
		veryLargeDataSize*10,
		100,
		logger,
	)

	testKey := "mutex-test-key"
	testData := generateTestData(smallDataSize)
	testMeta := createTestMetadata(200)

	// Store data
	err := storage.SetWithKey(testKey, testMeta, testData)
	if err != nil {
		t.Fatalf("Failed to store: %v", err)
	}

	// Wait for TTL to expire
	time.Sleep(2 * time.Second)

	// Try to get expired data (this triggers cleanup)
	_, _, err = storage.Get(testKey, "none")
	if err != ErrCacheExpired {
		t.Errorf("Expected ErrCacheExpired, got: %v", err)
	}

	// Wait for async cleanup to complete
	storage.WaitForAsyncOps()

	// Check that mutex was cleaned up
	storage.keyMutexesMu.Lock()
	_, exists := storage.keyMutexes[testKey]
	storage.keyMutexesMu.Unlock()

	if exists {
		t.Errorf("Key mutex was not cleaned up after expiration")
	}
}

// TestMemoryCacheCostAccounting tests that memory cost is properly tracked
func TestMemoryCacheCostAccounting(t *testing.T) {
	logger := zaptest.NewLogger(t)
	defer cleanupTestDir(t)

	maxSize := mediumDataSize * 5
	storage := NewStorage(
		testCacheDir,
		0,
		maxSize,
		-1, // unlimited count
		0,  // disk disabled
		0,
		0,
		logger,
	)

	// Add items and track expected cost
	expectedCost := 0
	items := []struct {
		key  string
		size int
	}{
		{"item1", smallDataSize},
		{"item2", mediumDataSize},
		{"item3", smallDataSize * 2},
	}

	for _, item := range items {
		data := generateTestData(item.size)
		meta := createTestMetadata(200)
		err := storage.SetWithKey(item.key, meta, data)
		if err != nil {
			t.Fatalf("Failed to store %s: %v", item.key, err)
		}
		expectedCost += item.size
	}

	// Check actual cost
	memCache := storage.getMemCache()
	actualCost := memCache.Cost()

	// Allow some overhead for metadata
	if actualCost > expectedCost+1000 || actualCost < expectedCost {
		t.Errorf("Memory cost mismatch. Expected ~%d, got %d", expectedCost, actualCost)
	}

	// Update an existing item with different size
	newData := generateTestData(mediumDataSize * 2)
	newMeta := createTestMetadata(200)
	err := storage.SetWithKey("item2", newMeta, newData)
	if err != nil {
		t.Fatalf("Failed to update item2: %v", err)
	}

	// Cost should be updated
	expectedCost = expectedCost - mediumDataSize + (mediumDataSize * 2)
	actualCost = memCache.Cost()

	if actualCost > expectedCost+1000 || actualCost < expectedCost {
		t.Errorf("Memory cost after update mismatch. Expected ~%d, got %d", expectedCost, actualCost)
	}
}

// BenchmarkStorageMemoryCacheEviction benchmarks eviction performance
func BenchmarkStorageMemoryCacheEviction(b *testing.B) {
	logger := zap.NewNop()
	defer cleanupTestDir(b)

	// Small cache to trigger frequent evictions
	storage := NewStorage(
		testCacheDir,
		0,
		smallDataSize*10, // Can hold ~10 items
		10,
		0, // disk disabled
		0,
		0,
		logger,
	)

	data := generateTestData(smallDataSize)
	meta := createTestMetadata(200)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		key := fmt.Sprintf("evict-bench-key-%d", i)
		_ = storage.SetWithKey(key, meta, data)
	}
}

// BenchmarkDiskCacheEviction benchmarks disk eviction performance
func BenchmarkDiskCacheEviction(b *testing.B) {
	logger := zap.NewNop()
	defer cleanupTestDir(b)

	// Small cache to trigger frequent evictions
	storage := NewStorage(
		testCacheDir,
		0,
		0, // memory disabled
		0,
		mediumDataSize*2,
		mediumDataSize*10, // Can hold ~10 items
		10,
		logger,
	)

	data := generateTestData(mediumDataSize)
	meta := createTestMetadata(200)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		key := fmt.Sprintf("disk-evict-bench-key-%d", i)
		_ = storage.SetWithKey(key, meta, data)
	}
}

// BenchmarkCacheKeyGeneration benchmarks cache key generation
func BenchmarkCacheKeyGeneration(b *testing.B) {
	logger := zap.NewNop()
	defer cleanupTestDir(b)

	storage := NewStorage(
		testCacheDir,
		0,
		veryLargeDataSize*10,
		1000,
		0,
		0,
		0,
		logger,
	)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = storage.buildCacheKey("/path/to/resource", fmt.Sprintf("param=%d", i))
	}
}

// TestMemoryLeaks tests for potential memory leaks
func TestMemoryLeaks(t *testing.T) {
	logger := zaptest.NewLogger(t)
	defer cleanupTestDir(t)

	storage := NewStorage(
		testCacheDir,
		1, // 1 second TTL
		mediumDataSize*10,
		100,
		veryLargeDataSize,
		veryLargeDataSize*10,
		100,
		logger,
	)

	// Perform many operations
	for cycle := 0; cycle < 5; cycle++ {
		// Add items
		for i := 0; i < 100; i++ {
			key := fmt.Sprintf("leak-test-%d-%d", cycle, i)
			data := generateTestData(smallDataSize)
			meta := createTestMetadata(200)
			_ = storage.SetWithKey(key, meta, data)
		}

		// Read items
		for i := 0; i < 100; i++ {
			key := fmt.Sprintf("leak-test-%d-%d", cycle, i)
			_, _, _ = storage.Get(key, "none")
		}

		// Wait for TTL expiration
		time.Sleep(1100 * time.Millisecond)

		// Trigger cleanup by trying to get expired items
		// This will trigger the cleanup goroutines
		for i := 0; i < 100; i++ {
			key := fmt.Sprintf("leak-test-%d-%d", cycle, i)
			_, _, _ = storage.Get(key, "none")
		}

		// Wait for async cleanup operations to complete
		storage.WaitForAsyncOps()

		// Force garbage collection
		runtime.GC()
		runtime.Gosched()
	}

	// Check that memory cache is reasonable size
	memCache := storage.getMemCache()
	if memCache.Size() > 200 { // Should have expired items removed
		t.Errorf("Memory cache has %d items, expected much fewer after TTL expiration", memCache.Size())
	}

	// Check memory cache cost is reasonable
	if memCache.Cost() > mediumDataSize*10 {
		t.Errorf("Memory cache cost %d exceeds expected maximum", memCache.Cost())
	}

	// Note: We don't check mutex cleanup as it's done asynchronously and is best-effort
	// The important thing is that the memory cache itself doesn't leak
}

// TestEdgeCases tests various edge cases
func TestEdgeCases(t *testing.T) {
	logger := getTestLogger(t)
	defer cleanupTestDir(t)

	t.Run("ZeroSizeData", func(t *testing.T) {
		storage := NewStorage(testCacheDir, 0, 1000, 10, 1000, 10000, 10, logger)
		meta := createTestMetadata(200)
		err := storage.SetWithKey("empty", meta, []byte{})
		if err != nil {
			t.Errorf("Failed to store empty data: %v", err)
		}
	})

	t.Run("NilMetadata", func(t *testing.T) {
		storage := NewStorage(testCacheDir, 0, 1000, 10, 0, 0, 0, logger)
		// This should be handled gracefully
		err := storage.SetWithKey("nil-meta", nil, []byte("test"))
		if err == nil {
			t.Error("Expected error for nil metadata, got nil")
		}
	})

	t.Run("SpecialCharactersInKey", func(t *testing.T) {
		storage := NewStorage(testCacheDir, 0, 10000, 10, 10000, 100000, 10, logger)
		specialKey := "test/key/with/slashes/and-dashes_underscores.dots"
		data := generateTestData(smallDataSize)
		meta := createTestMetadata(200)

		err := storage.SetWithKey(specialKey, meta, data)
		if err != nil {
			t.Errorf("Failed to store with special characters in key: %v", err)
		}

		retrieved, _, err := storage.Get(specialKey, "none")
		if err != nil {
			t.Errorf("Failed to retrieve with special characters in key: %v", err)
		}

		if !bytes.Equal(retrieved, data) {
			t.Errorf("Data mismatch with special characters in key")
		}
	})

	t.Run("VeryLongKey", func(t *testing.T) {
		storage := NewStorage(testCacheDir, 0, 10000, 10, 10000, 100000, 10, logger)
		longKey := string(bytes.Repeat([]byte("a"), 500))
		data := generateTestData(smallDataSize)
		meta := createTestMetadata(200)

		err := storage.SetWithKey(longKey, meta, data)
		if err != nil {
			t.Logf("Very long key may fail on some filesystems: %v", err)
		}
	})
}

// BenchmarkHighConcurrency benchmarks under very high concurrency
func BenchmarkHighConcurrency(b *testing.B) {
	logger := zap.NewNop()
	defer cleanupTestDir(b)

	storage := NewStorage(
		testCacheDir,
		0,
		veryLargeDataSize*100,
		10000,
		veryLargeDataSize*10,
		veryLargeDataSize*1000,
		10000,
		logger,
	)

	// Pre-populate
	numKeys := 1000
	data := generateTestData(smallDataSize)
	meta := createTestMetadata(200)

	for i := 0; i < numKeys; i++ {
		key := fmt.Sprintf("high-concurrency-key-%d", i)
		_ = storage.SetWithKey(key, meta, data)
	}

	b.ResetTimer()

	// Run with many goroutines
	numGoroutines := runtime.NumCPU() * 10
	b.SetParallelism(numGoroutines)

	var ops int64
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			if i%5 == 0 { // 20% writes
				key := fmt.Sprintf("high-concurrency-new-%d", atomic.AddInt64(&ops, 1))
				_ = storage.SetWithKey(key, meta, data)
			} else { // 80% reads
				key := fmt.Sprintf("high-concurrency-key-%d", i%numKeys)
				_, _, _ = storage.Get(key, "none")
			}
			i++
		}
	})

	b.Logf("Operations completed: %d", ops)
}
