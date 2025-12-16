package sidekick

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
)

// Package-level errors
var (
	ErrCacheExpired  = errors.New("cache expired")
	ErrCacheNotFound = errors.New("cache not found")
)

var CachedContentEncoding = []string{
	"none",
	"gzip",
	"br",
	"zstd",
}

// Storage manages both memory and disk cache storage
type Storage struct {
	loc    string
	ttl    int
	logger *zap.Logger

	memMaxSize  int
	memMaxCount int
	memCache    atomic.Value // *MemoryCache[string, *MemoryCacheItem]

	// Disk cache limits
	diskItemMaxSize int
	diskMaxSize     int
	diskMaxCount    int
	diskUsage       int64 // Current disk usage in bytes
	diskItemCount   int64 // Current number of items on disk

	// Disk cache index for fast eviction
	diskIndex   map[string]*DiskCacheEntry
	diskIndexMu sync.RWMutex
	diskLRU     []*DiskCacheEntry // Sorted by access time for LRU eviction

	// Mutex for file operations
	fileMu sync.RWMutex
	// Per-key mutexes for granular locking
	keyMutexes   map[string]*sync.RWMutex
	keyMutexesMu sync.Mutex
	// Mutex for disk usage tracking
	diskUsageMu sync.RWMutex
	// WaitGroup for tracking async operations
	asyncOps sync.WaitGroup
}

type MemoryCacheItem struct {
	*Metadata
	value bytes.Buffer
}

// CACHE_DIR is the subdirectory name for cache storage
const CACHE_DIR = "cache"

// NewStorage creates a new Storage instance
func NewStorage(loc string, ttl int, memMaxSize int, memMaxCount int, diskItemMaxSize int, diskMaxSize int, diskMaxCount int, logger *zap.Logger) *Storage {
	s := &Storage{
		loc:             loc,
		ttl:             ttl,
		logger:          logger,
		memMaxSize:      memMaxSize,
		memMaxCount:     memMaxCount,
		diskItemMaxSize: diskItemMaxSize,
		diskMaxSize:     diskMaxSize,
		diskMaxCount:    diskMaxCount,
		keyMutexes:      make(map[string]*sync.RWMutex),
		diskIndex:       make(map[string]*DiskCacheEntry),
		diskLRU:         make([]*DiskCacheEntry, 0),
	}

	// Initialize memory cache
	memCache := NewMemoryCache[string, *MemoryCacheItem](s.memMaxCount, s.memMaxSize)
	s.memCache.Store(memCache)

	// Create cache directory if it doesn't exist
	cacheDir := path.Join(loc, CACHE_DIR)
	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		if logger != nil {
			logger.Error("Failed to create cache directory", zap.String("path", cacheDir), zap.Error(err))
		}
	}

	// Load disk cache index asynchronously
	s.asyncOps.Add(1)
	go func() {
		defer s.asyncOps.Done()
		startTime := time.Now()
		s.loadDiskCacheIndex()
		if logger != nil {
			s.diskUsageMu.RLock()
			items := s.diskItemCount
			usage := s.diskUsage
			s.diskUsageMu.RUnlock()
			logger.Info("Disk cache index loaded",
				zap.Duration("load_time", time.Since(startTime)),
				zap.Int64("items", items),
				zap.String("usage", s.humanizeSize(usage)))
		}
	}()

	return s
}

func (s *Storage) getMemCache() *MemoryCache[string, *MemoryCacheItem] {
	cache := s.memCache.Load()
	if cache == nil {
		return nil
	}
	return cache.(*MemoryCache[string, *MemoryCacheItem])
}

// getKeyMutex returns a mutex for a specific key to ensure per-key locking
func (s *Storage) getKeyMutex(key string) *sync.RWMutex {
	s.keyMutexesMu.Lock()
	defer s.keyMutexesMu.Unlock()

	if mu, exists := s.keyMutexes[key]; exists {
		return mu
	}

	mu := &sync.RWMutex{}
	s.keyMutexes[key] = mu
	return mu
}

// cleanupKeyMutex removes a key-specific mutex when no longer needed
func (s *Storage) cleanupKeyMutex(key string) {
	s.keyMutexesMu.Lock()
	defer s.keyMutexesMu.Unlock()
	delete(s.keyMutexes, key)
}

