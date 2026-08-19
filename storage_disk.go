package sidekick

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
)

// DiskCacheItem represents an item stored in disk cache
type DiskCacheItem struct {
	*Metadata
	Path string // File path on disk
	Size int64  // Size in bytes
	// AccessTime is when the entry was added to (or loaded into) the index. It is
	// not refreshed on read — see DiskCache.Get. Eviction recency is tracked by the
	// LRU list, not by this field.
	AccessTime time.Time
	ModTime    time.Time // Last modification time
}

// DiskCache wraps the generic LRU cache with disk-specific functionality
type DiskCache struct {
	lru *MemoryCache[string, *DiskCacheItem]

	// Disk usage tracking
	diskUsage     atomic.Int64 // Current disk usage in bytes
	diskItemCount atomic.Int64 // Current number of items on disk

	// Configuration
	maxDiskSize  int64
	maxItemSize  int64
	maxItemCount int64

	// Base path for cache storage
	basePath string

	// Logger
	logger *zap.Logger

	// Mutex for file operations
	fileMu sync.RWMutex
}

// NewDiskCache creates a new disk cache instance
func NewDiskCache(basePath string, maxItemCount int, maxDiskSize int64, maxItemSize int64, logger *zap.Logger) *DiskCache {
	dc := &DiskCache{
		// Use the generic LRU cache, with maxDiskSize as the cost limit
		lru:          NewMemoryCache[string, *DiskCacheItem](maxItemCount, int(maxDiskSize)),
		maxDiskSize:  maxDiskSize,
		maxItemSize:  maxItemSize,
		maxItemCount: int64(maxItemCount),
		basePath:     basePath,
		logger:       logger,
	}

	return dc
}

// Get retrieves an item from disk cache
func (dc *DiskCache) Get(key string) (*DiskCacheItem, error) {
	// Check if item exists in LRU index
	item, exists := dc.lru.Get(key)
	if !exists || item == nil {
		return nil, ErrCacheNotFound
	}

	// Deliberately does NOT stamp AccessTime here. Storage.Get holds only a read
	// lock, so concurrent readers of the same key would all write that field at
	// once — a data race, and one the race detector flags under concurrent range
	// requests. Nothing reads AccessTime: recency for eviction comes from the LRU
	// list, which lru.Get above has already updated under its own lock.
	return *item, nil
}

// Put adds or updates an item in disk cache
func (dc *DiskCache) Put(key string, item *DiskCacheItem) error {
	// Check item size limit
	if dc.maxItemSize > 0 && item.Size > dc.maxItemSize {
		return fmt.Errorf("item size %d exceeds max item size %d", item.Size, dc.maxItemSize)
	}

	// Check if we need to evict items before adding
	dc.checkAndEvict(item.Size)

	// Store in LRU cache with size as cost
	oldItem, existed := dc.lru.Peek(key)
	dc.lru.Put(key, item, int(item.Size))

	// Update disk usage
	if existed && oldItem != nil {
		// Replacing existing item
		sizeDiff := item.Size - (*oldItem).Size
		dc.diskUsage.Add(sizeDiff)
	} else {
		// New item
		dc.diskUsage.Add(item.Size)
		dc.diskItemCount.Add(1)
	}

	return nil
}

// Delete removes an item from disk cache
func (dc *DiskCache) Delete(key string) error {
	item, exists := dc.lru.Peek(key)
	if !exists || item == nil {
		return nil
	}

	// Remove from LRU
	dc.lru.Delete(key)

	// Update disk usage
	dc.diskUsage.Add(-(*item).Size)
	dc.diskItemCount.Add(-1)

	// Remove actual files
	dc.fileMu.Lock()
	err := os.RemoveAll((*item).Path)
	dc.fileMu.Unlock()

	if err != nil && !os.IsNotExist(err) {
		if dc.logger != nil {
			dc.logger.Error("Failed to delete cache files",
				zap.String("key", key),
				zap.String("path", (*item).Path),
				zap.Error(err))
		}
		return err
	}

	return nil
}

// List returns all keys in the disk cache
func (dc *DiskCache) List() []string {
	keys := make([]string, 0)
	dc.lru.Range(func(key string, _ *DiskCacheItem) bool {
		keys = append(keys, key)
		return true
	})
	return keys
}

// Stats returns current disk cache statistics
func (dc *DiskCache) Stats() (itemCount int64, diskUsage int64) {
	return dc.diskItemCount.Load(), dc.diskUsage.Load()
}

