package sidekick

import (
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"go.uber.org/zap"
)

// DiskCacheEntry represents an entry in the disk cache index
type DiskCacheEntry struct {
	Key        string
	Path       string
	Size       int64
	AccessTime time.Time
	ModTime    time.Time
}

// loadDiskCacheIndex loads the disk cache index on startup
// This reads the cache directory and rebuilds the index
func (s *Storage) loadDiskCacheIndex() {
	basePath := path.Join(s.loc, CACHE_DIR)

	// Use goroutines to load entries concurrently for speed
	entriesChan := make(chan *DiskCacheEntry, 100)
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
				if entry := s.loadDiskCacheEntry(dirPath); entry != nil {
					entriesChan <- entry
				}
			}
		}()
	}

	// Scan directories
	go func() {
		filepath.Walk(basePath, func(dirPath string, info os.FileInfo, err error) error { //nolint:errcheck
			if err != nil || !info.IsDir() || dirPath == basePath {
				return nil
			}

			// Check if this is a cache directory (direct child of base path)
			if filepath.Dir(dirPath) == basePath {
				dirChan <- dirPath
			}
			return filepath.SkipDir // Don't recurse into subdirectories
		})
		close(dirChan)
	}()

	// Collector goroutine
	go func() {
		wg.Wait()
		close(entriesChan)
	}()

	// Collect all entries
	var entries []*DiskCacheEntry
	var totalSize int64
	for entry := range entriesChan {
		entries = append(entries, entry)
		totalSize += entry.Size
	}

	// Sort by access time for LRU
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].AccessTime.Before(entries[j].AccessTime)
	})

	// Update the index
	s.diskIndexMu.Lock()
	s.diskIndex = make(map[string]*DiskCacheEntry, len(entries))
	s.diskLRU = entries
	for _, entry := range entries {
		s.diskIndex[entry.Key] = entry
	}
	s.diskIndexMu.Unlock()

	// Update usage stats
	s.diskUsageMu.Lock()
	s.diskUsage = totalSize
	s.diskItemCount = int64(len(entries))
	s.diskUsageMu.Unlock()

	if s.logger != nil {
		s.logger.Info("Disk cache index loaded",
			zap.Int("items", len(entries)),
			zap.String("usage", s.humanizeSize(totalSize)))
	}
}

// loadDiskCacheEntry loads a single disk cache entry
func (s *Storage) loadDiskCacheEntry(dirPath string) *DiskCacheEntry {
	key := filepath.Base(dirPath)

	info, err := os.Stat(dirPath)
	if err != nil {
		return nil
	}

	// Calculate directory size
	var size int64
	filepath.Walk(dirPath, func(path string, info os.FileInfo, err error) error { //nolint:errcheck
		if err == nil && !info.IsDir() {
			size += info.Size()
		}
		return nil
	})

	// Get metadata for access time if available
	metadataPath := filepath.Join(dirPath, "metadata.json")
	metadata := &Metadata{}
	if err := metadata.LoadFromFile(metadataPath); err == nil && metadata.Timestamp > 0 {
		return &DiskCacheEntry{
			Key:        key,
			Path:       dirPath,
			Size:       size,
			AccessTime: time.Unix(metadata.Timestamp, 0),
			ModTime:    info.ModTime(),
		}
	}

	return &DiskCacheEntry{
		Key:        key,
		Path:       dirPath,
		Size:       size,
		AccessTime: info.ModTime(), // Fall back to mod time
		ModTime:    info.ModTime(),
	}
}

