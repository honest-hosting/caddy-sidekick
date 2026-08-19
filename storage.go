package sidekick

import (
	"bytes"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
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
	// ErrNotStreamable means the entry exists but cannot be served without being
	// materialized first (it is compressed on disk). Callers should fall back to Get.
	ErrNotStreamable = errors.New("cache entry is not streamable")
)

// keyMutexShards is the number of per-key lock shards. Large enough that unrelated
// keys rarely collide, small enough to stay a trivial fixed allocation.
const keyMutexShards = 256

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

	memItemMaxSize int
	memMaxSize     int
	memMaxCount    int
	memCache       atomic.Value // *MemoryCache

	// Disk cache
	diskItemMaxSize int
	diskMaxSize     int
	diskMaxCount    int
	diskCache       atomic.Value // *DiskCache

	// compressMaxSize is the largest body that will be considered for compression
	// before storing. Bodies above it are stored verbatim. See compressData.
	compressMaxSize int

	// Mutex for file operations
	fileMu sync.RWMutex
	// Per-key mutexes for granular locking, sharded by key hash.
	//
	// A fixed array rather than a map keyed by cache key: the map grew without
	// bound because entries were only ever removed by Purge, so every distinct URL
	// ever requested leaked a mutex for the process lifetime. Sharding gives the
	// same granularity guarantee (the same key always maps to the same mutex) with
	// no lifecycle to manage. Distinct keys may share a shard, which costs
	// occasional false contention and nothing else.
	keyMutexes [keyMutexShards]sync.RWMutex
	// WaitGroup for tracking async operations
	asyncOps sync.WaitGroup
}

type MemoryCacheItem struct {
	*Metadata
	value bytes.Buffer
}