// checkAndEvict checks if eviction is needed and performs it
func (dc *DiskCache) checkAndEvict(neededSpace int64) {
	currentUsage := dc.diskUsage.Load()
	currentCount := dc.diskItemCount.Load()

	// Check if we need to evict for size
	if dc.maxDiskSize > 0 && currentUsage+neededSpace > dc.maxDiskSize {
		dc.evictForSpace(neededSpace)
	}

	// Check if we need to evict for count
	if dc.maxItemCount > 0 && currentCount >= dc.maxItemCount {
		dc.evictForCount()
	}
}

// evictForSpace evicts items to make space
func (dc *DiskCache) evictForSpace(neededSpace int64) {
	targetUsage := dc.maxDiskSize - neededSpace
	currentUsage := dc.diskUsage.Load()

	if currentUsage <= targetUsage {
		return
	}

	evictedCount := 0
	evictedSize := int64(0)

	// Keep removing least recently used items until we have enough space
	for currentUsage > targetUsage && dc.lru.Size() > 0 {
		// Get LRU keys (oldest first)
		lruKeys := dc.lru.GetLRUKeys(10) // Get up to 10 items at a time for efficiency

		if len(lruKeys) == 0 {
			break
		}

		// Delete LRU items until we have enough space
		for _, key := range lruKeys {
			if currentUsage <= targetUsage {
				break
			}

			item, exists := dc.lru.Peek(key)
			if exists && item != nil {
				evictedSize += (*item).Size
				evictedCount++
				_ = dc.Delete(key)
				currentUsage = dc.diskUsage.Load()
			}
		}
	}

	if dc.logger != nil && evictedCount > 0 {
		dc.logger.Info("Evicted items from disk cache for space",
			zap.Int("count", evictedCount),
			zap.String("size", dc.humanizeSize(evictedSize)))
	}
}

// evictForCount evicts items to stay within count limit
func (dc *DiskCache) evictForCount() {
	currentCount := dc.diskItemCount.Load()
	if currentCount < dc.maxItemCount {
		return
	}

	// Need to evict at least one item
	toEvict := currentCount - dc.maxItemCount + 1
	evicted := 0

	// Get LRU keys and evict them
	lruKeys := dc.lru.GetLRUKeys(int(toEvict))
	for _, key := range lruKeys {
		_ = dc.Delete(key)
		evicted++
		if int64(evicted) >= toEvict {
			break
		}
	}

	if dc.logger != nil && evicted > 0 {
		dc.logger.Info("Evicted items from disk cache for count",
			zap.Int("count", evicted))
	}
}

// LoadIndex loads the disk cache index from filesystem
// This is called on startup to rebuild the cache index
func (dc *DiskCache) LoadIndex() error {
	startTime := time.Now()

	// Clean the base path for consistent comparison
	cleanBasePath := filepath.Clean(dc.basePath)

	// Use goroutines for parallel loading
	entriesChan := make(chan *DiskCacheItem, 100)
	var wg sync.WaitGroup

	// Worker pool to process directories
	numWorkers := 8
	dirChan := make(chan string, 100)

	// Start workers
	for i := 0; i < numWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for dirPath := range dirChan {
				if item := dc.loadCacheEntry(dirPath); item != nil {
					entriesChan <- item
				}
			}
		}()
	}

	// Scan directories
	go func() {
		dirCount := 0
		err := filepath.Walk(cleanBasePath, func(dirPath string, info os.FileInfo, err error) error {
			if err != nil {
				return nil
			}

			if !info.IsDir() || dirPath == cleanBasePath {
				return nil
			}

			// Check if this is a cache directory (direct child of base path)
			parent := filepath.Dir(dirPath)
			if parent == cleanBasePath {
				dirCount++
				dirChan <- dirPath
			}
			return filepath.SkipDir // Don't recurse into subdirectories
		})

		if err != nil && dc.logger != nil {
			dc.logger.Error("LoadIndex walk error", zap.Error(err))
		}
		close(dirChan)
	}()

	// Collector goroutine
	go func() {
		wg.Wait()
		close(entriesChan)
	}()

	// Collect and add all entries
	totalSize := int64(0)
	itemCount := 0
	for item := range entriesChan {
		key := filepath.Base(item.Path)
		dc.lru.Put(key, item, int(item.Size))
		totalSize += item.Size
		itemCount++
	}

	// Update stats
	dc.diskUsage.Store(totalSize)
	dc.diskItemCount.Store(int64(itemCount))

	if dc.logger != nil {
		dc.logger.Info("Disk cache index loaded",
			zap.Duration("load_time", time.Since(startTime)),
			zap.Int("items", itemCount),
			zap.String("size", dc.humanizeSize(totalSize)))
	}

	return nil
}

