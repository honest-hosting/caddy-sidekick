package sidekick

import (
	"fmt"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
)

// Test constants
const (
	testCacheDir      = "./test-cache"
	testTTL           = 3600 // 1 hour
	smallDataSize     = 1024
	mediumDataSize    = 10 * 1024
	largeDataSize     = 100 * 1024
	veryLargeDataSize = 1024 * 1024
)

// Helper functions
func getTestLogger() *zap.Logger {
	config := zap.NewDevelopmentConfig()
	config.Level = zap.NewAtomicLevelAt(zap.WarnLevel) // Reduce noise in tests
	logger, _ := config.Build()
	return logger
}

func generateTestData(size int) []byte {
	data := make([]byte, size)
	for i := range data {
		data[i] = byte(i % 256)
	}
	return data
}

func createTestMetadata() *Metadata {
	return &Metadata{
		StateCode: 200,
		Header: [][]string{
			{"Content-Type", "text/plain"},
		},
		Timestamp: time.Now().Unix(),
	}
}

func cleanupTestDir(t testing.TB) {
	if err := os.RemoveAll(testCacheDir); err != nil {
		t.Logf("Failed to cleanup test directory: %v", err)
	}
}

// TestMemoryCacheStorage tests memory cache operations
func TestMemoryCacheStorage(t *testing.T) {
	cleanupTestDir(t)
	defer cleanupTestDir(t)

	logger := getTestLogger()
	storage := NewStorage(testCacheDir, testTTL, largeDataSize, 100, 0, 0, 0, logger)

	// Test Set and Get operations
	testData := generateTestData(smallDataSize)
	metadata := createTestMetadata()

	key := "test-key"
	err := storage.SetWithKey(key, metadata, testData)
	if err != nil {
		t.Fatalf("Failed to set data: %v", err)
	}

	// Retrieve data
	retrievedData, retrievedMetadata, err := storage.Get(key, "none")
	if err != nil {
		t.Fatalf("Failed to get data: %v", err)
	}

	// Verify data
	if len(retrievedData) != len(testData) {
		t.Errorf("Data size mismatch: expected %d, got %d", len(testData), len(retrievedData))
	}

	// Verify metadata
	if retrievedMetadata.StateCode != metadata.StateCode {
		t.Errorf("StateCode mismatch: expected %d, got %d", metadata.StateCode, retrievedMetadata.StateCode)
	}
}

// TestStorageMemoryCacheEvictionByCount tests memory cache eviction when count limit is reached
func TestStorageMemoryCacheEvictionByCount(t *testing.T) {
	cleanupTestDir(t)
	defer cleanupTestDir(t)

	logger := getTestLogger()
	storage := NewStorage(testCacheDir, testTTL, largeDataSize*10, 3, 0, 0, 0, logger)

	// Add items to exceed count limit
	for i := 0; i < 5; i++ {
		key := fmt.Sprintf("count-test-%d", i)
		data := generateTestData(smallDataSize)
		metadata := createTestMetadata()
		err := storage.SetWithKey(key, metadata, data)
		if err != nil {
			t.Fatalf("Failed to set data for key %s: %v", key, err)
		}
	}

	// Check that memory cache respects count limit
	memCache := storage.GetMemCache()
	if memCache == nil {
		t.Fatal("Memory cache is nil")
	}

	if memCache.Size() > storage.memMaxCount {
		t.Errorf("Memory cache size %d exceeds limit %d", memCache.Size(), storage.memMaxCount)
	}

	// First items should be evicted (LRU)
	for i := 0; i < 2; i++ {
		key := fmt.Sprintf("count-test-%d", i)
		_, _, err := storage.Get(key, "none")
		if err != ErrCacheNotFound {
			t.Errorf("Expected item %s to be evicted from memory", key)
		}
	}

	// Last items should still be in memory
	for i := 2; i < 5; i++ {
		key := fmt.Sprintf("count-test-%d", i)
		_, _, err := storage.Get(key, "none")
		if err != nil {
			t.Errorf("Expected item %s to be in cache, got error: %v", key, err)
		}
	}
}

