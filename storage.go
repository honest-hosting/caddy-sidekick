package sidekick

import (
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"os"
	"path"
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

	// Mutex for file operations
	fileMu sync.RWMutex
	// Per-key mutexes for granular locking
	keyMutexes   map[string]*sync.RWMutex
	keyMutexesMu sync.Mutex
}

type MemoryCacheItem struct {
	*Metadata
	value []byte
}

const (
	CACHE_DIR = "sidekick-cache"
)

func NewStorage(loc string, ttl int, memMaxSize int, memMaxCount int, logger *zap.Logger) *Storage {
	if err := os.MkdirAll(loc+"/"+CACHE_DIR, 0o755); err != nil {
		logger.Error("Failed to create cache directory", zap.Error(err))
	}

	s := &Storage{
		loc:    loc,
		ttl:    ttl,
		logger: logger,

		memMaxSize:  memMaxSize,
		memMaxCount: memMaxCount,
		keyMutexes:  make(map[string]*sync.RWMutex),
	}
	memCache := NewMemoryCache[string, *MemoryCacheItem](memMaxCount, memMaxSize)
	s.memCache.Store(memCache)

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

	key = strings.ReplaceAll(key, "/", "+")
	ce := meta.contentEncoding

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

	memCache := s.getMemCache()
	if memCache != nil {
		existed := memCache.Put(key+"::"+ce, &MemoryCacheItem{
			Metadata: meta,
			value:    value,
		}, len(value))

		s.logger.Debug("Setting key in cache", zap.String("key", key), zap.String("ce", ce), zap.Bool("replace", existed))
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

	err := os.WriteFile(path.Join(basePath, "."+ce), value, 0o644)
	if err != nil {
		s.logger.Error("Error writing data to cache", zap.Error(err))
		return err
	}

	err = meta.WriteToFile(path.Join(basePath, ".meta"))
	if err != nil {
		s.logger.Error("Error writing meta to cache", zap.Error(err))
		// Try to clean up the data file
		_ = os.Remove(path.Join(basePath, "."+ce))
		return err
	}

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

	for _, f := range files {
		name := f.Name()
		if !strings.HasPrefix(name, key) {
			continue
		}
		fp := path.Join(basePath, name)
		err := os.RemoveAll(fp)
		if err != nil {
			s.logger.Error("Error removing key from disk cache", zap.String("fp", fp), zap.Error(err))
		}
	}
}

func (s *Storage) Flush() error {
	// Replace memory cache
	s.memCache.Store(NewMemoryCache[string, *MemoryCacheItem](s.memMaxCount, s.memMaxSize))

	// Clear all key mutexes
	s.keyMutexesMu.Lock()
	s.keyMutexes = make(map[string]*sync.RWMutex)
	s.keyMutexesMu.Unlock()

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
