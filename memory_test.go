package sidekick

import (
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestNewMemoryCache tests memory cache initialization
func TestNewMemoryCache(t *testing.T) {
	capacityCount := 100
	capacityCost := 10000

	cache := NewMemoryCache[string, string](capacityCount, capacityCost)

	if cache == nil {
		t.Fatal("NewMemoryCache returned nil")
	}

	if cache.capacityCount != capacityCount {
		t.Errorf("Expected capacity count %d, got %d", capacityCount, cache.capacityCount)
	}

	if cache.capacityCost != int64(capacityCost) {
		t.Errorf("Expected capacity cost %d, got %d", capacityCost, cache.capacityCost)
	}

	if cache.Size() != 0 {
		t.Errorf("Expected initial size 0, got %d", cache.Size())
	}

	if cache.Cost() != 0 {
		t.Errorf("Expected initial cost 0, got %d", cache.Cost())
	}
}

// TestMemoryCachePutAndGet tests basic put and get operations
func TestMemoryCachePutAndGet(t *testing.T) {
	cache := NewMemoryCache[string, string](10, 1000)

	// Test Put
	key := "test-key"
	value := "test-value"
	cost := 10

	existed := cache.Put(key, value, cost)
	if existed {
		t.Error("Put should return false for new key")
	}

	// Test Get
	retrieved, ok := cache.Get(key)
	if !ok {
		t.Error("Get should return true for existing key")
	}

	if retrieved == nil || *retrieved != value {
		t.Errorf("Expected value %s, got %v", value, retrieved)
	}

	// Test updating existing key
	newValue := "new-value"
	existed = cache.Put(key, newValue, cost)
	if !existed {
		t.Error("Put should return true when updating existing key")
	}

	retrieved, ok = cache.Get(key)
	if !ok || retrieved == nil || *retrieved != newValue {
		t.Errorf("Expected updated value %s, got %v", newValue, retrieved)
	}
}

// TestMemoryCachePeek tests peek operation (doesn't update LRU)
func TestMemoryCachePeek(t *testing.T) {
	cache := NewMemoryCache[string, int](3, 1000)

	// Add items
	cache.Put("first", 1, 10)
	cache.Put("second", 2, 10)
	cache.Put("third", 3, 10)

	// Peek at first (shouldn't update LRU order)
	val, ok := cache.Peek("first")
	if !ok || val == nil || *val != 1 {
		t.Errorf("Peek failed for 'first'")
	}

	// Add fourth item - should evict "first" since it wasn't accessed with Get
	cache.Put("fourth", 4, 10)

	// First should be evicted (wasn't touched with Get)
	_, ok = cache.Get("first")
	if ok {
		t.Error("'first' should have been evicted as LRU")
	}

	// Now test with Get to compare
	cache = NewMemoryCache[string, int](3, 1000)
	cache.Put("first", 1, 10)
	cache.Put("second", 2, 10)
	cache.Put("third", 3, 10)

	// Get first (should update LRU order)
	val, ok = cache.Get("first")
	if !ok || val == nil || *val != 1 {
		t.Errorf("Get failed for 'first'")
	}

	// Add fourth item - should evict "second" since "first" was recently accessed
	cache.Put("fourth", 4, 10)

	// First should still exist
	_, ok = cache.Get("first")
	if !ok {
		t.Error("'first' should not have been evicted after Get")
	}

	// Second should be evicted
	_, ok = cache.Get("second")
	if ok {
		t.Error("'second' should have been evicted as LRU")
	}
}

// TestMemoryCacheDelete tests delete operation
func TestMemoryCacheDelete(t *testing.T) {
	cache := NewMemoryCache[string, string](10, 1000)

	key := "delete-key"
	value := "delete-value"
	cost := 20

	cache.Put(key, value, cost)

	// Verify it exists
	_, ok := cache.Get(key)
	if !ok {
		t.Error("Key should exist before delete")
	}

	// Delete
	cache.Delete(key)

	// Verify it's gone
	_, ok = cache.Get(key)
	if ok {
		t.Error("Key should not exist after delete")
	}

	// Verify cost was updated
	if cache.Cost() != 0 {
		t.Errorf("Cost should be 0 after deleting only item, got %d", cache.Cost())
	}
}

// TestMemoryCacheEvictionByCount tests eviction when count limit is reached
func TestMemoryCacheEvictionByCount(t *testing.T) {
	maxCount := 5
	cache := NewMemoryCache[string, int](maxCount, -1) // No cost limit

	// Add more items than capacity
	for i := 0; i < maxCount+2; i++ {
		key := fmt.Sprintf("key-%d", i)
		cache.Put(key, i, 10)
	}

	// Size should not exceed maxCount
	if cache.Size() > maxCount {
		t.Errorf("Cache size %d exceeds max count %d", cache.Size(), maxCount)
	}

	// First items should be evicted (LRU)
	for i := 0; i < 2; i++ {
		key := fmt.Sprintf("key-%d", i)
		_, ok := cache.Get(key)
		if ok {
			t.Errorf("Key %s should have been evicted", key)
		}
	}

	// Last items should still be present
	for i := 2; i < maxCount+2; i++ {
		key := fmt.Sprintf("key-%d", i)
		val, ok := cache.Get(key)
		if !ok || val == nil || *val != i {
			t.Errorf("Key %s should be present with value %d", key, i)
		}
	}
}

// TestMemoryCacheEvictionByCost tests eviction when cost limit is reached
func TestMemoryCacheEvictionByCost(t *testing.T) {
	maxCost := 100
	cache := NewMemoryCache[string, string](-1, maxCost) // No count limit

	// Add items with different costs
	items := []struct {
		key   string
		value string
		cost  int
	}{
		{"item1", "value1", 30},
		{"item2", "value2", 40},
		{"item3", "value3", 20},
		{"item4", "value4", 25},
		{"item5", "value5", 15},
	}

	for _, item := range items {
		cache.Put(item.key, item.value, item.cost)
	}

	// Total cost should not exceed maxCost
	if cache.Cost() > maxCost {
		t.Errorf("Cache cost %d exceeds max cost %d", cache.Cost(), maxCost)
	}

	// At least the most recent items should be present
	val, ok := cache.Get("item5")
	if !ok || val == nil {
		t.Error("Most recent item should be present")
	}
}

// TestMemoryCacheEvictionMixed tests eviction with both count and cost limits
func TestMemoryCacheEvictionMixed(t *testing.T) {
	maxCount := 5
	maxCost := 100
	cache := NewMemoryCache[string, int](maxCount, maxCost)

	// Add items that will trigger count-based eviction
	for i := 0; i < maxCount; i++ {
		key := fmt.Sprintf("small-%d", i)
		cache.Put(key, i, 10) // Total cost: 50
	}

	if cache.Size() != maxCount {
		t.Errorf("Expected size %d, got %d", maxCount, cache.Size())
	}

	// Add one more small item - should evict by count
	cache.Put("small-extra", 99, 10)
	if cache.Size() > maxCount {
		t.Errorf("Size %d exceeds max count %d", cache.Size(), maxCount)
	}

	// Now add a large item that triggers cost-based eviction
	cache.Put("large", 100, 60)

	if cache.Cost() > maxCost {
		t.Errorf("Cost %d exceeds max cost %d", cache.Cost(), maxCost)
	}

	// The large item should be present
	val, ok := cache.Get("large")
	if !ok || val == nil || *val != 100 {
		t.Error("Large item should be present after cost-based eviction")
	}
}

// TestMemoryCacheLoadOrCompute tests the LoadOrCompute method
func TestMemoryCacheLoadOrCompute(t *testing.T) {
	cache := NewMemoryCache[string, string](10, 1000)

	computeCount := 0
	computeFn := func() (string, int, bool) {
		computeCount++
		return fmt.Sprintf("computed-%d", computeCount), 20, true
	}

	// First call should compute
	val, loaded := cache.LoadOrCompute("key1", computeFn)
	if loaded {
		t.Error("First call should not be loaded")
	}
	if val != "computed-1" {
		t.Errorf("Expected 'computed-1', got %s", val)
	}
	if computeCount != 1 {
		t.Errorf("Compute should have been called once, got %d", computeCount)
	}

	// Second call should load from cache
	val, loaded = cache.LoadOrCompute("key1", computeFn)
	if !loaded {
		t.Error("Second call should be loaded from cache")
	}
	if val != "computed-1" {
		t.Errorf("Expected cached 'computed-1', got %s", val)
	}
	if computeCount != 1 {
		t.Errorf("Compute should still be 1, got %d", computeCount)
	}

	// Test with compute function returning false
	cache.LoadOrCompute("key2", func() (string, int, bool) {
		return "", 0, false
	})

	// Key2 should not be in cache
	_, ok := cache.Get("key2")
	if ok {
		t.Error("key2 should not be in cache when compute returns false")
	}
}

// TestMemoryCacheRange tests the Range method
func TestMemoryCacheRange(t *testing.T) {
	cache := NewMemoryCache[string, int](10, 1000)

	// Add items
	expected := map[string]int{
		"key1": 10,
		"key2": 20,
		"key3": 30,
	}

	for k, v := range expected {
		cache.Put(k, v, 10)
	}

	// Range over all items
	visited := make(map[string]int)
	cache.Range(func(key string, value int) bool {
		visited[key] = value
		return true
	})

	if len(visited) != len(expected) {
		t.Errorf("Expected %d items, visited %d", len(expected), len(visited))
	}

	for k, v := range expected {
		if visited[k] != v {
			t.Errorf("Expected %s=%d, got %d", k, v, visited[k])
		}
	}

	// Test early termination
	count := 0
	cache.Range(func(key string, value int) bool {
		count++
		return count < 2 // Stop after 2 iterations
	})

	if count != 2 {
		t.Errorf("Expected Range to stop at 2, got %d", count)
	}
}

// TestMemoryCacheConcurrency tests concurrent operations
func TestMemoryCacheConcurrency(t *testing.T) {
	cache := NewMemoryCache[string, int](1000, 100000)

	numGoroutines := 50
	numOperations := 100

	var wg sync.WaitGroup
	wg.Add(numGoroutines)

	for i := 0; i < numGoroutines; i++ {
		go func(id int) {
			defer wg.Done()

			for j := 0; j < numOperations; j++ {
				key := fmt.Sprintf("key-%d-%d", id, j)

				// Mix of operations
				switch j % 4 {
				case 0:
					cache.Put(key, id*1000+j, 10)
				case 1:
					val, ok := cache.Get(key)
					if ok && val != nil && *val != id*1000+j {
						t.Errorf("Unexpected value for %s", key)
					}
				case 2:
					cache.Peek(key)
				case 3:
					cache.Delete(fmt.Sprintf("key-%d-%d", id, j-1))
				}
			}
		}(i)
	}

	wg.Wait()

	// Cache should still be in valid state
	if cache.Size() < 0 {
		t.Error("Cache size is negative")
	}

	if cache.Cost() < 0 {
		t.Error("Cache cost is negative")
	}
}

// TestMemoryCacheConcurrentLoadOrCompute tests concurrent LoadOrCompute calls
func TestMemoryCacheConcurrentLoadOrCompute(t *testing.T) {
	cache := NewMemoryCache[string, string](100, 10000)

	key := "shared-key"
	computeCount := int64(0)

	numGoroutines := 20
	var wg sync.WaitGroup
	wg.Add(numGoroutines)

	for i := 0; i < numGoroutines; i++ {
		go func() {
			defer wg.Done()

			val, _ := cache.LoadOrCompute(key, func() (string, int, bool) {
				atomic.AddInt64(&computeCount, 1)
				time.Sleep(10 * time.Millisecond) // Simulate expensive computation
				return "computed-value", 20, true
			})

			if val != "computed-value" {
				t.Errorf("Expected 'computed-value', got %s", val)
			}
		}()
	}

	wg.Wait()

	// Compute function should only be called once despite concurrent access
	if computeCount != 1 {
		t.Errorf("Expected compute to be called once, got %d", computeCount)
	}
}

// TestMemoryCacheUpdateCost tests cost update when replacing values
func TestMemoryCacheUpdateCost(t *testing.T) {
	cache := NewMemoryCache[string, string](10, 1000)

	key := "test-key"

	// Initial put
	cache.Put(key, "value1", 50)
	if cache.Cost() != 50 {
		t.Errorf("Expected cost 50, got %d", cache.Cost())
	}

	// Update with different cost
	cache.Put(key, "value2", 75)
	if cache.Cost() != 75 {
		t.Errorf("Expected cost 75 after update, got %d", cache.Cost())
	}

	// Update with smaller cost
	cache.Put(key, "value3", 25)
	if cache.Cost() != 25 {
		t.Errorf("Expected cost 25 after update, got %d", cache.Cost())
	}
}

// TestMemoryCacheZeroCapacity tests cache with zero capacity
func TestMemoryCacheZeroCapacity(t *testing.T) {
	// Zero count capacity (unlimited)
	cache := NewMemoryCache[string, int](0, 100)

	for i := 0; i < 100; i++ {
		cache.Put(fmt.Sprintf("key-%d", i), i, 1)
	}

	// Should be limited by cost, not count
	if cache.Cost() > 100 {
		t.Errorf("Cost %d exceeds limit 100", cache.Cost())
	}

	// Zero cost capacity (unlimited)
	cache2 := NewMemoryCache[string, int](10, 0)

	for i := 0; i < 20; i++ {
		cache2.Put(fmt.Sprintf("key-%d", i), i, 100)
	}

	// Should be limited by count, not cost
	if cache2.Size() > 10 {
		t.Errorf("Size %d exceeds limit 10", cache2.Size())
	}
}

// TestMemoryCacheNilValue tests handling of nil values
func TestMemoryCacheNilValue(t *testing.T) {
	cache := NewMemoryCache[string, *string](10, 1000)

	key := "nil-key"
	var nilValue *string = nil

	cache.Put(key, nilValue, 10)

	val, ok := cache.Get(key)
	if !ok {
		t.Error("Key should exist even with nil value")
	}

	if val == nil || *val != nil {
		t.Error("Expected nil value to be preserved")
	}
}

// BenchmarkMemoryCachePut benchmarks Put operation
func BenchmarkMemoryCachePut(b *testing.B) {
	cache := NewMemoryCache[string, int](10000, 1000000)

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			key := fmt.Sprintf("bench-key-%d", i)
			cache.Put(key, i, 10)
			i++
		}
	})
}