// loadCacheEntry loads a single cache entry from disk
func (dc *DiskCache) loadCacheEntry(dirPath string) *DiskCacheItem {
	info, err := os.Stat(dirPath)
	if err != nil {
		return nil
	}

	// Calculate directory size
	var size int64
	_ = filepath.Walk(dirPath, func(path string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			size += info.Size()
		}
		return nil
	})

	// Load metadata if available
	metadataPath := filepath.Join(dirPath, "metadata.json")
	metadata := &Metadata{}
	accessTime := info.ModTime() // Default to mod time

	if err := metadata.LoadFromFile(metadataPath); err == nil && metadata.Timestamp > 0 {
		accessTime = time.Unix(metadata.Timestamp, 0)
	}

	return &DiskCacheItem{
		Metadata:   metadata,
		Path:       dirPath,
		Size:       size,
		AccessTime: accessTime,
		ModTime:    info.ModTime(),
	}
}

// storeData stores data to disk with compression
func (s *Storage) storeDataToDisk(cacheDir, dataFilePath string, data []byte, md *Metadata) error {
	// Create cache directory
	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		return fmt.Errorf("failed to create cache directory: %w", err)
	}

	// Compress data if beneficial
	compressedData, compressionType := s.compressData(data, md)

	// Update metadata with compression info
	if compressionType != "" {
		md.Header = append(md.Header, []string{"X-Compression-Type", compressionType})
	}

	// Save metadata
	metadataPath := filepath.Join(cacheDir, "metadata.json")
	if err := md.WriteToFile(metadataPath); err != nil {
		return fmt.Errorf("failed to save metadata: %w", err)
	}

	// Write data file
	s.fileMu.Lock()
	defer s.fileMu.Unlock()

	file, err := os.Create(dataFilePath)
	if err != nil {
		return fmt.Errorf("failed to create data file: %w", err)
	}
	defer func() { _ = file.Close() }()

	if _, err := file.Write(compressedData); err != nil {
		return fmt.Errorf("failed to write data: %w", err)
	}

	return nil
}

// readDiskCacheData reads data from disk cache
func (s *Storage) readDiskCacheData(key string, cacheDir string) ([]byte, *Metadata, error) {
	// Load metadata
	metadataPath := filepath.Join(cacheDir, "metadata.json")
	md := &Metadata{}
	if err := md.LoadFromFile(metadataPath); err != nil {
		return nil, nil, fmt.Errorf("failed to load metadata: %w", err)
	}

	// Check TTL
	if s.isExpired(md.Timestamp) {
		s.purgeAsync(key)
		return nil, nil, ErrCacheExpired
	}

	// Read data file
	dataFilePath := filepath.Join(cacheDir, "data")

	s.fileMu.RLock()
	file, err := os.Open(dataFilePath)
	if err != nil {
		s.fileMu.RUnlock()
		return nil, nil, fmt.Errorf("failed to open data file: %w", err)
	}

	data, err := io.ReadAll(file)
	_ = file.Close()
	s.fileMu.RUnlock()

	if err != nil {
		return nil, nil, fmt.Errorf("failed to read data: %w", err)
	}

	// Decompress if needed
	compressionType := md.HeaderValue("X-Compression-Type")
	if compressionType != "" && compressionType != "none" {
		switch compressionType {
		case "gzip":
			data, err = DecompressGzip(data)
			if err != nil {
				return nil, nil, fmt.Errorf("failed to decompress gzip: %w", err)
			}
		case "br":
			data, err = DecompressBrotli(data)
			if err != nil {
				return nil, nil, fmt.Errorf("failed to decompress brotli: %w", err)
			}
		}
	}

	return data, md, nil
}

// incompressibleContentTypes lists media types whose payload is already compressed,
// so running gzip or brotli over them only burns CPU to produce a larger result.
var incompressibleContentTypes = map[string]bool{
	"application/zip":              true,
	"application/gzip":             true,
	"application/x-gzip":           true,
	"application/x-bzip2":          true,
	"application/x-xz":             true,
	"application/zstd":             true,
	"application/x-7z-compressed":  true,
	"application/x-rar-compressed": true,
	"application/pdf":              true,
	"font/woff":                    true,
	"font/woff2":                   true,
	"application/font-woff":        true,
}

// compressibleImageTypes are the image types that are NOT already compressed and do
// benefit from compression, carved out of the blanket "image/" rule below.
var compressibleImageTypes = map[string]bool{
	"image/svg+xml":            true,
	"image/bmp":                true,
	"image/x-ms-bmp":           true,
	"image/x-icon":             true,
	"image/vnd.microsoft.icon": true,
}