// updateDiskIndex updates the disk cache index when a new item is added
func (s *Storage) updateDiskIndex(key string, size int64, isNew bool) {
	s.diskIndexMu.Lock()
	defer s.diskIndexMu.Unlock()

	now := time.Now()
	oldSize := int64(0)
	exists := false

	if entry, ok := s.diskIndex[key]; ok {
		// Update existing entry
		exists = true
		oldSize = entry.Size
		entry.Size = size
		entry.AccessTime = now
		entry.ModTime = now
		s.moveDiskEntryToEnd(entry)
	} else {
		// Add new entry
		entry := &DiskCacheEntry{
			Key:        key,
			Path:       path.Join(s.loc, CACHE_DIR, key),
			Size:       size,
			AccessTime: now,
			ModTime:    now,
		}
		s.diskIndex[key] = entry
		s.diskLRU = append(s.diskLRU, entry)
	}

	// Update usage stats
	s.diskUsageMu.Lock()
	if isNew && !exists {
		s.diskItemCount++
		s.diskUsage += size
	} else if exists {
		s.diskUsage = s.diskUsage - oldSize + size
	}
	s.diskUsageMu.Unlock()
}

// moveDiskEntryToEnd moves a disk cache entry to the end of the LRU list (most recently used)
func (s *Storage) moveDiskEntryToEnd(entry *DiskCacheEntry) {
	for i, e := range s.diskLRU {
		if e == entry {
			// Remove from current position
			s.diskLRU = append(s.diskLRU[:i], s.diskLRU[i+1:]...)
			// Add to end
			s.diskLRU = append(s.diskLRU, entry)
			break
		}
	}
}

// touchDiskEntry updates the access time of a disk cache entry
func (s *Storage) touchDiskEntry(key string) {
	s.diskIndexMu.Lock()
	defer s.diskIndexMu.Unlock()

	if entry, exists := s.diskIndex[key]; exists {
		entry.AccessTime = time.Now()
		s.moveDiskEntryToEnd(entry)
	}
}

// removeDiskEntry removes an entry from the disk cache index
func (s *Storage) removeDiskEntry(key string) {
	s.diskIndexMu.Lock()
	defer s.diskIndexMu.Unlock()

	if entry, exists := s.diskIndex[key]; exists {
		delete(s.diskIndex, key)

		// Remove from LRU list
		for i, e := range s.diskLRU {
			if e == entry {
				s.diskLRU = append(s.diskLRU[:i], s.diskLRU[i+1:]...)
				break
			}
		}

		// Update usage stats
		s.diskUsageMu.Lock()
		s.diskUsage -= entry.Size
		s.diskItemCount--
		s.diskUsageMu.Unlock()
	}
}

// evictOldestFromDisk evicts the oldest items from disk to make space
func (s *Storage) evictOldestFromDisk(neededSpace int64, reason string) {
	s.diskIndexMu.Lock()
	defer s.diskIndexMu.Unlock()

	// Get current usage with proper locking
	s.diskUsageMu.RLock()
	currentUsage := s.diskUsage
	currentCount := s.diskItemCount
	s.diskUsageMu.RUnlock()

	// Determine eviction targets based on reason
	var targetUsage int64
	var targetCount int64

	switch reason {
	case "size":
		targetUsage = int64(s.diskMaxSize) - neededSpace
		targetCount = currentCount // Don't change count limit
	case "count":
		targetUsage = currentUsage // Don't change size limit
		targetCount = int64(s.diskMaxCount) - 1
	default:
		// General eviction - respect both limits
		targetUsage = int64(s.diskMaxSize) - neededSpace
		if targetUsage < 0 {
			targetUsage = 0
		}
		targetCount = int64(s.diskMaxCount)
		if currentCount >= targetCount {
			targetCount = int64(s.diskMaxCount) - 1
		}
	}

	// Track what we're evicting
	var evictedSize int64
	var evictedCount int64
	var evictedKeys []string

	// Evict oldest entries until we meet our targets
	// We iterate from the beginning since diskLRU is sorted by access time (oldest first)
	for i := 0; i < len(s.diskLRU); i++ {
		// Check if we've evicted enough
		remainingUsage := currentUsage - evictedSize
		remainingCount := currentCount - evictedCount

		if remainingUsage <= targetUsage && remainingCount <= targetCount {
			break
		}

		entry := s.diskLRU[i]
		evictedKeys = append(evictedKeys, entry.Key)
		evictedSize += entry.Size
		evictedCount++
	}

	// Actually remove the evicted entries
	for _, key := range evictedKeys {
		if entry, exists := s.diskIndex[key]; exists {
			// Remove from index
			delete(s.diskIndex, key)

			// Remove the actual files
			cachePath := entry.Path
			s.fileMu.Lock()
			if err := os.RemoveAll(cachePath); err != nil && s.logger != nil {
				s.logger.Warn("Failed to remove cache directory during eviction",
					zap.String("key", key),
					zap.String("path", cachePath),
					zap.Error(err))
			}
			s.fileMu.Unlock()
		}
	}

	// Update LRU list - remove evicted entries
	newLRU := make([]*DiskCacheEntry, 0, len(s.diskLRU)-len(evictedKeys))
	for _, entry := range s.diskLRU {
		if _, evicted := s.diskIndex[entry.Key]; evicted {
			newLRU = append(newLRU, entry)
		}
	}
	s.diskLRU = newLRU

	// Update usage stats
	s.diskUsageMu.Lock()
	s.diskUsage -= evictedSize
	s.diskItemCount -= evictedCount
	s.diskUsageMu.Unlock()

	if s.logger != nil && evictedCount > 0 {
		s.logger.Info("Evicted items from disk cache",
			zap.String("reason", reason),
			zap.Int64("evicted_count", evictedCount),
			zap.String("evicted_size", s.humanizeSize(evictedSize)),
			zap.String("remaining_size", s.humanizeSize(s.diskUsage)),
			zap.Int64("remaining_count", s.diskItemCount))
	}
}