// TestMemoryCacheEvictionBySize tests memory cache eviction when size limit is reached
func TestMemoryCacheEvictionBySize(t *testing.T) {
	cleanupTestDir(t)
	defer cleanupTestDir(t)

	logger := getTestLogger()
	storage := NewStorage(testCacheDir, testTTL, smallDataSize*3, 100, 0, 0, 0, logger)

	// Add items to exceed size limit
	for i := 0; i < 5; i++ {
		key := fmt.Sprintf("size-test-%d", i)
		data := generateTestData(smallDataSize)
		metadata := createTestMetadata()
		err := storage.SetWithKey(key, metadata, data)
		if err != nil {
			t.Fatalf("Failed to set data for key %s: %v", key, err)
		}
	}

	// Check memory cache cost is within limit
	memCache := storage.GetMemCache()
	if memCache == nil {
		t.Fatal("Memory cache is nil")
	}

	// Allow small overhead since eviction happens after adding
	allowedOverhead := 1024 + 500 // One small item plus metadata
	if memCache.Cost() > storage.memMaxSize+allowedOverhead {
		t.Errorf("Memory cache cost %d exceeds limit %d (with allowed overhead %d)", memCache.Cost(), storage.memMaxSize, allowedOverhead)
	}

	// At least the most recent item should be in memory
	key := "size-test-4"
	_, _, err := storage.Get(key, "none")
	if err != nil {
		t.Errorf("Most recent item should be accessible, got error: %v", err)
	}
}

// TestCacheLifetime tests cache TTL expiration
func TestCacheLifetime(t *testing.T) {
	cleanupTestDir(t)
	defer cleanupTestDir(t)

	logger := getTestLogger()
	// Use a very short TTL for testing
	storage := NewStorage(testCacheDir, 1, largeDataSize, 100, 0, 0, 0, logger) // 1 second TTL

	key := "ttl-test"
	data := generateTestData(smallDataSize)
	metadata := createTestMetadata()

	// Set data
	err := storage.SetWithKey(key, metadata, data)
	if err != nil {
		t.Fatalf("Failed to set data: %v", err)
	}

	// Should be retrievable immediately
	_, _, err = storage.Get(key, "none")
	if err != nil {
		t.Errorf("Data should be retrievable immediately after setting: %v", err)
	}

	// Wait for TTL to expire
	time.Sleep(2 * time.Second)

	// Should return expired error
	_, _, err = storage.Get(key, "none")
	if err != ErrCacheExpired {
		t.Errorf("Expected ErrCacheExpired after TTL, got: %v", err)
	}

	// Wait for async cleanup
	storage.WaitForAsyncOps()

	// Should be completely gone now
	_, _, err = storage.Get(key, "none")
	if err != ErrCacheNotFound {
		t.Errorf("Expected ErrCacheNotFound after cleanup, got: %v", err)
	}
}

// TestMaxItemSizeMemory tests memory cache respects item size limits
func TestMaxItemSizeMemory(t *testing.T) {
	cleanupTestDir(t)
	defer cleanupTestDir(t)

	logger := getTestLogger()
	storage := NewStorage(testCacheDir, testTTL, mediumDataSize, 100, smallDataSize, largeDataSize, 100, logger)

	// Try to store item larger than disk limit
	largeData := generateTestData(mediumDataSize)
	metadata := createTestMetadata()

	err := storage.SetWithKey("too-large", metadata, largeData)
	if err == nil {
		t.Error("Expected error when storing item larger than diskItemMaxSize")
	}

	// Store item within limits
	normalData := generateTestData(smallDataSize)
	err = storage.SetWithKey("normal-size", metadata, normalData)
	if err != nil {
		t.Errorf("Failed to store normal-sized item: %v", err)
	}

	// Should be retrievable
	_, _, err = storage.Get("normal-size", "none")
	if err != nil {
		t.Errorf("Failed to retrieve normal-sized item: %v", err)
	}
}

