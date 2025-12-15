package sidekick

import (
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
	"go.uber.org/zap"
)

var (
	ErrCacheExpired  = errors.New("cache expired")
	ErrCacheNotFound = errors.New("key not found in cache")

	CachedContentEncoding = []string{
		"none",
		"gzip",
		"br",
		"zstd",
	}
)

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

	// Mutex for file operations
	fileMu sync.RWMutex
	// Per-key mutexes for granular locking
	keyMutexes   map[string]*sync.RWMutex
	keyMutexesMu sync.Mutex
	// Mutex for disk usage tracking
	diskUsageMu sync.RWMutex
}

type MemoryCacheItem struct {
	*Metadata
	value []byte
}

const (
	CACHE_DIR = "sidekick-cache"
)

func NewStorage(loc string, ttl int, memMaxSize int, memMaxCount int, diskItemMaxSize int, diskMaxSize int, diskMaxCount int, logger *zap.Logger) *Storage {
	if err := os.MkdirAll(loc+"/"+CACHE_DIR, 0o755); err != nil {
		logger.Error("Failed to create cache directory", zap.Error(err))
	}

	s := &Storage{
		loc:    loc,
		ttl:    ttl,
		logger: logger,

		memMaxSize:      memMaxSize,
		memMaxCount:     memMaxCount,
		diskItemMaxSize: diskItemMaxSize,
		diskMaxSize:     diskMaxSize,
		diskMaxCount:    diskMaxCount,
		keyMutexes:      make(map[string]*sync.RWMutex),
	}
	memCache := NewMemoryCache[string, *MemoryCacheItem](memMaxCount, memMaxSize)
	s.memCache.Store(memCache)

	// Log storage configuration
	logger.Debug("Storage initialized",
		zap.String("location", loc),
		zap.Int("ttl", ttl),
		zap.String("memory_max_size", s.humanizeSize(int64(memMaxSize))),
		zap.Int("memory_max_count", memMaxCount),
		zap.String("disk_item_max_size", s.humanizeSize(int64(diskItemMaxSize))),
		zap.String("disk_max_size", s.humanizeSize(int64(diskMaxSize))),
		zap.Int("disk_max_count", diskMaxCount))

	// Calculate initial disk usage and count
	s.calculateDiskUsage()
	s.calculateDiskCount()

	return s
}

func (s *Storage) getMemCache() *MemoryCache[string, *MemoryCacheItem] {
	memCache, ok := s.memCache.Load().(*MemoryCache[string, *MemoryCacheItem])
	if !ok {
		return nil
	}
	return memCache
}

// getKeyMutex returns a mutex for the given key, creating one if needed
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

// cleanupKeyMutex removes the mutex for a key if no longer needed
func (s *Storage) cleanupKeyMutex(key string) {
	s.keyMutexesMu.Lock()
	defer s.keyMutexesMu.Unlock()
	delete(s.keyMutexes, key)
}