// NewStorage creates a new Storage instance
func NewStorage(loc string, ttl int, memItemMaxSize int, memMaxSize int, memMaxCount int, diskItemMaxSize int, diskMaxSize int, diskMaxCount int, logger *zap.Logger) *Storage {
	s := &Storage{
		loc:             loc,
		ttl:             ttl,
		logger:          logger,
		memItemMaxSize:  memItemMaxSize,
		memMaxSize:      memMaxSize,
		memMaxCount:     memMaxCount,
		diskItemMaxSize: diskItemMaxSize,
		diskMaxSize:     diskMaxSize,
		diskMaxCount:    diskMaxCount,
		compressMaxSize: DefaultCompressMaxSize,
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

// SetCompressMaxSize configures the largest body considered for compression before
// storing. A value of 0 or less disables the size guard (unlimited).
//
// This is applied after construction rather than being another NewStorage parameter:
// that constructor already takes nine positional values and is called from ~40 test
// sites, so widening it buys churn rather than clarity.
func (s *Storage) SetCompressMaxSize(n int) {
	s.compressMaxSize = n
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

// getKeyMutex returns the mutex guarding a specific key. The same key always maps to
// the same shard, so per-key mutual exclusion holds; different keys may share one.
func (s *Storage) getKeyMutex(key string) *sync.RWMutex {
	h := fnv.New32a()
	_, _ = h.Write([]byte(key))
	return &s.keyMutexes[h.Sum32()%keyMutexShards]
}

func (s *Storage) Get(key string) ([]byte, *Metadata, error) {
	// Get key-specific lock for reading
	keyMu := s.getKeyMutex(key)
	keyMu.RLock()
	defer keyMu.RUnlock()

	// Check memory cache first
	memCache := s.GetMemCache()
	if memCache != nil {
		if cacheItem, ok := memCache.Get(key); ok && cacheItem != nil && (*cacheItem).Metadata != nil {
			// Check TTL
			if s.isExpired((*cacheItem).Timestamp) {
				s.purgeAsync(key)
				return nil, nil, ErrCacheExpired
			}

			if s.logger != nil {
				s.logger.Debug("Cache hit (memory)",
					zap.String("key", key),
					zap.Int("data_size", (*cacheItem).value.Len()))
			}
			// Record metrics
			metrics := GetMetrics()
			metrics.RecordCacheOperation("get", "hit", "memory")
			metrics.RecordItemSize("memory", "default", int64((*cacheItem).value.Len()))
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
				// Record metrics
				metrics := GetMetrics()
				metrics.RecordCacheOperation("get", "hit", "disk")
				metrics.RecordItemSize("disk", "default", int64(len(data)))
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

	// Record cache miss
	metrics := GetMetrics()
	metrics.RecordCacheOperation("get", "miss", "default")
	return nil, nil, ErrCacheNotFound
}

// GetReader returns a seekable reader over a cached entry without materializing the
// body in memory, along with its metadata and size.
//
// This is the path that makes large objects cheap to serve: Get reads the whole entry
// into a []byte on every request, so a cached 34MB video costs 34MB of transient heap
// per concurrent viewer. A disk-backed entry returned here is an open *os.File, so the
// bytes go from page cache to socket without a copy through the handler.
//
// Returns ErrNotStreamable when the entry is compressed on disk and would have to be
// decompressed in full anyway; callers should fall back to Get in that case. The
// caller MUST Close the returned reader.
func (s *Storage) GetReader(key string) (io.ReadSeekCloser, *Metadata, int64, error) {
	keyMu := s.getKeyMutex(key)
	keyMu.RLock()
	defer keyMu.RUnlock()

	// Memory tier: entries are capped at cache_memory_item_max_size, so there is
	// nothing to stream. Hand back the bytes we already hold.
	memCache := s.GetMemCache()
	if memCache != nil {
		if cacheItem, ok := memCache.Get(key); ok && cacheItem != nil && (*cacheItem).Metadata != nil {
			if s.isExpired((*cacheItem).Timestamp) {
				s.purgeAsync(key)
				return nil, nil, 0, ErrCacheExpired
			}

			data := (*cacheItem).value.Bytes()
			metrics := GetMetrics()
			metrics.RecordCacheOperation("get", "hit", "memory")
			metrics.RecordItemSize("memory", "default", int64(len(data)))
			return nopSeekCloser{bytes.NewReader(data)}, (*cacheItem).Metadata, int64(len(data)), nil
		}
	}

	diskCache := s.GetDiskCache()
	if diskCache == nil {
		return nil, nil, 0, ErrCacheNotFound
	}

	item, err := diskCache.Get(key)
	if err != nil || item == nil {
		metrics := GetMetrics()
		metrics.RecordCacheOperation("get", "miss", "default")
		return nil, nil, 0, ErrCacheNotFound
	}

	md := &Metadata{}
	if err := md.LoadFromFile(filepath.Join(item.Path, "metadata.json")); err != nil {
		return nil, nil, 0, fmt.Errorf("failed to load metadata: %w", err)
	}

	if s.isExpired(md.Timestamp) {
		_ = diskCache.Delete(key)
		return nil, nil, 0, ErrCacheExpired
	}

	// A compressed entry has to be decompressed in full to be useful, so there is
	// nothing to gain from streaming it. Phase 3.1 keeps media uncompressed, which
	// is what makes this path apply to the objects that actually matter.
	if ct := md.HeaderValue("X-Compression-Type"); ct != "" && ct != "none" {
		return nil, nil, 0, ErrNotStreamable
	}

	// Take the file lock only to open, never across the response.
	//
	// fileMu is Storage-wide, not per-key: holding it while streaming a large body
	// to a slow client would stall every write in the process. Releasing it here is
	// safe because eviction removes entries with os.RemoveAll, and on Linux an open
	// descriptor keeps the inode alive after the directory entry is unlinked — so a
	// response already in flight completes correctly even if the entry is evicted
	// mid-stream.
	s.fileMu.RLock()
	f, err := os.Open(filepath.Join(item.Path, "data"))
	s.fileMu.RUnlock()
	if err != nil {
		return nil, nil, 0, fmt.Errorf("failed to open data file: %w", err)
	}

	size, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		_ = f.Close()
		return nil, nil, 0, fmt.Errorf("failed to size data file: %w", err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		_ = f.Close()
		return nil, nil, 0, fmt.Errorf("failed to rewind data file: %w", err)
	}

	if s.logger != nil {
		s.logger.Debug("Cache hit (disk, streamed)",
			zap.String("key", key), zap.Int64("size", size))
	}
	metrics := GetMetrics()
	metrics.RecordCacheOperation("get", "hit", "disk")
	metrics.RecordItemSize("disk", "default", size)

	return f, md, size, nil
}

// isExpired reports whether an entry stamped at ts has outlived the configured TTL.
//
// Compared as a duration rather than by whole seconds. The two tiers previously
// disagreed here — the memory path used integer-second arithmetic while the disk path
// used time.Since — which made expiry differ by up to a second depending on which
// tier an entry happened to land in. This is the disk path's (more precise) rule,
// now applied to both.
func (s *Storage) isExpired(ts int64) bool {
	if s.ttl <= 0 || ts <= 0 {
		return false
	}
	return time.Since(time.Unix(ts, 0)) > time.Duration(s.ttl)*time.Second
}

// purgeAsync removes an entry in the background, used when a read discovers it has
// expired and must not block on cleanup.
func (s *Storage) purgeAsync(key string) {
	s.asyncOps.Add(1)
	go func() {
		defer s.asyncOps.Done()
		_ = s.Purge(key)
	}()
}

// nopSeekCloser adapts an in-memory reader to io.ReadSeekCloser so callers can treat
// memory- and disk-backed entries identically.
type nopSeekCloser struct {
	*bytes.Reader
}

func (nopSeekCloser) Close() error { return nil }

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

	// Try to store in memory first if it fits within the per-item size limit and memory cache is enabled
	if s.memMaxSize > 0 && s.memItemMaxSize > 0 && dataSize <= s.memItemMaxSize {
		if err := s.storeInMemory(key, data, metadata); err == nil {
			if s.logger != nil {
				s.logger.Debug("Stored in memory cache",
					zap.String("key", key),
					zap.Int("size", dataSize))
			}
			// Record metrics
			metrics := GetMetrics()
			metrics.RecordCacheOperation("store", "success", "memory")
			metrics.RecordItemSize("memory", "default", int64(dataSize))
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
		// Record metrics
		metrics := GetMetrics()
		metrics.RecordCacheOperation("store", "success", "disk")
		metrics.RecordItemSize("disk", "default", actualSize)
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

	// Record metrics
	metrics := GetMetrics()
	metrics.RecordCacheOperation("purge", "success", "default")

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