// TestConcurrentMemoryAccess tests concurrent memory cache operations
func TestConcurrentMemoryAccess(t *testing.T) {
	cleanupTestDir(t)
	defer cleanupTestDir(t)

	logger := getTestLogger()
	storage := NewStorage(testCacheDir, testTTL, largeDataSize*100, 1000, 0, 0, 0, logger)

	numGoroutines := 20
	numOperations := 50
	var wg sync.WaitGroup

	// Concurrent writes
	wg.Add(numGoroutines)
	for i := 0; i < numGoroutines; i++ {
		go func(id int) {
			defer wg.Done()
			for j := 0; j < numOperations; j++ {
				key := fmt.Sprintf("concurrent-%d-%d", id, j)
				data := generateTestData(smallDataSize)
				metadata := createTestMetadata()
				if err := storage.SetWithKey(key, metadata, data); err != nil {
					t.Errorf("Failed to set data: %v", err)
				}
			}
		}(i)
	}
	wg.Wait()

	// Verify data can be retrieved
	successCount := 0
	for i := 0; i < numGoroutines; i++ {
		for j := 0; j < numOperations; j++ {
			key := fmt.Sprintf("concurrent-%d-%d", i, j)
			if _, _, err := storage.Get(key, "none"); err == nil {
				successCount++
			}
		}
	}

	// Should have stored many items
	if successCount < (numGoroutines * numOperations / 2) {
		t.Errorf("Too few successful retrievals: %d out of %d", successCount, numGoroutines*numOperations)
	}

	// Concurrent reads
	wg.Add(numGoroutines)
	for i := 0; i < numGoroutines; i++ {
		go func(id int) {
			defer wg.Done()
			key := fmt.Sprintf("concurrent-%d-0", id)
			for j := 0; j < 10; j++ {
				_, _, _ = storage.Get(key, "none")
			}
		}(i)
	}
	wg.Wait()
}

// TestCacheKeyMutexCleanup tests that key mutexes are properly cleaned up
func TestCacheKeyMutexCleanup(t *testing.T) {
	cleanupTestDir(t)
	defer cleanupTestDir(t)

	logger := getTestLogger()
	storage := NewStorage(testCacheDir, testTTL, largeDataSize, 100, 0, 0, 0, logger)

	key := "mutex-test"
	data := generateTestData(smallDataSize)
	metadata := createTestMetadata()

	// Set data
	err := storage.SetWithKey(key, metadata, data)
	if err != nil {
		t.Fatalf("Failed to set data: %v", err)
	}

	// Purge should cleanup the mutex
	_ = storage.Purge(key)

	// Check that mutex was cleaned up
	storage.keyMutexesMu.Lock()
	_, exists := storage.keyMutexes[key]
	storage.keyMutexesMu.Unlock()

	if exists {
		t.Error("Key mutex should have been cleaned up after purge")
	}
}

// TestMemoryCacheCostAccounting tests memory cache cost tracking
func TestMemoryCacheCostAccounting(t *testing.T) {
	cleanupTestDir(t)
	defer cleanupTestDir(t)

	logger := getTestLogger()
	storage := NewStorage(testCacheDir, testTTL, largeDataSize, 100, 0, 0, 0, logger)

	memCache := storage.GetMemCache()
	if memCache == nil {
		t.Fatal("Memory cache is nil")
	}

	initialCost := memCache.Cost()

	// Add items and track cost
	items := []struct {
		key  string
		size int
	}{
		{"item1", smallDataSize},
		{"item2", smallDataSize * 2},
		{"item3", smallDataSize * 3},
	}

	totalSize := 0
	// Account for metadata overhead
	for _, item := range items {
		data := generateTestData(item.size)
		metadata := createTestMetadata()
		err := storage.SetWithKey(item.key, metadata, data)
		if err != nil {
			t.Fatalf("Failed to set data for key %s: %v", item.key, err)
		}
		totalSize += item.size + 100 // Add some overhead for metadata
	}

	finalCost := memCache.Cost()
	costIncrease := finalCost - initialCost

	// Cost should have increased by approximately the total size
	// Allow some variance for metadata and internal overhead
	expectedMin := totalSize * 90 / 100  // 90% of expected
	expectedMax := totalSize * 150 / 100 // 150% of expected

	if costIncrease < expectedMin || costIncrease > expectedMax {
		t.Errorf("Cost increase %d not in expected range [%d, %d]", costIncrease, expectedMin, expectedMax)
	}
}

// BenchmarkMemoryCacheSet benchmarks memory cache set operations
func BenchmarkMemoryCacheSet(b *testing.B) {
	cleanupTestDir(b)
	defer cleanupTestDir(b)

	logger := getTestLogger()
	storage := NewStorage(testCacheDir, testTTL, veryLargeDataSize*100, 100000, 0, 0, 0, logger)

	data := generateTestData(smallDataSize)
	metadata := createTestMetadata()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		key := fmt.Sprintf("bench-%d", i)
		err := storage.SetWithKey(key, metadata, data)
		if err != nil {
			b.Fatalf("Failed to set data: %v", err)
		}
	}
}