func (s *Storage) Get(key string, ce string) ([]byte, *Metadata, error) {
	key = strings.ReplaceAll(key, "/", "+")
	s.logger.Debug("Getting key from cache", zap.String("key", key), zap.String("ce", ce))

	memCache := s.getMemCache()
	if memCache == nil {
		return nil, nil, ErrCacheNotFound
	}

	// Get per-key mutex for reading
	keyMu := s.getKeyMutex(key)
	keyMu.RLock()
	defer keyMu.RUnlock()

	// load from memory or try load from disk
	var retErr error
	var cacheItem *MemoryCacheItem
	isDisk := false
	cacheKey := key + "::" + ce

	cacheItem, _ = memCache.LoadOrCompute(cacheKey, func() (*MemoryCacheItem, int, bool) {
		// Try to load from disk with file lock
		s.fileMu.RLock()
		defer s.fileMu.RUnlock()

		cacheMeta := &Metadata{}
		err := cacheMeta.LoadFromFile(path.Join(s.loc, CACHE_DIR, key, ".meta"))
		if err != nil {
			retErr = err
			return nil, 0, false
		}

		value, err := os.ReadFile(path.Join(s.loc, CACHE_DIR, key, "."+ce))
		if err != nil {
			retErr = err
			return nil, 0, false
		}

		isDisk = true
		return &MemoryCacheItem{
			Metadata: cacheMeta,
			value:    value,
		}, len(value), true
	})

	if cacheItem == nil {
		memCache.Delete(cacheKey)
	}

	if retErr != nil {
		s.logger.Debug("Error loading key from disk", zap.String("key", key), zap.String("ce", ce), zap.Error(retErr))
		return nil, nil, ErrCacheNotFound
	}

	if isDisk {
		s.logger.Debug("Pulled key from disk", zap.String("key", key), zap.String("ce", ce))
	} else {
		s.logger.Debug("Pulled key from memory", zap.String("key", key), zap.String("ce", ce))
	}

	if s.ttl > 0 {
		if time.Now().Unix() > cacheItem.Timestamp+int64(s.ttl) {
			s.logger.Debug("Cache expired", zap.String("key", key))
			// Clean up expired cache asynchronously
			go func() {
				s.Purge(key)
				s.cleanupKeyMutex(key)
			}()
			return nil, nil, ErrCacheExpired
		}
	}

	s.logger.Debug("Cache hit", zap.String("key", key), zap.String("ce", ce))
	return cacheItem.value, cacheItem.Metadata, nil
}

func (s *Storage) Set(reqPath string, cacheKey string, meta *Metadata, value []byte) error {
	key := s.buildCacheKey(reqPath, cacheKey)
	return s.SetWithKey(key, meta, value)
}

// SetWithKey stores data with a pre-built cache key
func (s *Storage) SetWithKey(key string, meta *Metadata, value []byte) error {
	s.logger.Debug("Cache Key", zap.String("Key", key), zap.String("ce", meta.contentEncoding))

	// Check disk item size limit
	if s.diskItemMaxSize > 0 && len(value) > s.diskItemMaxSize {
		s.logger.Debug("Item too large for disk cache",
			zap.String("key", key),
			zap.String("size", s.humanizeSize(int64(len(value)))),
			zap.String("limit", s.humanizeSize(int64(s.diskItemMaxSize))))
		return fmt.Errorf("item size %d exceeds disk limit %d", len(value), s.diskItemMaxSize)
	}

	// Check if disk cache is disabled
	if s.diskItemMaxSize == 0 || s.diskMaxSize == 0 {
		s.logger.Debug("Disk cache disabled", zap.String("key", key))
		// Still store in memory if memory cache is enabled
		if s.memMaxSize != 0 {
			return s.storeInMemory(key, meta, value)
		}
		return nil
	}

	key = strings.ReplaceAll(key, "/", "+")
	ce := meta.contentEncoding

	// Check disk space and count before storing
	needEviction := false
	evictionReason := ""

	s.diskUsageMu.RLock()
	currentUsage := s.diskUsage
	currentCount := s.diskItemCount
	s.diskUsageMu.RUnlock()

	if s.diskMaxSize > 0 && currentUsage+int64(len(value)) > int64(s.diskMaxSize) {
		needEviction = true
		evictionReason = "size"
		s.logger.Debug("Disk cache full (size), evicting old items",
			zap.String("current_usage", s.humanizeSize(currentUsage)),
			zap.String("needed_space", s.humanizeSize(int64(len(value)))),
			zap.String("disk_limit", s.humanizeSize(int64(s.diskMaxSize))))
	} else if s.diskMaxCount > 0 && currentCount >= int64(s.diskMaxCount) {
		needEviction = true
		evictionReason = "count"
		s.logger.Debug("Disk cache full (count), evicting old items",
			zap.Int64("current_count", currentCount),
			zap.Int("disk_max_count", s.diskMaxCount))
	}

	if needEviction {
		// Try to evict old items to make space
		s.evictOldestFromDisk(int64(len(value)), evictionReason)
	}

	// Compress data if not already compressed
	if ce == "none" || ce == "" {
		// Try to compress with gzip by default
		compressedData, err := s.compressData(value, "gzip")
		if err == nil && len(compressedData) < len(value) {
			// Compression is beneficial, also store compressed version
			compressedMeta := *meta
			compressedMeta.contentEncoding = "gzip"
			go func() {
				if err := s.storeData(key, "gzip", &compressedMeta, compressedData); err != nil {
					s.logger.Error("Failed to store compressed data", zap.Error(err))
				}
			}()
		}
	}

	return s.storeData(key, ce, meta, value)
}