// BenchmarkMemoryCacheGet benchmarks Get operation
func BenchmarkMemoryCacheGet(b *testing.B) {
	cache := NewMemoryCache[string, int](10000, 1000000)

	// Pre-populate
	for i := 0; i < 1000; i++ {
		cache.Put(fmt.Sprintf("key-%d", i), i, 10)
	}

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			key := fmt.Sprintf("key-%d", i%1000)
			cache.Get(key)
			i++
		}
	})
}

// BenchmarkMemoryCacheLoadOrCompute benchmarks LoadOrCompute operation
func BenchmarkMemoryCacheLoadOrCompute(b *testing.B) {
	cache := NewMemoryCache[string, string](10000, 1000000)

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			key := fmt.Sprintf("key-%d", i%1000) // Reuse some keys
			cache.LoadOrCompute(key, func() (string, int, bool) {
				return fmt.Sprintf("value-%d", i), 10, true
			})
			i++
		}
	})
}

// BenchmarkMemoryCacheEviction benchmarks eviction performance
func BenchmarkMemoryCacheEviction(b *testing.B) {
	// Small cache to trigger frequent evictions
	cache := NewMemoryCache[string, int](100, 1000)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		key := fmt.Sprintf("key-%d", i)
		cache.Put(key, i, 10)
	}
}

