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
	memCache    atomic.Value // *MemoryCache

	// Disk cache
	diskItemMaxSize int
	diskMaxSize     int
	diskMaxCount    int
	diskCache       atomic.Value // *DiskCache

	// Mutex for file operations
	fileMu sync.RWMutex
	// Per-key mutexes for granular locking
	keyMutexes   map[string]*sync.RWMutex
	keyMutexesMu sync.Mutex
	// WaitGroup for tracking async operations
	asyncOps sync.WaitGroup
}

type MemoryCacheItem struct {
	*Metadata
	value bytes.Buffer
}

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
	}

	// Initialize memory cache
	memCache := NewMemoryCache[string, *MemoryCacheItem](s.memMaxCount, s.memMaxSize)
	s.memCache.Store(memCache)

	// Create cache directory if it doesn't exist
	if err := os.MkdirAll(loc, 0755); err != nil {
		if logger != nil {
			logger.Error("Failed to create cache directory", zap.String("path", loc), zap.Error(err))
		}
	}

	// Initialize disk cache
	diskCache := NewDiskCache(loc, diskMaxCount, int64(diskMaxSize), int64(diskItemMaxSize), logger)
	s.diskCache.Store(diskCache)

	// Load disk cache index asynchronously
	s.asyncOps.Add(1)
	go func() {
		defer s.asyncOps.Done()
		if dc := s.GetDiskCache(); dc != nil {
			_ = dc.LoadIndex()
		}
	}()

	return s
}

func (s *Storage) GetMemCache() *MemoryCache[string, *MemoryCacheItem] {
	cache := s.memCache.Load()
	if cache == nil {
		return nil
	}
	return cache.(*MemoryCache[string, *MemoryCacheItem])
}

func (s *Storage) GetDiskCache() *DiskCache {
	cache := s.diskCache.Load()
	if cache == nil {
		return nil
	}
	return cache.(*DiskCache)
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
	memCache := s.GetMemCache()
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
	diskCache := s.GetDiskCache()
	if diskCache != nil {
		item, err := diskCache.Get(key)
		if err == nil && item != nil {
			// Read the actual data from disk
			data, md, err := s.readDiskCacheData(key, item.Path)
			if err == nil {
				if s.logger != nil {
					s.logger.Debug("Cache hit (disk)",
						zap.String("key", key),
						zap.Int("data_size", len(data)))
				}
				return data, md, nil
			}
			if err == ErrCacheExpired {
				// Clean up expired entry
				_ = diskCache.Delete(key)
				return nil, nil, err
			}
			// Log other errors but continue
			if s.logger != nil {
				s.logger.Warn("Failed to read from disk cache",
					zap.String("key", key),
					zap.Error(err))
			}
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
	diskCache := s.GetDiskCache()
	if diskCache != nil {
		cacheDir := path.Join(s.loc, key)
		dataFilePath := path.Join(cacheDir, "data")

		// Store the data
		if err := s.storeDataToDisk(cacheDir, dataFilePath, data, metadata); err != nil {
			if s.logger != nil {
				s.logger.Error("Failed to store data on disk",
					zap.String("key", key),
					zap.Error(err))
			}
			return err
		}

		// Calculate actual size on disk
		var actualSize int64
		filepath.Walk(cacheDir, func(path string, info os.FileInfo, err error) error { //nolint:errcheck
			if err == nil && !info.IsDir() {
				actualSize += info.Size()
			}
			return nil
		})

		// Create disk cache item
		item := &DiskCacheItem{
			Metadata:   metadata,
			Path:       cacheDir,
			Size:       actualSize,
			AccessTime: time.Now(),
			ModTime:    time.Now(),
		}

		// Add to disk cache
		if err := diskCache.Put(key, item); err != nil {
			// If we can't add to cache index, remove the files
			_ = os.RemoveAll(cacheDir)
			return err
		}

		if s.logger != nil {
			s.logger.Debug("Stored on disk",
				zap.String("key", key),
				zap.String("size", diskCache.humanizeSize(actualSize)))
		}
	}

	return nil
}

func (s *Storage) storeInMemory(key string, data []byte, metadata *Metadata) error {
	memCache := s.GetMemCache()
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
	memCache := s.GetMemCache()
	if memCache != nil {
		memCache.Delete(key)
	}

	// Remove from disk cache
	diskCache := s.GetDiskCache()
	if diskCache != nil {
		if err := diskCache.Delete(key); err != nil && !os.IsNotExist(err) {
			if s.logger != nil {
				s.logger.Error("Failed to purge cache",
					zap.String("key", key),
					zap.Error(err))
			}
			return err
		}
	}

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
	s.fileMu.Lock()
	// Remove contents of cache directory, not the directory itself
	dir, err := os.Open(s.loc)
	if err == nil {
		defer dir.Close() //nolint:errcheck
		entries, err := dir.Readdirnames(-1)
		if err == nil {
			for _, entry := range entries {
				fullPath := filepath.Join(s.loc, entry)
				if removeErr := os.RemoveAll(fullPath); removeErr != nil && err == nil {
					err = removeErr
				}
			}
		}
	} else if os.IsNotExist(err) {
		// If directory doesn't exist, create it
		err = os.MkdirAll(s.loc, 0755)
	}
	s.fileMu.Unlock()

	// Reinitialize disk cache
	if err == nil {
		diskCache := NewDiskCache(s.loc, s.diskMaxCount, int64(s.diskMaxSize), int64(s.diskItemMaxSize), s.logger)
		s.diskCache.Store(diskCache)
	}

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
	memCache := s.GetMemCache()
	if memCache != nil {
		memCache.Range(func(key string, _ *MemoryCacheItem) bool {
			memKeys = append(memKeys, key)
			return true
		})
	}

	// Get keys from disk cache
	diskCache := s.GetDiskCache()
	if diskCache != nil {
		diskKeys = diskCache.List()
	}

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