func (s *Storage) Get(key string, ce string) ([]byte, *Metadata, error) {
	// Get key-specific lock for reading
	keyMu := s.getKeyMutex(key)
	keyMu.RLock()
	defer keyMu.RUnlock()

	// Check memory cache first
	memCache := s.getMemCache()
	if memCache != nil {
		if cacheItem, ok := memCache.Get(key); ok && cacheItem != nil && (*cacheItem).Metadata != nil {
			// Check TTL
			if s.ttl > 0 && (*cacheItem).Timestamp > 0 {
				if time.Now().Unix() > (*cacheItem).Timestamp+int64(s.ttl) {
					// Cache expired, clean it up asynchronously
					s.asyncOps.Add(1)
					go func() {
						defer s.asyncOps.Done()
						_ = s.Purge(key)
					}()
					return nil, nil, ErrCacheExpired
				}
			}

			if s.logger != nil {
				s.logger.Debug("Cache hit (memory)",
					zap.String("key", key),
					zap.Int("data_size", (*cacheItem).value.Len()))
			}
			return (*cacheItem).value.Bytes(), (*cacheItem).Metadata, nil
		}
	}

	// Check disk cache
	cacheDir := path.Join(s.loc, CACHE_DIR, key)
	if info, err := os.Stat(cacheDir); err == nil && info.IsDir() {
		data, md, err := s.readDiskCache(key, cacheDir)
		if err == nil {
			if s.logger != nil {
				s.logger.Debug("Cache hit (disk)",
					zap.String("key", key),
					zap.Int("data_size", len(data)))
			}
			return data, md, nil
		}
		if err == ErrCacheExpired {
			return nil, nil, err
		}
		// Log other errors but continue
		if s.logger != nil {
			s.logger.Warn("Failed to read from disk cache",
				zap.String("key", key),
				zap.Error(err))
		}
	}

	return nil, nil, ErrCacheNotFound
}

func (s *Storage) Set(url string, metadata *Metadata, data []byte) error {
	key := s.buildCacheKey(url)
	return s.SetWithKey(key, metadata, data)
}

// SetWithKey stores data with the provided key
func (s *Storage) SetWithKey(key string, metadata *Metadata, data []byte) error {
	// Get key-specific lock for writing
	keyMu := s.getKeyMutex(key)
	keyMu.Lock()
	defer keyMu.Unlock()

	dataSize := len(data)

	// Check if data exceeds disk item max size
	if s.diskItemMaxSize > 0 && dataSize > s.diskItemMaxSize {
		if s.logger != nil {
			s.logger.Warn("Data too large for storage",
				zap.String("key", key),
				zap.Int("size", dataSize),
				zap.Int("max_size", s.diskItemMaxSize))
		}
		return fmt.Errorf("data size %d exceeds maximum item size %d", dataSize, s.diskItemMaxSize)
	}

	// Update metadata timestamps
	now := time.Now()
	if metadata.Timestamp == 0 {
		metadata.Timestamp = now.Unix()
	}

	// Try to store in memory first if it fits and memory cache is enabled
	if s.memMaxSize > 0 && dataSize <= s.memMaxSize {
		if err := s.storeInMemory(key, data, metadata); err == nil {
			if s.logger != nil {
				s.logger.Debug("Stored in memory cache",
					zap.String("key", key),
					zap.Int("size", dataSize))
			}
			return nil
		}
	}

	// Store on disk
	cacheDir := path.Join(s.loc, CACHE_DIR, key)
	dataFilePath := path.Join(cacheDir, "data")

	// Check if we need to evict items to make space
	s.diskUsageMu.RLock()
	currentUsage := s.diskUsage
	currentCount := s.diskItemCount
	s.diskUsageMu.RUnlock()

	// Calculate the size we need (estimate with some overhead for metadata)
	estimatedSize := int64(dataSize + 1024) // Add 1KB for metadata

	// Check if we need to evict for size
	if s.diskMaxSize > 0 && currentUsage+estimatedSize > int64(s.diskMaxSize) {
		s.evictOldestFromDisk(estimatedSize, "size")
	}

	// Check if we need to evict for count
	if s.diskMaxCount > 0 && currentCount >= int64(s.diskMaxCount) {
		s.evictOldestFromDisk(0, "count")
	}

	// Store the data
	if err := s.storeData(cacheDir, dataFilePath, data, metadata); err != nil {
		if s.logger != nil {
			s.logger.Error("Failed to store data on disk",
				zap.String("key", key),
				zap.Error(err))
		}
		return err
	}

	// Update disk index
	var actualSize int64
	filepath.Walk(cacheDir, func(path string, info os.FileInfo, err error) error { //nolint:errcheck
		if err == nil && !info.IsDir() {
			actualSize += info.Size()
		}
		return nil
	})
	s.updateDiskIndex(key, actualSize, true)

	if s.logger != nil {
		s.logger.Debug("Stored on disk",
			zap.String("key", key),
			zap.String("size", s.humanizeSize(actualSize)))
	}

	return nil
}