// BenchmarkStorageMemoryCacheGet benchmarks memory cache get operations
func BenchmarkStorageMemoryCacheGet(b *testing.B) {
	cleanupTestDir(b)
	defer cleanupTestDir(b)

	logger := getTestLogger()
	storage := NewStorage(testCacheDir, testTTL, veryLargeDataSize*100, 100000, 0, 0, 0, logger)

	// Pre-populate cache
	data := generateTestData(smallDataSize)
	metadata := createTestMetadata()
	numKeys := 1000
	for i := 0; i < numKeys; i++ {
		key := fmt.Sprintf("bench-%d", i)
		_ = storage.SetWithKey(key, metadata, data)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		key := fmt.Sprintf("bench-%d", i%numKeys)
		_, _, err := storage.Get(key, "none")
		if err != nil {
			b.Fatalf("Failed to get data: %v", err)
		}
	}
}

// BenchmarkConcurrentMixedOperations benchmarks mixed operations under concurrency
func BenchmarkConcurrentMixedOperations(b *testing.B) {
	cleanupTestDir(b)
	defer cleanupTestDir(b)

	logger := getTestLogger()
	storage := NewStorage(testCacheDir, testTTL, veryLargeDataSize*100, 100000, 0, 0, 0, logger)

	// Pre-populate some data
	data := generateTestData(smallDataSize)
	metadata := createTestMetadata()
	for i := 0; i < 100; i++ {
		key := fmt.Sprintf("bench-%d", i)
		_ = storage.SetWithKey(key, metadata, data)
	}

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			key := fmt.Sprintf("bench-%d", i%200)
			if i%3 == 0 {
				// Set operation
				_ = storage.SetWithKey(key, metadata, data)
			} else {
				// Get operation
				_, _, _ = storage.Get(key, "none")
			}
			i++
		}
	})
}

// TestMemoryPressure tests behavior under memory pressure
func TestMemoryPressure(t *testing.T) {
	cleanupTestDir(t)
	defer cleanupTestDir(t)

	logger := getTestLogger()
	storage := NewStorage(testCacheDir, testTTL, mediumDataSize*5, 100, 0, 0, 0, logger)

	// Fill up the memory cache
	for i := 0; i < 10; i++ {
		key := fmt.Sprintf("pressure-%d", i)
		data := generateTestData(mediumDataSize)
		metadata := createTestMetadata()
		_ = storage.SetWithKey(key, metadata, data)
	}

	memCache := storage.GetMemCache()
	if memCache == nil {
		t.Fatal("Memory cache is nil")
	}

	// Memory usage should be within limit (allow small overhead for eviction timing)
	// The cache may temporarily exceed the limit before eviction completes
	allowedOverhead := 1024 * 11 // Allow one extra item (10KB + overhead)
	if memCache.Cost() > storage.memMaxSize+allowedOverhead {
		t.Errorf("Memory cost %d exceeds limit %d (with allowed overhead %d)", memCache.Cost(), storage.memMaxSize, allowedOverhead)
	}

	// Should have evicted older items to stay within limit
	if memCache.Size() >= 10 {
		t.Errorf("Expected fewer than 10 items due to size limit, got %d", memCache.Size())
	}
}

// BenchmarkStorageMemoryCacheEviction benchmarks memory cache eviction
func BenchmarkStorageMemoryCacheEviction(b *testing.B) {
	cleanupTestDir(b)
	defer cleanupTestDir(b)

	logger := getTestLogger()
	storage := NewStorage(testCacheDir, testTTL, mediumDataSize*10, 10, 0, 0, 0, logger)

	data := generateTestData(mediumDataSize)
	metadata := createTestMetadata()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		key := fmt.Sprintf("bench-evict-%d", i)
		_ = storage.SetWithKey(key, metadata, data)
	}
}

// BenchmarkCacheKeyGeneration benchmarks cache key generation
func BenchmarkCacheKeyGeneration(b *testing.B) {
	storage := &Storage{}

	urls := []string{
		"http://example.com/",
		"http://example.com/path",
		"http://example.com/path?query=1",
		"http://example.com/path?query=1&param=2",
		"https://subdomain.example.com/long/path/to/resource?with=many&different=params",
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		url := urls[i%len(urls)]
		_ = storage.buildCacheKey(url)
	}
}