// storeData stores the data with the given key and content encoding
func (s *Storage) storeData(key string, ce string, meta *Metadata, value []byte) error {
	// Get per-key mutex for writing
	keyMu := s.getKeyMutex(key)
	keyMu.Lock()
	defer keyMu.Unlock()

	// Store in memory cache if enabled
	memCache := s.getMemCache()
	if memCache != nil && s.memMaxSize != 0 {
		existed := memCache.Put(key+"::"+ce, &MemoryCacheItem{
			Metadata: meta,
			value:    value,
		}, len(value))

		s.logger.Debug("Setting key in memory cache",
			zap.String("key", key),
			zap.String("ce", ce),
			zap.String("size", s.humanizeSize(int64(len(value)))),
			zap.Bool("replace", existed))
	}

	// Skip disk storage if disabled
	if s.diskMaxSize == 0 || s.diskItemMaxSize == 0 {
		return nil
	}

	// Write to disk with file lock
	s.fileMu.Lock()
	defer s.fileMu.Unlock()

	// create page directory
	basePath := path.Join(s.loc, CACHE_DIR, key)
	if err := os.MkdirAll(basePath, 0o755); err != nil {
		s.logger.Error("Error creating cache directory", zap.Error(err))
		return err
	}

	dataPath := path.Join(basePath, "."+ce)
	metaPath := path.Join(basePath, ".meta")

	// Check if file already exists to update disk usage correctly
	oldSize := int64(0)
	if stat, err := os.Stat(dataPath); err == nil {
		oldSize = stat.Size()
	}
	if stat, err := os.Stat(metaPath); err == nil {
		oldSize += stat.Size()
	}

	err := os.WriteFile(dataPath, value, 0o644)
	if err != nil {
		s.logger.Error("Error writing data to cache", zap.Error(err))
		return err
	}

	err = meta.WriteToFile(metaPath)
	if err != nil {
		s.logger.Error("Error writing meta to cache", zap.Error(err))
		// Try to clean up the data file
		_ = os.Remove(dataPath)
		return err
	}

	// Update disk usage
	newSize := int64(len(value))
	if metaStat, err := os.Stat(metaPath); err == nil {
		newSize += metaStat.Size()
	}

	s.diskUsageMu.Lock()
	oldUsage := s.diskUsage
	s.diskUsage = s.diskUsage - oldSize + newSize
	newUsage := s.diskUsage
	// Update count if this is a new item
	if oldSize == 0 {
		s.diskItemCount++
	}
	s.diskUsageMu.Unlock()

	s.logger.Debug("Disk cache updated",
		zap.String("key", key),
		zap.String("old_usage", s.humanizeSize(oldUsage)),
		zap.String("new_usage", s.humanizeSize(newUsage)),
		zap.String("item_size", s.humanizeSize(newSize)),
		zap.Int64("item_count", s.diskItemCount))

	return nil
}

// storeInMemory stores data only in memory cache
func (s *Storage) storeInMemory(key string, meta *Metadata, value []byte) error {
	memCache := s.getMemCache()
	if memCache == nil || s.memMaxSize == 0 {
		return nil
	}

	key = strings.ReplaceAll(key, "/", "+")
	ce := meta.contentEncoding

	memCache.Put(key+"::"+ce, &MemoryCacheItem{
		Metadata: meta,
		value:    value,
	}, len(value))

	s.logger.Debug("Setting key in memory cache only", zap.String("key", key))
	return nil
}