// storeData stores data to disk with compression
func (s *Storage) storeData(cacheDir, dataFilePath string, data []byte, md *Metadata) error {
	// Create cache directory
	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		return fmt.Errorf("failed to create cache directory: %w", err)
	}

	// Compress data if beneficial
	compressedData, compressionType := s.compressData(data)

	// Update metadata with compression info
	if compressionType != "" {
		// Store compression type in Header field since contentEncoding is private
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

	// Calculate actual size on disk
	var totalSize int64
	filepath.Walk(cacheDir, func(path string, info os.FileInfo, err error) error { //nolint:errcheck
		if err == nil && !info.IsDir() {
			totalSize += info.Size()
		}
		return nil
	})

	return nil
}

// compressData compresses data using gzip or brotli if it reduces size
func (s *Storage) compressData(data []byte) ([]byte, string) {
	dataSize := len(data)

	// Only compress if data is large enough to benefit
	if dataSize < 1024 { // Less than 1KB, don't bother
		return data, ""
	}

	// Try gzip compression
	gzipData, err := CompressGzip(data)
	if err == nil && len(gzipData) < dataSize {
		// Calculate compression ratio
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

	// Try brotli compression for larger files
	if dataSize > 10240 { // Only for files > 10KB
		brData, err := CompressBrotli(data)
		if err == nil && len(brData) < len(gzipData) && len(brData) < dataSize {
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

// Helper function to read disk cache data
func (s *Storage) readDiskCache(key, cacheDir string) ([]byte, *Metadata, error) {
	// Load metadata
	metadataPath := filepath.Join(cacheDir, "metadata.json")
	md := &Metadata{}
	if err := md.LoadFromFile(metadataPath); err != nil {
		return nil, nil, fmt.Errorf("failed to load metadata: %w", err)
	}

	// Check TTL
	if s.ttl > 0 && md.Timestamp > 0 {
		if time.Since(time.Unix(md.Timestamp, 0)) > time.Duration(s.ttl)*time.Second {
			// Cache expired, clean it up asynchronously
			s.asyncOps.Add(1)
			go func() {
				defer s.asyncOps.Done()
				_ = s.Purge(key)
			}()
			return nil, nil, ErrCacheExpired
		}
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

	// Decompress if needed - check Header for compression type
	var compressionType string
	for _, header := range md.Header {
		if len(header) >= 2 && header[0] == "X-Compression-Type" {
			compressionType = header[1]
			break
		}
	}

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

	// Update access time and move to end of LRU
	s.touchDiskEntry(key)

	return data, md, nil
}

// Helper function to humanize size for logging
func (s *Storage) humanizeSize(bytes int64) string {
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