// isIncompressibleContentType reports whether a media type's payload is already
// compressed. video/*, audio/* and image/* are compressed formats as a rule, with a
// small carve-out for the text-based and raw ones.
func isIncompressibleContentType(contentType string) bool {
	ct := strings.ToLower(strings.TrimSpace(contentType))
	if idx := strings.Index(ct, ";"); idx != -1 {
		ct = strings.TrimSpace(ct[:idx])
	}
	if ct == "" {
		return false
	}

	if strings.HasPrefix(ct, "video/") || strings.HasPrefix(ct, "audio/") {
		return true
	}
	if strings.HasPrefix(ct, "image/") {
		return !compressibleImageTypes[ct]
	}

	return incompressibleContentTypes[ct]
}

// compressionBaseline returns the size a subsequent compressor must beat to be worth
// selecting: gzip's output when gzip succeeded, otherwise the original size.
//
// Guarding on gzipErr matters. When CompressGzip returns an error, gzipData is nil and
// len(gzipData) is 0, so comparing a candidate against it directly is unsatisfiable —
// no compressor could ever be selected on that path.
func compressionBaseline(dataSize int, gzipData []byte, gzipErr error) int {
	if gzipErr == nil && len(gzipData) > 0 {
		return len(gzipData)
	}
	return dataSize
}

// compressData compresses data using gzip or brotli if it reduces size.
//
// Compression is skipped entirely for bodies that cannot benefit. Without those
// guards a large media object is run through gzip AND brotli in full on every store,
// allocating a copy per attempt, only for both results to be discarded by the ratio
// check below — pure CPU and allocation cost on exactly the largest objects.
func (s *Storage) compressData(data []byte, md *Metadata) ([]byte, string) {
	dataSize := len(data)

	// Only compress if data is large enough to benefit
	if dataSize < 1024 { // Less than 1KB, don't bother
		return data, ""
	}

	// Above the configured ceiling, store verbatim. Compressing a multi-megabyte
	// body costs more CPU and transient memory than the disk space it saves.
	if s.compressMaxSize > 0 && dataSize > s.compressMaxSize {
		if s.logger != nil {
			s.logger.Debug("Skipping compression: body exceeds compress_max_size",
				zap.Int("size", dataSize),
				zap.Int("limit", s.compressMaxSize))
		}
		return data, ""
	}

	if md != nil {
		// Already compressed on the wire by the origin; recompressing is pointless.
		if ce := md.HeaderValue("Content-Encoding"); ce != "" && ce != "none" && ce != "identity" {
			return data, ""
		}

		if ct := md.HeaderValue("Content-Type"); isIncompressibleContentType(ct) {
			if s.logger != nil {
				s.logger.Debug("Skipping compression: content type is already compressed",
					zap.String("content_type", ct),
					zap.Int("size", dataSize))
			}
			return data, ""
		}
	}

	// Try gzip compression
	gzipData, gzipErr := CompressGzip(data)
	if gzipErr == nil && len(gzipData) < dataSize {
		ratio := float64(len(gzipData)) / float64(dataSize)

		// Only use compression if it saves at least 10%
		if ratio < 0.9 {
			if s.logger != nil {
				s.logger.Debug("Using gzip compression",
					zap.Int("original_size", dataSize),
					zap.Int("compressed_size", len(gzipData)),
					zap.Float64("ratio", ratio))
			}
			return gzipData, "gzip"
		}
	}

	// Try brotli compression for larger files. Note this is only reached when gzip
	// did NOT clear the ratio threshold above (or failed): gzip short-circuits.
	if dataSize > 10240 { // Only for files > 10KB
		baseline := compressionBaseline(dataSize, gzipData, gzipErr)

		brData, err := CompressBrotli(data)
		if err == nil && len(brData) < baseline && len(brData) < dataSize {
			ratio := float64(len(brData)) / float64(dataSize)
			if ratio < 0.9 {
				if s.logger != nil {
					s.logger.Debug("Using brotli compression",
						zap.Int("original_size", dataSize),
						zap.Int("compressed_size", len(brData)),
						zap.Float64("ratio", ratio))
				}
				return brData, "br"
			}
		}
	}

	// No compression beneficial
	return data, ""
}

// humanizeSize formats bytes as human-readable string
func (dc *DiskCache) humanizeSize(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	div, exp := int64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(bytes)/float64(div), "KMGTPE"[exp])
}