// compressData compresses data using the specified encoding
func (s *Storage) compressData(data []byte, encoding string) ([]byte, error) {
	var buf bytes.Buffer

	switch encoding {
	case "gzip":
		writer := gzip.NewWriter(&buf)
		defer func() {
			if err := writer.Close(); err != nil {
				s.logger.Error("Failed to close gzip writer", zap.Error(err))
			}
		}()
		if _, err := writer.Write(data); err != nil {
			return nil, err
		}
		if err := writer.Close(); err != nil {
			return nil, err
		}

	case "br":
		writer := brotli.NewWriter(&buf)
		defer func() {
			if err := writer.Close(); err != nil {
				s.logger.Error("Failed to close brotli writer", zap.Error(err))
			}
		}()
		if _, err := writer.Write(data); err != nil {
			return nil, err
		}
		if err := writer.Close(); err != nil {
			return nil, err
		}

	case "zstd":
		writer, err := zstd.NewWriter(&buf)
		if err != nil {
			return nil, err
		}
		defer func() {
			if err := writer.Close(); err != nil {
				s.logger.Error("Failed to close zstd writer", zap.Error(err))
			}
		}()
		if _, err := writer.Write(data); err != nil {
			return nil, err
		}
		if err := writer.Close(); err != nil {
			return nil, err
		}

	default:
		return nil, fmt.Errorf("unsupported encoding: %s", encoding)
	}

	return buf.Bytes(), nil
}

func (s *Storage) Purge(key string) {
	key = strings.ReplaceAll(key, "/", "+")
	s.logger.Debug("Removing key from cache", zap.String("key", key))

	// Get per-key mutex for writing
	keyMu := s.getKeyMutex(key)
	keyMu.Lock()
	defer keyMu.Unlock()

	// Remove from memory cache
	memCache := s.getMemCache()
	if memCache != nil {
		rmKeys := make([]string, 0, 4)
		memCache.Range(func(k string, v *MemoryCacheItem) bool {
			if strings.HasPrefix(k, key) {
				rmKeys = append(rmKeys, k)
			}
			return true
		})
		for _, k := range rmKeys {
			s.logger.Debug("Removing key from mem cache", zap.String("key", k))
			memCache.Delete(k)
		}
	}

	// Remove from disk with file lock
	s.fileMu.Lock()
	defer s.fileMu.Unlock()

	basePath := path.Join(s.loc, CACHE_DIR)
	files, err := os.ReadDir(basePath)
	if err != nil {
		s.logger.Error("Error removing key from disk cache", zap.Error(err))
		return
	}

	removedSize := int64(0)
	for _, f := range files {
		name := f.Name()
		if !strings.HasPrefix(name, key) {
			continue
		}
		fp := path.Join(basePath, name)

		// Calculate size before removal
		if _, err := os.Stat(fp); err == nil {
			removedSize += calculateDirSize(fp)
		}

		err := os.RemoveAll(fp)
		if err != nil {
			s.logger.Error("Error removing key from disk cache", zap.String("fp", fp), zap.Error(err))
		}
	}

	// Update disk usage and count
	if removedSize > 0 {
		s.diskUsageMu.Lock()
		s.diskUsage -= removedSize
		if s.diskUsage < 0 {
			s.diskUsage = 0
		}
		// Decrease item count based on removed directories
		removedCount := 0
		for _, f := range files {
			name := f.Name()
			if strings.HasPrefix(name, key) {
				removedCount++
			}
		}
		s.diskItemCount -= int64(removedCount)
		if s.diskItemCount < 0 {
			s.diskItemCount = 0
		}
		s.diskUsageMu.Unlock()
	}
}

func (s *Storage) Flush() error {
	// Replace memory cache
	s.memCache.Store(NewMemoryCache[string, *MemoryCacheItem](s.memMaxCount, s.memMaxSize))

	// Clear all key mutexes
	s.keyMutexesMu.Lock()
	s.keyMutexes = make(map[string]*sync.RWMutex)
	s.keyMutexesMu.Unlock()

	// Reset disk count
	s.diskUsageMu.Lock()
	s.diskItemCount = 0
	s.diskUsageMu.Unlock()

	// Remove from disk with file lock
	s.fileMu.Lock()
	defer s.fileMu.Unlock()

	basePath := path.Join(s.loc, CACHE_DIR)
	files, err := os.ReadDir(basePath)
	if err != nil {
		s.logger.Error("Error flushing cache", zap.Error(err))
		return err
	}

	for _, f := range files {
		fp := path.Join(basePath, f.Name())
		err = os.RemoveAll(fp)
		if err != nil {
			s.logger.Error("Error flushing cache", zap.String("fp", fp), zap.Error(err))
		}
	}
	return nil
}