func (s *Storage) storeInMemory(key string, data []byte, metadata *Metadata) error {
	memCache := s.getMemCache()
	if memCache == nil {
		return fmt.Errorf("memory cache not initialized")
	}

	// Store in memory cache
	item := &MemoryCacheItem{
		Metadata: metadata,
		value:    *bytes.NewBuffer(data),
	}
	// Account for both data and metadata in cost (estimate metadata as ~500 bytes)
	totalCost := len(data) + 500
	memCache.Put(key, item, totalCost)

	return nil
}

func (s *Storage) Purge(key string) error {
	// Get key-specific lock for writing
	keyMu := s.getKeyMutex(key)
	keyMu.Lock()
	defer keyMu.Unlock()
	defer s.cleanupKeyMutex(key)

	// Remove from memory cache
	memCache := s.getMemCache()
	if memCache != nil {
		memCache.Delete(key)
	}

	// Remove from disk
	cacheDir := path.Join(s.loc, CACHE_DIR, key)
	s.fileMu.Lock()
	err := os.RemoveAll(cacheDir)
	s.fileMu.Unlock()

	if err != nil && !os.IsNotExist(err) {
		if s.logger != nil {
			s.logger.Error("Failed to purge cache",
				zap.String("key", key),
				zap.Error(err))
		}
		return err
	}

	// Update disk index
	s.removeDiskEntry(key)

	if s.logger != nil {
		s.logger.Debug("Purged cache entry",
			zap.String("key", key))
	}

	return nil
}

func (s *Storage) Flush() error {
	// Clear memory cache
	memCache := NewMemoryCache[string, *MemoryCacheItem](s.memMaxCount, s.memMaxSize)
	s.memCache.Store(memCache)

	// Clear disk cache
	cacheDir := path.Join(s.loc, CACHE_DIR)
	s.fileMu.Lock()
	err := os.RemoveAll(cacheDir)
	if err == nil {
		// Recreate the cache directory
		err = os.MkdirAll(cacheDir, 0755)
	}
	s.fileMu.Unlock()

	// Clear disk index
	s.diskIndexMu.Lock()
	s.diskIndex = make(map[string]*DiskCacheEntry)
	s.diskLRU = make([]*DiskCacheEntry, 0)
	s.diskIndexMu.Unlock()

	// Reset usage stats
	s.diskUsageMu.Lock()
	s.diskUsage = 0
	s.diskItemCount = 0
	s.diskUsageMu.Unlock()

	if s.logger != nil {
		if err != nil {
			s.logger.Error("Failed to flush cache", zap.Error(err))
		} else {
			s.logger.Info("Cache flushed")
		}
	}

	return err
}

func (s *Storage) List() map[string][]string {
	result := make(map[string][]string)
	memKeys := make([]string, 0)
	diskKeys := make([]string, 0)

	// Get keys from memory cache
	memCache := s.getMemCache()
	if memCache != nil {
		memCache.Range(func(key string, _ *MemoryCacheItem) bool {
			memKeys = append(memKeys, key)
			return true
		})
	}

	// Get keys from disk cache using the index
	s.diskIndexMu.RLock()
	for key := range s.diskIndex {
		diskKeys = append(diskKeys, key)
	}
	s.diskIndexMu.RUnlock()

	result["mem"] = memKeys
	result["disk"] = diskKeys
	return result
}

func (s *Storage) buildCacheKey(url string) string {
	// Simple cache key generation - can be replaced with more sophisticated logic
	return url
}

// WaitForAsyncOps waits for all asynchronous operations to complete
// This is mainly useful for testing and graceful shutdown
func (s *Storage) WaitForAsyncOps() {
	s.asyncOps.Wait()
}