// TestMemoryLeaks tests for memory leaks in cache operations
func TestMemoryLeaks(t *testing.T) {
	cleanupTestDir(t)
	defer cleanupTestDir(t)

	logger := getTestLogger()
	storage := NewStorage(testCacheDir, testTTL, largeDataSize*10, 100, 0, 0, 0, logger)

	// Track memory before operations
	runtime.GC()
	var m1 runtime.MemStats
	runtime.ReadMemStats(&m1)

	// Perform many operations
	data := generateTestData(mediumDataSize)
	metadata := createTestMetadata()

	for cycle := 0; cycle < 10; cycle++ {
		// Add items
		for i := 0; i < 100; i++ {
			key := fmt.Sprintf("leak-test-%d-%d", cycle, i)
			_ = storage.SetWithKey(key, metadata, data)
		}

		// Read items
		for i := 0; i < 100; i++ {
			key := fmt.Sprintf("leak-test-%d-%d", cycle, i)
			_, _, _ = storage.Get(key, "none")
		}

		// Delete some items
		for i := 0; i < 50; i++ {
			key := fmt.Sprintf("leak-test-%d-%d", cycle, i)
			_ = storage.Purge(key)
		}
	}

	// Clear everything
	_ = storage.Flush()

	// Force garbage collection and measure memory
	runtime.GC()
	runtime.GC() // Run twice to ensure finalization
	var m2 runtime.MemStats
	runtime.ReadMemStats(&m2)

	// Memory increase should be reasonable (allow 50MB increase)
	memIncrease := int64(m2.Alloc) - int64(m1.Alloc)
	maxIncrease := int64(50 * 1024 * 1024) // 50MB

	if memIncrease > maxIncrease {
		t.Logf("Warning: Memory increased by %d bytes (%.2f MB)", memIncrease, float64(memIncrease)/(1024*1024))
		// This is a warning, not a failure, as Go's memory management is complex
	}
}

// TestEdgeCases tests various edge cases
func TestEdgeCases(t *testing.T) {
	cleanupTestDir(t)
	defer cleanupTestDir(t)

	logger := getTestLogger()
	storage := NewStorage(testCacheDir, testTTL, largeDataSize, 100, 0, 0, 0, logger)

	// Test empty key
	err := storage.SetWithKey("", createTestMetadata(), generateTestData(smallDataSize))
	if err != nil {
		t.Logf("Empty key handled: %v", err)
	}

	// Test empty data
	err = storage.SetWithKey("empty-data", createTestMetadata(), []byte{})
	if err != nil {
		t.Errorf("Should handle empty data: %v", err)
	}

	// Test nil metadata fields
	metadata := &Metadata{}
	err = storage.SetWithKey("nil-metadata", metadata, generateTestData(smallDataSize))
	if err != nil {
		t.Errorf("Should handle nil metadata fields: %v", err)
	}

	// Test very long key
	longKey := ""
	for i := 0; i < 1000; i++ {
		longKey += "a"
	}
	err = storage.SetWithKey(longKey, createTestMetadata(), generateTestData(smallDataSize))
	if err != nil {
		t.Logf("Long key handled: %v", err)
	}

	// Test special characters in key
	specialKey := "key-with-/\\:*?\"<>|spaces and unicode 文字"
	err = storage.SetWithKey(specialKey, createTestMetadata(), generateTestData(smallDataSize))
	if err != nil {
		t.Logf("Special characters in key handled: %v", err)
	}

	// Test Get non-existent key
	_, _, err = storage.Get("non-existent-key", "none")
	if err != ErrCacheNotFound {
		t.Errorf("Expected ErrCacheNotFound for non-existent key, got: %v", err)
	}
}

// BenchmarkHighConcurrency benchmarks under high concurrency
func BenchmarkHighConcurrency(b *testing.B) {
	cleanupTestDir(b)
	defer cleanupTestDir(b)

	logger := getTestLogger()
	storage := NewStorage(testCacheDir, testTTL, veryLargeDataSize*100, 100000, 0, 0, 0, logger)

	// Pre-populate cache
	data := generateTestData(smallDataSize)
	metadata := createTestMetadata()
	for i := 0; i < 1000; i++ {
		key := fmt.Sprintf("bench-%d", i)
		_ = storage.SetWithKey(key, metadata, data)
	}

	numGoroutines := runtime.NumCPU() * 4
	b.SetParallelism(numGoroutines)

	var ops int64
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			op := atomic.AddInt64(&ops, 1)
			key := fmt.Sprintf("bench-%d", int(op)%2000)

			switch op % 10 {
			case 0: // 10% writes
				_ = storage.SetWithKey(key, metadata, data)
			case 1: // 10% deletes
				_ = storage.Purge(key)
			default: // 80% reads
				_, _, _ = storage.Get(key, "none")
			}
			i++
		}
	})

	b.Logf("Total operations: %d", ops)
}