func (s *Storage) List() map[string][]string {
	memCache := s.getMemCache()
	list := make(map[string][]string)

	if memCache != nil {
		list["mem"] = make([]string, 0, memCache.Size())
		memCache.Range(func(key string, value *MemoryCacheItem) bool {
			list["mem"] = append(list["mem"], key)
			return true
		})
	} else {
		list["mem"] = []string{}
	}

	s.fileMu.RLock()
	defer s.fileMu.RUnlock()

	basePath := path.Join(s.loc, CACHE_DIR)
	files, err := os.ReadDir(basePath)
	list["disk"] = make([]string, 0)

	if err == nil {
		for _, file := range files {
			if !file.IsDir() {
				continue
			}
			dirName := file.Name()
			fp := path.Join(basePath, dirName)
			for _, name := range CachedContentEncoding {
				ckPath := path.Join(fp, "."+name)
				_, err := os.Stat(ckPath)
				if errors.Is(err, os.ErrNotExist) {
					continue
				}
				list["disk"] = append(list["disk"], dirName+"::"+name)
			}
		}
	}

	if memCache != nil {
		list["debug"] = []string{
			fmt.Sprintf("max_size=%v", s.memMaxSize),
			fmt.Sprintf("max_count=%v", s.memMaxCount),
			fmt.Sprintf("size=%v", memCache.Cost()),
			fmt.Sprintf("count=%v", memCache.Size()),
		}
	} else {
		list["debug"] = []string{
			fmt.Sprintf("max_size=%v", s.memMaxSize),
			fmt.Sprintf("max_count=%v", s.memMaxCount),
			"memory_cache=nil",
		}
	}

	return list
}

func (s *Storage) buildCacheKey(reqPath string, cacheKey string) string {
	return fmt.Sprintf("%v::%v", reqPath, cacheKey)
}

// calculateDiskCount calculates the current number of items in disk cache
func (s *Storage) calculateDiskCount() {
	basePath := path.Join(s.loc, CACHE_DIR)

	count := int64(0)
	files, err := os.ReadDir(basePath)
	if err == nil {
		for _, f := range files {
			if f.IsDir() {
				count++
			}
		}
	}

	s.diskUsageMu.Lock()
	s.diskItemCount = count
	s.diskUsageMu.Unlock()

	s.logger.Debug("Calculated disk item count",
		zap.Int64("count", count))

	// Warn if count exceeds limit
	if s.diskMaxCount > 0 && count > int64(s.diskMaxCount) {
		s.logger.Warn("Current disk item count exceeds configured limit",
			zap.Int64("count", count),
			zap.Int("limit", s.diskMaxCount))
	}
}

// calculateDiskUsage calculates the current disk usage of the cache
func (s *Storage) calculateDiskUsage() {
	basePath := path.Join(s.loc, CACHE_DIR)

	size := int64(0)
	err := filepath.Walk(basePath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // Skip errors
		}
		if !info.IsDir() {
			size += info.Size()
		}
		return nil
	})

	if err != nil {
		s.logger.Error("Error calculating disk usage", zap.Error(err))
	}

	s.diskUsageMu.Lock()
	s.diskUsage = size
	s.diskUsageMu.Unlock()

	s.logger.Debug("Calculated disk usage",
		zap.String("size", s.humanizeSize(size)),
		zap.Int64("bytes", size))

	// Warn if disk usage exceeds limit
	if s.diskMaxSize > 0 && size > int64(s.diskMaxSize) {
		s.logger.Warn("Current disk usage exceeds configured limit",
			zap.String("usage", s.humanizeSize(size)),
			zap.String("limit", s.humanizeSize(int64(s.diskMaxSize))))
	}
}

// calculateDirSize calculates the size of a directory
func calculateDirSize(dirPath string) int64 {
	var size int64
	filepath.Walk(dirPath, func(path string, info os.FileInfo, err error) error { //nolint:errcheck
		if err != nil {
			return nil
		}
		if !info.IsDir() {
			size += info.Size()
		}
		return nil
	})
	return size
}