// BenchmarkMemoryCacheRange benchmarks Range operation
func BenchmarkMemoryCacheRange(b *testing.B) {
	cache := NewMemoryCache[string, int](1000, 100000)

	// Pre-populate
	for i := 0; i < 1000; i++ {
		cache.Put(fmt.Sprintf("key-%d", i), i, 10)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sum := 0
		cache.Range(func(key string, value int) bool {
			sum += value
			return true
		})
		_ = sum
	}
}

// BenchmarkMemoryCacheHighConcurrency benchmarks under high concurrency
func BenchmarkMemoryCacheHighConcurrency(b *testing.B) {
	cache := NewMemoryCache[string, int](10000, 1000000)

	// Pre-populate
	for i := 0; i < 1000; i++ {
		cache.Put(fmt.Sprintf("key-%d", i), i, 10)
	}

	b.ResetTimer()

	numGoroutines := runtime.NumCPU() * 4
	b.SetParallelism(numGoroutines)

	var ops int64
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			op := atomic.AddInt64(&ops, 1)
			switch op % 10 {
			case 0: // 10% puts
				cache.Put(fmt.Sprintf("new-key-%d", op), int(op), 10)
			case 1: // 10% deletes
				cache.Delete(fmt.Sprintf("key-%d", i%1000))
			default: // 80% gets
				cache.Get(fmt.Sprintf("key-%d", i%1000))
			}
			i++
		}
	})

	b.Logf("Total operations: %d", ops)
}