// evictOldestFromDisk removes oldest items from disk to make space or reduce count
func (s *Storage) evictOldestFromDisk(neededSpace int64, reason string) {
	s.diskUsageMu.RLock()
	currentUsage := s.diskUsage
	currentCount := s.diskItemCount
	s.diskUsageMu.RUnlock()

	// Determine eviction targets based on reason
	targetUsage := int64(s.diskMaxSize) - neededSpace

	// Check if we need to evict
	needEvictForSize := s.diskMaxSize > 0 && currentUsage > targetUsage
	needEvictForCount := s.diskMaxCount > 0 && currentCount >= int64(s.diskMaxCount)

	if !needEvictForSize && !needEvictForCount {
		return // No eviction needed
	}

	basePath := path.Join(s.loc, CACHE_DIR)

	// Get all cache directories with their modification times
	type cacheEntry struct {
		path    string
		modTime time.Time
		size    int64
	}

	var entries []cacheEntry
	files, err := os.ReadDir(basePath)
	if err != nil {
		s.logger.Error("Error reading cache directory for eviction", zap.Error(err))
		return
	}

	for _, f := range files {
		if !f.IsDir() {
			continue
		}

		fp := path.Join(basePath, f.Name())
		info, err := os.Stat(fp)
		if err != nil {
			continue
		}

		entries = append(entries, cacheEntry{
			path:    fp,
			modTime: info.ModTime(),
			size:    calculateDirSize(fp),
		})
	}

	// Sort by modification time (oldest first)
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].modTime.Before(entries[j].modTime)
	})

	// Remove oldest entries until we meet our targets
	freedSpace := int64(0)
	freedCount := int64(0)
	for _, entry := range entries {
		// Check if we've freed enough based on constraints
		if reason == "size" && s.diskMaxSize > 0 {
			if currentUsage-freedSpace <= targetUsage {
				break
			}
		} else if reason == "count" && s.diskMaxCount > 0 {
			// Make room for one new item
			if currentCount-freedCount < int64(s.diskMaxCount) {
				break
			}
		}

		err := os.RemoveAll(entry.path)
		if err != nil {
			s.logger.Error("Error removing cache entry during eviction",
				zap.String("path", entry.path),
				zap.Error(err))
			continue
		}

		freedSpace += entry.size
		freedCount++
		s.logger.Debug("Evicted cache entry",
			zap.String("path", entry.path),
			zap.String("size", s.humanizeSize(entry.size)),
			zap.String("age", time.Since(entry.modTime).String()))
	}

	// Update disk usage and count
	s.diskUsageMu.Lock()
	oldUsage := s.diskUsage
	oldCount := s.diskItemCount
	s.diskUsage -= freedSpace
	if s.diskUsage < 0 {
		s.diskUsage = 0
	}
	s.diskItemCount -= freedCount
	if s.diskItemCount < 0 {
		s.diskItemCount = 0
	}
	newUsage := s.diskUsage
	newCount := s.diskItemCount
	s.diskUsageMu.Unlock()

	if freedSpace > 0 || freedCount > 0 {
		s.logger.Info("Disk cache eviction completed",
			zap.String("reason", reason),
			zap.String("freed_space", s.humanizeSize(freedSpace)),
			zap.Int64("freed_count", freedCount),
			zap.String("old_usage", s.humanizeSize(oldUsage)),
			zap.String("new_usage", s.humanizeSize(newUsage)),
			zap.Int64("old_count", oldCount),
			zap.Int64("new_count", newCount))
	}
}

// humanizeSize converts bytes to human-readable format
func (s *Storage) humanizeSize(bytes int64) string {
	if bytes < 0 {
		return "unlimited"
	}
	if bytes == 0 {
		return "0B"
	}

	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%dB", bytes)
	}

	div, exp := int64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}

	units := []string{"B", "KB", "MB", "GB", "TB"}
	return fmt.Sprintf("%.1f%s", float64(bytes)/float64(div), units[exp+1])
}
