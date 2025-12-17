package sidekick

import (
	"bytes"
	"crypto/md5"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"github.com/dustin/go-humanize"
	"go.uber.org/zap"
)

type Sidekick struct {
	logger             *zap.Logger
	CacheDir           string   `json:"cache_dir,omitempty"`
	PurgeURI           string   `json:"purge_uri,omitempty"`
	PurgeHeader        string   `json:"purge_header,omitempty"`
	PurgeToken         string   `json:"purge_token,omitempty"`
	NoCache            []string `json:"nocache,omitempty"`       // Path prefixes to bypass
	NoCacheRegex       string   `json:"nocache_regex,omitempty"` // Regex pattern to bypass
	NoCacheHome        bool     `json:"nocache_home,omitempty"`  // Whether to skip caching home page
	CacheResponseCodes []string `json:"cache_response_codes,omitempty"`
	CacheTTL           int      `json:"cache_ttl,omitempty"` // TTL in seconds
	Storage            *Storage

	// Size configurations (stored as int64 for byte values)
	CacheMemoryItemMaxSize      int64 `json:"cache_memory_item_max_size,omitempty"`       // Max size for single item in memory
	CacheMemoryMaxSize          int64 `json:"cache_memory_max_size,omitempty"`            // Total memory cache size limit
	CacheMemoryMaxPercent       int   `json:"cache_memory_max_percent,omitempty"`         // Memory cache as percent of available RAM (1-100)
	CacheMemoryMaxCount         int   `json:"cache_memory_max_count,omitempty"`           // Max number of items in memory
	CacheMemoryStreamToDiskSize int64 `json:"cache_memory_stream_to_disk_size,omitempty"` // Threshold to stream to disk
	CacheDiskItemMaxSize        int64 `json:"cache_disk_item_max_size,omitempty"`         // Max size for any cached item on disk
	CacheDiskMaxSize            int64 `json:"cache_disk_max_size,omitempty"`              // Total disk cache size limit
	CacheDiskMaxPercent         int   `json:"cache_disk_max_percent,omitempty"`           // Disk cache as percent of available space (1-100)
	CacheDiskMaxCount           int   `json:"cache_disk_max_count,omitempty"`             // Max number of items on disk

	// Cache key configuration
	CacheKeyHeaders []string `json:"cache_key_headers,omitempty"`
	CacheKeyQueries []string `json:"cache_key_queries,omitempty"`
	CacheKeyCookies []string `json:"cache_key_cookies,omitempty"`

	pathRx           *regexp.Regexp
	bypassDebugQuery string // Internal field for debug query bypass

	// Synchronization handler (initialized during Provision)
	syncHandler *SyncHandler

	// Buffer pool for response buffering
	bufferPool *sync.Pool
}

// SyncHandler manages synchronization for cache operations
type SyncHandler struct {
	// Mutex for cache operations
	cacheMu sync.RWMutex
	// Track in-flight cache operations per key to prevent duplicates
	inFlight   map[string]*sync.Once
	inFlightMu sync.Mutex
}

func init() {
	caddy.RegisterModule(Sidekick{})
	httpcaddyfile.RegisterHandlerDirective("sidekick", parseCaddyfileHandler)
}

func parseCaddyfileHandler(h httpcaddyfile.Helper) (caddyhttp.MiddlewareHandler,
	error) {
	s := new(Sidekick)
	if err := s.UnmarshalCaddyfile(h.Dispenser); err != nil {
		return nil, err
	}

	return s, nil
}

// parseSize parses human-readable size strings like "4MB", "1.5GB", etc.
// Returns -1 for unlimited, 0 for disabled, or the size in bytes
func parseSize(value string) (int64, error) {
	value = strings.TrimSpace(value)

	// Check for special values
	if value == "-1" || strings.ToLower(value) == "unlimited" {
		return -1, nil
	}
	if value == "0" || strings.ToLower(value) == "disabled" {
		return 0, nil
	}

	// Try parsing as plain number first (assume bytes)
	if n, err := strconv.ParseInt(value, 10, 64); err == nil {
		return n, nil
	}

	// Parse human-readable format
	bytes, err := humanize.ParseBytes(value)
	if err != nil {
		return 0, fmt.Errorf("invalid size format: %s", value)
	}
	return int64(bytes), nil
}

// parsePercent parses percentage values (0-100, or -1 for unlimited)
func parsePercent(value string) (int, error) {
	value = strings.TrimSpace(value)

	// Check for special values
	if value == "-1" || strings.ToLower(value) == "unlimited" {
		return -1, nil
	}
	if value == "0" || strings.ToLower(value) == "disabled" {
		return 0, nil
	}

	// Remove % suffix if present
	value = strings.TrimSuffix(value, "%")

	// Parse as integer
	n, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("invalid percentage format: %s", value)
	}

	if n < 0 || n > 100 {
		return 0, fmt.Errorf("percentage must be between 0-100 or -1 for unlimited")
	}

	return n, nil
}

func (s *Sidekick) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	for d.Next() {
		for d.NextBlock(0) {
			key := d.Val()

			if !d.NextArg() {
				return d.ArgErr()
			}
			value := d.Val()

			switch key {
			case "cache_dir":
				s.CacheDir = value

			case "nocache":
				// Can be comma-separated or multiple arguments
				prefixes := strings.Split(value, ",")
				for d.NextArg() {
					prefixes = append(prefixes, strings.Split(d.Val(), ",")...)
				}
				for i := range prefixes {
					prefixes[i] = strings.TrimSpace(prefixes[i])
				}
				s.NoCache = prefixes

			case "nocache_regex":
				value = strings.TrimSpace(value)
				if len(value) != 0 {
					_, err := regexp.Compile(value)
					if err != nil {
						return err
					}
				}
				s.NoCacheRegex = value

			case "nocache_home":
				b, err := strconv.ParseBool(value)
				if err != nil {
					return d.Errf("nocache_home must be true or false")
				}
				s.NoCacheHome = b

			case "cache_response_codes":
				codes := strings.Split(value, ",")
				for d.NextArg() {
					codes = append(codes, strings.Split(d.Val(), ",")...)
				}
				s.CacheResponseCodes = make([]string, len(codes))
				for i, code := range codes {
					code = strings.TrimSpace(code)
					if strings.Contains(code, "XX") {
						code = string(code[0])
					}
					s.CacheResponseCodes[i] = code
				}

			case "cache_ttl":
				ttl, err := strconv.Atoi(value)
				if err != nil {
					return d.Errf("invalid cache_ttl value: %v", err)
				}
				s.CacheTTL = ttl

			case "purge_uri":
				// Validate that it's an absolute path with only allowed chars
				if !strings.HasPrefix(value, "/") {
					return d.Errf("purge_uri must be an absolute path starting with /")
				}
				purgeURIRegex := regexp.MustCompile(`^/[a-z0-9\-/_]*$`)
				if !purgeURIRegex.MatchString(value) {
					return d.Errf("purge_uri can only contain lowercase letters, numbers, hyphens, underscores, and forward slashes")
				}
				s.PurgeURI = value

			case "purge_header":
				s.PurgeHeader = strings.TrimSpace(value)

			case "purge_token":
				s.PurgeToken = strings.TrimSpace(value)

			case "cache_memory_item_max_size":
				size, err := parseSize(value)
				if err != nil {
					return d.Errf("invalid cache_memory_item_max_size: %v", err)
				}
				s.CacheMemoryItemMaxSize = size

			case "cache_memory_max_size":
				size, err := parseSize(value)
				if err != nil {
					return d.Errf("invalid cache_memory_max_size: %v", err)
				}
				s.CacheMemoryMaxSize = size

			case "cache_memory_max_percent":
				percent, err := parsePercent(value)
				if err != nil {
					return d.Errf("invalid cache_memory_max_percent: %v", err)
				}
				s.CacheMemoryMaxPercent = percent

			case "cache_memory_max_count":
				count, err := strconv.Atoi(value)
				if err != nil {
					return d.Errf("invalid cache_memory_max_count: %v", err)
				}
				s.CacheMemoryMaxCount = count

			case "cache_disk_item_max_size":
				size, err := parseSize(value)
				if err != nil {
					return d.Errf("invalid cache_disk_item_max_size: %v", err)
				}
				s.CacheDiskItemMaxSize = size

			case "cache_disk_max_size":
				size, err := parseSize(value)
				if err != nil {
					return d.Errf("invalid cache_disk_max_size: %v", err)
				}
				s.CacheDiskMaxSize = size

			case "cache_disk_max_percent":
				percent, err := parsePercent(value)
				if err != nil {
					return d.Errf("invalid cache_disk_max_percent: %v", err)
				}
				s.CacheDiskMaxPercent = percent

			case "cache_disk_max_count":
				count, err := strconv.Atoi(value)
				if err != nil {
					return d.Errf("invalid cache_disk_max_count: %v", err)
				}
				s.CacheDiskMaxCount = count

			case "cache_memory_stream_to_disk_size":
				size, err := parseSize(value)
				if err != nil {
					return d.Errf("invalid cache_memory_stream_to_disk_size: %v", err)
				}
				s.CacheMemoryStreamToDiskSize = size

			case "cache_key_headers":
				headers := strings.Split(value, ",")
				for d.NextArg() {
					headers = append(headers, strings.Split(d.Val(), ",")...)
				}
				for i := range headers {
					headers[i] = strings.TrimSpace(headers[i])
				}
				s.CacheKeyHeaders = headers

			case "cache_key_queries":
				queries := strings.Split(value, ",")
				for d.NextArg() {
					queries = append(queries, strings.Split(d.Val(), ",")...)
				}
				for i := range queries {
					queries[i] = strings.TrimSpace(queries[i])
				}
				s.CacheKeyQueries = queries

			case "cache_key_cookies":
				cookies := strings.Split(value, ",")
				for d.NextArg() {
					cookies = append(cookies, strings.Split(d.Val(), ",")...)
				}
				for i := range cookies {
					cookies[i] = strings.TrimSpace(cookies[i])
				}
				s.CacheKeyCookies = cookies

			default:
				return d.Errf("unknown subdirective: %s", key)
			}
		}
	}

	return nil
}

// Constants for default values
const (
	DefaultCacheDir            = "/var/www/html/wp-content/cache"
	DefaultMemoryItemMaxSize   = 4 * 1024 * 1024   // 4MB
	DefaultMemoryCacheMaxSize  = 128 * 1024 * 1024 // 128MB
	DefaultMemoryCacheMaxCount = 32 * 1024         // 32K items
	DefaultBypassDebugQuery    = "sidekick-nocache"
	DefaultPurgeURI            = "/__sidekick/purge"
	DefaultPurgeHeader         = "X-Sidekick-Purge"
	DefaultPurgeToken          = "dead-beef"
	CacheHeaderName            = "X-Sidekick-Cache" // Not configurable
	DefaultNoCacheRegex        = `\.(jpg|jpeg|png|gif|ico|css|js|svg|woff|woff2|ttf|eot|otf|mp4|webm|mp3|ogg|wav|pdf|zip|tar|gz|7z|exe|doc|docx|xls|xlsx|ppt|pptx)$`
	DefaultTTL                 = 6000
	DefaultDiskItemMaxSize     = 100 * 1024 * 1024       // 100MB
	DefaultDiskMaxSize         = 10 * 1024 * 1024 * 1024 // 10GB
	DefaultDiskMaxCount        = 100000                  // 100K items on disk
	DefaultStreamToDiskSize    = 10 * 1024 * 1024        // 10MB
	DefaultBufferSize          = 32 * 1024               // 32KB buffer size
)

func (s *Sidekick) Provision(ctx caddy.Context) error {
	s.logger = ctx.Logger(s)
	s.syncHandler = &SyncHandler{
		inFlight: make(map[string]*sync.Once),
	}

	// Validate mutually exclusive options
	if s.CacheMemoryMaxSize != 0 && s.CacheMemoryMaxPercent != 0 {
		return fmt.Errorf("cache_memory_max_size and cache_memory_max_percent are mutually exclusive")
	}
	if s.CacheDiskMaxSize != 0 && s.CacheDiskMaxPercent != 0 {
		return fmt.Errorf("cache_disk_max_size and cache_disk_max_percent are mutually exclusive")
	}

	// Initialize buffer pool
	s.bufferPool = &sync.Pool{
		New: func() interface{} {
			return bytes.NewBuffer(make([]byte, 0, DefaultBufferSize))
		},
	}

	// Load from environment variables with SIDEKICK_ prefix
	if s.CacheDir == "" {
		s.CacheDir = os.Getenv("SIDEKICK_CACHE_DIR")
		if s.CacheDir == "" {
			s.CacheDir = DefaultCacheDir
		}
	}

	if s.CacheResponseCodes == nil {
		codes := os.Getenv("SIDEKICK_CACHE_RESPONSE_CODES")
		if codes == "" {
			codes = "200,404,405"
		}
		codeList := strings.Split(codes, ",")
		s.CacheResponseCodes = make([]string, len(codeList))

		for i, code := range codeList {
			code = strings.TrimSpace(code)
			if strings.Contains(code, "XX") {
				code = string(code[0])
			}
			s.CacheResponseCodes[i] = code
		}
	}

	if s.NoCache == nil {
		prefixes := os.Getenv("SIDEKICK_NOCACHE")
		if prefixes == "" {
			prefixes = "/wp-admin,/wp-json"
		}
		s.NoCache = strings.Split(strings.TrimSpace(prefixes), ",")
		for i := range s.NoCache {
			s.NoCache[i] = strings.TrimSpace(s.NoCache[i])
		}
	}

	if s.NoCacheRegex == "" {
		s.NoCacheRegex = os.Getenv("SIDEKICK_NOCACHE_REGEX")
		if s.NoCacheRegex == "" {
			s.NoCacheRegex = DefaultNoCacheRegex
		}
	}
	if s.NoCacheRegex != "" {
		rx, err := regexp.Compile(s.NoCacheRegex)
		if err != nil {
			return fmt.Errorf("invalid nocache_regex pattern: %v", err)
		}
		s.pathRx = rx
	}

	if !s.NoCacheHome {
		if strings.ToLower(os.Getenv("SIDEKICK_NOCACHE_HOME")) == "true" {
			s.NoCacheHome = true
		}
	}

	// Internal bypass debug query
	s.bypassDebugQuery = DefaultBypassDebugQuery

	if s.CacheTTL == 0 {
		ttl := os.Getenv("SIDEKICK_CACHE_TTL")
		if ttl != "" {
			if n, err := strconv.Atoi(ttl); err == nil && n > 0 {
				s.CacheTTL = n
			}
		}
		if s.CacheTTL == 0 {
			s.CacheTTL = DefaultTTL
		}
	}

	if s.PurgeURI == "" {
		s.PurgeURI = os.Getenv("SIDEKICK_PURGE_URI")
		if s.PurgeURI == "" {
			s.PurgeURI = DefaultPurgeURI
		}
	}
	// Validate PurgeURI format
	if !strings.HasPrefix(s.PurgeURI, "/") {
		return fmt.Errorf("purge_uri must be an absolute path starting with /")
	}
	if matched, _ := regexp.MatchString(`^/[a-z0-9\-/_]*$`, s.PurgeURI); !matched {
		return fmt.Errorf("purge_uri can only contain lowercase letters, numbers, hyphens, underscores, and forward slashes")
	}

	if s.PurgeHeader == "" {
		s.PurgeHeader = os.Getenv("SIDEKICK_PURGE_HEADER")
		if s.PurgeHeader == "" {
			s.PurgeHeader = DefaultPurgeHeader
		}
	}

	if s.PurgeToken == "" {
		s.PurgeToken = os.Getenv("SIDEKICK_PURGE_TOKEN")
		if s.PurgeToken == "" {
			s.PurgeToken = DefaultPurgeToken
		}
	}

	// Parse size configurations from environment if not set
	if s.CacheMemoryItemMaxSize == 0 {
		if envVal := os.Getenv("SIDEKICK_CACHE_MEMORY_ITEM_MAX_SIZE"); envVal != "" {
			if size, err := parseSize(envVal); err == nil {
				s.CacheMemoryItemMaxSize = size
				s.logger.Debug("Memory item max size from environment",
					zap.String("env_value", envVal),
					zap.String("size", humanizeBytes(size)),
					zap.Int64("bytes", size))
			}
		}
		if s.CacheMemoryItemMaxSize == 0 {
			s.CacheMemoryItemMaxSize = DefaultMemoryItemMaxSize
			s.logger.Debug("Using default memory item max size",
				zap.String("size", humanizeBytes(DefaultMemoryItemMaxSize)),
				zap.Int64("bytes", DefaultMemoryItemMaxSize))
		}
	} else if s.CacheMemoryItemMaxSize > 0 {
		s.logger.Debug("Memory item max size configured",
			zap.String("size", humanizeBytes(s.CacheMemoryItemMaxSize)),
			zap.Int64("bytes", s.CacheMemoryItemMaxSize))
	}

	if s.CacheMemoryMaxSize == 0 {
		if envVal := os.Getenv("SIDEKICK_CACHE_MEMORY_MAX_SIZE"); envVal != "" {
			if size, err := parseSize(envVal); err == nil {
				s.CacheMemoryMaxSize = size
			}
		}
		if s.CacheMemoryMaxSize == 0 {
			s.CacheMemoryMaxSize = DefaultMemoryCacheMaxSize
		}
	}

	if s.CacheMemoryMaxPercent == 0 {
		if envVal := os.Getenv("SIDEKICK_CACHE_MEMORY_MAX_PERCENT"); envVal != "" {
			if percent, err := parsePercent(envVal); err == nil {
				s.CacheMemoryMaxPercent = percent
			}
		}
	}

	// Re-validate after loading from environment
	if s.CacheMemoryMaxSize != 0 && s.CacheMemoryMaxPercent != 0 {
		return fmt.Errorf("SIDEKICK_CACHE_MEMORY_MAX_SIZE and SIDEKICK_CACHE_MEMORY_MAX_PERCENT are mutually exclusive")
	}

	if s.CacheMemoryMaxCount == 0 {
		if envVal := os.Getenv("SIDEKICK_CACHE_MEMORY_MAX_COUNT"); envVal != "" {
			if n, err := strconv.Atoi(envVal); err == nil {
				s.CacheMemoryMaxCount = n
			}
		}
		if s.CacheMemoryMaxCount == 0 {
			s.CacheMemoryMaxCount = DefaultMemoryCacheMaxCount
		}
	}

	if s.CacheDiskItemMaxSize == 0 {
		if envVal := os.Getenv("SIDEKICK_CACHE_DISK_ITEM_MAX_SIZE"); envVal != "" {
			if size, err := parseSize(envVal); err == nil {
				s.CacheDiskItemMaxSize = size
			}
		}
		if s.CacheDiskItemMaxSize == 0 {
			s.CacheDiskItemMaxSize = DefaultDiskItemMaxSize
		}
	}

	if s.CacheDiskMaxSize == 0 {
		if envVal := os.Getenv("SIDEKICK_CACHE_DISK_MAX_SIZE"); envVal != "" {
			if size, err := parseSize(envVal); err == nil {
				s.CacheDiskMaxSize = size
			}
		}
		if s.CacheDiskMaxSize == 0 && s.CacheDiskMaxPercent == 0 {
			s.CacheDiskMaxSize = DefaultDiskMaxSize
		}
	}

	if s.CacheDiskMaxPercent == 0 {
		if envVal := os.Getenv("SIDEKICK_CACHE_DISK_MAX_PERCENT"); envVal != "" {
			if percent, err := parsePercent(envVal); err == nil {
				s.CacheDiskMaxPercent = percent
			}
		}
	}

	// Re-validate disk options after environment loading
	if s.CacheDiskMaxSize != 0 && s.CacheDiskMaxPercent != 0 {
		return fmt.Errorf("SIDEKICK_CACHE_DISK_MAX_SIZE and SIDEKICK_CACHE_DISK_MAX_PERCENT are mutually exclusive")
	}

	if s.CacheDiskMaxCount == 0 {
		if envVal := os.Getenv("SIDEKICK_CACHE_DISK_MAX_COUNT"); envVal != "" {
			if n, err := strconv.Atoi(envVal); err == nil {
				s.CacheDiskMaxCount = n
				s.logger.Debug("Disk max count from environment",
					zap.String("env_value", envVal),
					zap.Int("count", n))
			}
		}
		if s.CacheDiskMaxCount == 0 {
			s.CacheDiskMaxCount = DefaultDiskMaxCount
			s.logger.Debug("Using default disk max count", zap.Int("count", DefaultDiskMaxCount))
		}
	} else if s.CacheDiskMaxCount != 0 {
		s.logger.Debug("Disk max count configured", zap.Int("count", s.CacheDiskMaxCount))
	}

	if s.CacheMemoryStreamToDiskSize == 0 {
		if envVal := os.Getenv("SIDEKICK_CACHE_MEMORY_STREAM_TO_DISK_SIZE"); envVal != "" {
			if size, err := parseSize(envVal); err == nil {
				s.CacheMemoryStreamToDiskSize = size
				s.logger.Debug("Memory stream to disk size from environment",
					zap.String("env_value", envVal),
					zap.String("size", humanizeBytes(size)),
					zap.Int64("bytes", size))
			}
		}
		if s.CacheMemoryStreamToDiskSize == 0 {
			s.CacheMemoryStreamToDiskSize = DefaultStreamToDiskSize
			s.logger.Debug("Using default stream to disk size",
				zap.String("size", humanizeBytes(DefaultStreamToDiskSize)),
				zap.Int64("bytes", DefaultStreamToDiskSize))
		}
	} else if s.CacheMemoryStreamToDiskSize > 0 {
		s.logger.Debug("Memory stream to disk size configured",
			zap.String("size", humanizeBytes(s.CacheMemoryStreamToDiskSize)),
			zap.Int64("bytes", s.CacheMemoryStreamToDiskSize))
	}

	// Load cache key configuration from environment
	if len(s.CacheKeyHeaders) == 0 {
		if envVal := os.Getenv("SIDEKICK_CACHE_KEY_HEADERS"); envVal != "" {
			s.CacheKeyHeaders = strings.Split(envVal, ",")
			for i := range s.CacheKeyHeaders {
				s.CacheKeyHeaders[i] = strings.TrimSpace(s.CacheKeyHeaders[i])
			}
		}
	}

	if len(s.CacheKeyQueries) == 0 {
		if envVal := os.Getenv("SIDEKICK_CACHE_KEY_QUERIES"); envVal != "" {
			s.CacheKeyQueries = strings.Split(envVal, ",")
			for i := range s.CacheKeyQueries {
				s.CacheKeyQueries[i] = strings.TrimSpace(s.CacheKeyQueries[i])
			}
		}
	}

	if len(s.CacheKeyCookies) == 0 {
		if envVal := os.Getenv("SIDEKICK_CACHE_KEY_COOKIES"); envVal != "" {
			s.CacheKeyCookies = strings.Split(envVal, ",")
			for i := range s.CacheKeyCookies {
				s.CacheKeyCookies[i] = strings.TrimSpace(s.CacheKeyCookies[i])
			}
		}
	}

	// Convert size values for storage initialization
	memMaxSize := int(s.CacheMemoryMaxSize)
	if s.CacheMemoryMaxSize < 0 {
		memMaxSize = -1 // Unlimited
		s.logger.Debug("Memory cache size set to unlimited")
	} else if s.CacheMemoryMaxSize == 0 {
		memMaxSize = 0 // Disabled
		s.logger.Debug("Memory cache disabled")
	} else {
		s.logger.Debug("Memory cache size configured",
			zap.String("size", humanizeBytes(s.CacheMemoryMaxSize)),
			zap.Int64("bytes", s.CacheMemoryMaxSize))
	}

	memMaxCount := s.CacheMemoryMaxCount
	if memMaxCount < 0 {
		memMaxCount = -1 // Unlimited
		s.logger.Debug("Memory cache item count set to unlimited")
	} else if memMaxCount == 0 {
		s.logger.Debug("Memory cache item count disabled")
	} else {
		s.logger.Debug("Memory cache item count configured", zap.Int("count", memMaxCount))
	}

	// Calculate actual memory size if percentage is used
	if s.CacheMemoryMaxPercent > 0 {
		totalMem := getTotalMemory()
		availMem := getAvailableMemory()
		memMaxSize = int(totalMem * int64(s.CacheMemoryMaxPercent) / 100)

		s.logger.Debug("Memory cache size calculated from percentage",
			zap.Int("percent", s.CacheMemoryMaxPercent),
			zap.String("total_memory", humanizeBytes(totalMem)),
			zap.String("available_memory", humanizeBytes(availMem)),
			zap.String("cache_size", humanizeBytes(int64(memMaxSize))),
			zap.Int64("cache_size_bytes", int64(memMaxSize)))

		// Warn if cache size exceeds available memory
		if int64(memMaxSize) > availMem {
			s.logger.Warn("Memory cache size exceeds available memory",
				zap.String("cache_size", humanizeBytes(int64(memMaxSize))),
				zap.String("available", humanizeBytes(availMem)),
				zap.String("recommendation", "Consider reducing cache_memory_max_percent"))
		}
	} else if s.CacheMemoryMaxSize > 0 {
		// Check fixed size against available memory
		availMem := getAvailableMemory()
		if s.CacheMemoryMaxSize > availMem {
			s.logger.Warn("Memory cache size exceeds available memory",
				zap.String("cache_size", humanizeBytes(s.CacheMemoryMaxSize)),
				zap.String("available", humanizeBytes(availMem)),
				zap.String("recommendation", "Consider reducing cache_memory_max_size"))
		}
	}

	// Calculate actual disk size if percentage is used
	diskMaxSize := int(s.CacheDiskMaxSize)
	if s.CacheDiskMaxPercent > 0 {
		totalDisk := getTotalDiskSpace(s.CacheDir)
		freeDisk := getFreeDiskSpace(s.CacheDir)
		diskMaxSize = int(totalDisk * int64(s.CacheDiskMaxPercent) / 100)

		s.logger.Debug("Disk cache size calculated from percentage",
			zap.Int("percent", s.CacheDiskMaxPercent),
			zap.String("total_disk", humanizeBytes(totalDisk)),
			zap.String("free_disk", humanizeBytes(freeDisk)),
			zap.String("cache_size", humanizeBytes(int64(diskMaxSize))),
			zap.Int64("cache_size_bytes", int64(diskMaxSize)))

		// Warn if cache size exceeds free disk space
		if int64(diskMaxSize) > freeDisk {
			s.logger.Warn("Disk cache size exceeds available disk space",
				zap.String("cache_size", humanizeBytes(int64(diskMaxSize))),
				zap.String("free_space", humanizeBytes(freeDisk)),
				zap.String("recommendation", "Consider reducing cache_disk_max_percent"))
		}
	} else if s.CacheDiskMaxSize < 0 {
		diskMaxSize = -1 // Unlimited
		s.logger.Debug("Disk cache size set to unlimited")
	} else if s.CacheDiskMaxSize == 0 {
		diskMaxSize = 0 // Disabled
		s.logger.Debug("Disk cache disabled")
	} else {
		// Check fixed size against free disk space
		freeDisk := getFreeDiskSpace(s.CacheDir)
		s.logger.Debug("Disk cache size configured",
			zap.String("size", humanizeBytes(s.CacheDiskMaxSize)),
			zap.Int64("bytes", s.CacheDiskMaxSize),
			zap.String("free_disk", humanizeBytes(freeDisk)))

		if s.CacheDiskMaxSize > freeDisk {
			s.logger.Warn("Disk cache size exceeds available disk space",
				zap.String("cache_size", humanizeBytes(s.CacheDiskMaxSize)),
				zap.String("free_space", humanizeBytes(freeDisk)),
				zap.String("recommendation", "Consider reducing cache_disk_max_size"))
		}
	}

	// Handle disk count limit
	diskMaxCount := s.CacheDiskMaxCount
	if diskMaxCount < 0 {
		diskMaxCount = -1 // Unlimited
		s.logger.Debug("Disk cache item count set to unlimited")
	} else if diskMaxCount == 0 {
		s.logger.Debug("Disk cache item count disabled")
	} else {
		s.logger.Debug("Disk cache item count configured", zap.Int("count", diskMaxCount))
	}

	// Validate that purge configuration is set when cache is enabled
	cacheEnabled := (memMaxSize > 0 && memMaxCount > 0) || (diskMaxSize > 0 && diskMaxCount > 0)
	if cacheEnabled {
		// These fields are required when cache is enabled
		if s.PurgeURI == "" {
			return fmt.Errorf("purge_uri is required when cache is enabled")
		}
		if s.PurgeHeader == "" {
			return fmt.Errorf("purge_header is required when cache is enabled")
		}
		if s.PurgeToken == "" {
			return fmt.Errorf("purge_token is required when cache is enabled")
		}
	}

	s.Storage = NewStorage(s.CacheDir, s.CacheTTL, memMaxSize, memMaxCount,
		int(s.CacheDiskItemMaxSize), diskMaxSize, diskMaxCount, s.logger)

	return nil
}

func (Sidekick) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID: "http.handlers.sidekick",
		New: func() caddy.Module {
			return new(Sidekick)
		},
	}
}

// ServeHTTP implements the caddy.Handler interface.
func (s *Sidekick) ServeHTTP(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
	bypass := false
	s.logger.Debug("HTTP Version", zap.String("Version", r.Proto))

	reqHdr := r.Header
	storage := s.Storage

	// Handle purge requests
	if strings.HasPrefix(r.URL.Path, s.PurgeURI) {
		return s.handlePurgeRequest(w, r, storage)
	}

	// only GET Method can cache
	if r.Method != "GET" {
		return next.ServeHTTP(w, r)
	}

	// Check bypass conditions
	bypass = s.shouldBypass(r)

	hdr := w.Header()
	if bypass {
		hdr.Set(CacheHeaderName, "BYPASS")
		return next.ServeHTTP(w, r)
	}

	// Check for conditional requests (If-None-Match, If-Modified-Since)
	etag := r.Header.Get("If-None-Match")
	modifiedSince := r.Header.Get("If-Modified-Since")

	// Build cache key with configurable components
	cacheKey := s.buildCacheKey(r)

	requestEncoding := strings.Split(strings.Join(reqHdr["Accept-Encoding"], ""), ",")
	if len(requestEncoding) == 1 && len(requestEncoding[0]) == 0 {
		requestEncoding = nil
	}
	requestEncoding = append(requestEncoding, "none")

	// Try to get from cache with read lock
	s.syncHandler.cacheMu.RLock()
	var cacheData []byte
	var cacheMeta *Metadata
	var err error
	ce := ""
	for _, re := range requestEncoding {
		ce = strings.TrimSpace(re)
		cacheData, cacheMeta, err = storage.Get(cacheKey, ce)
		if err == nil {
			break
		}
	}
	s.syncHandler.cacheMu.RUnlock()

	if err == nil {
		// Check for 304 Not Modified
		if s.shouldReturn304(cacheMeta, etag, modifiedSince) {
			w.WriteHeader(http.StatusNotModified)
			return nil
		}

		// Serve from cache
		hdr.Set(CacheHeaderName, "HIT")
		hdr.Set("Vary", "Accept-Encoding")
		if ce != "none" {
			hdr.Set("Content-Encoding", ce)
		}
		// set header back
		for _, kv := range cacheMeta.Header {
			if len(kv) != 2 {
				continue
			}
			hdr.Set(kv[0], kv[1])
		}
		w.WriteHeader(cacheMeta.StateCode)
		_, writeErr := w.Write(cacheData)
		if writeErr != nil {
			s.logger.Error("Error writing cached response", zap.Error(writeErr))
		}

		return nil
	}

	s.logger.Debug("sidekick - cache miss - "+cacheKey, zap.Error(err))

	// Use sync.Once to prevent duplicate cache operations for the same key
	s.syncHandler.inFlightMu.Lock()
	once, exists := s.syncHandler.inFlight[cacheKey]
	if !exists {
		once = &sync.Once{}
		s.syncHandler.inFlight[cacheKey] = once
	}
	s.syncHandler.inFlightMu.Unlock()

	// Create custom writer to capture response
	buf := s.bufferPool.Get().(*bytes.Buffer)
	buf.Reset()
	nw := NewResponseWriter(w, r, storage, s.logger, s, once, cacheKey, buf)
	defer func() {
		// Return buffer to pool
		s.bufferPool.Put(buf)
		if err := nw.Close(); err != nil {
			s.logger.Error("Error closing response writer", zap.Error(err))
		}
		// Clean up in-flight tracker after some time
		go func() {
			s.syncHandler.inFlightMu.Lock()
			delete(s.syncHandler.inFlight, cacheKey)
			s.syncHandler.inFlightMu.Unlock()
		}()
	}()

	return next.ServeHTTP(nw, r)
}

func (s *Sidekick) handlePurgeRequest(w http.ResponseWriter, r *http.Request, storage *Storage) error {
	// Only accept POST requests
	if r.Method != "POST" {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return nil
	}

	// Validate purge token
	reqHdr := r.Header
	token := reqHdr.Get(s.PurgeHeader)

	if s.PurgeToken != "" && token != s.PurgeToken {
		s.logger.Warn("sidekick - purge - invalid token", zap.String("path", r.URL.Path))
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return nil
	}

	// Parse request body for paths
	var purgeRequest struct {
		Paths []string `json:"paths"`
	}

	// Read body if present
	if r.Body != nil {
		defer func() {
			_ = r.Body.Close()
		}()
		bodyBytes, err := io.ReadAll(r.Body)
		if err != nil {
			s.logger.Error("Error reading purge request body", zap.Error(err))
			http.Error(w, "Bad Request", http.StatusBadRequest)
			return nil
		}

		// Only parse if body is not empty
		if len(bodyBytes) > 0 {
			if err := json.Unmarshal(bodyBytes, &purgeRequest); err != nil {
				// Body is not valid JSON, treat as empty request
				purgeRequest.Paths = nil
			}
		}
	}

	// Use write lock for purge operations
	s.syncHandler.cacheMu.Lock()
	defer s.syncHandler.cacheMu.Unlock()

	// If no paths specified or empty body, purge everything
	if len(purgeRequest.Paths) == 0 {
		s.logger.Debug("sidekick - purge all cache")
		err := storage.Flush()
		if err != nil {
			s.logger.Error("Error flushing cache", zap.Error(err))
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return nil
		}
	} else {
		// Purge specific paths by checking metadata
		for _, path := range purgeRequest.Paths {
			s.logger.Debug("sidekick - purge specific path", zap.String("path", path))

			// Get all cache keys
			cacheList := storage.List()
			allKeys := append(cacheList["mem"], cacheList["disk"]...)

			s.logger.Debug("sidekick - total cache keys",
				zap.Int("memory_keys", len(cacheList["mem"])),
				zap.Int("disk_keys", len(cacheList["disk"])),
				zap.Int("total_keys", len(allKeys)))

			purgedCount := 0
			keysToPurge := []string{}

			// Handle wildcard patterns
			var pathMatcher func(string) bool
			if strings.Contains(path, "*") {
				// Convert wildcard pattern to regex
				pattern := regexp.QuoteMeta(path)
				pattern = strings.ReplaceAll(pattern, "\\*", ".*")
				re, err := regexp.Compile("^" + pattern + "$")
				if err != nil {
					s.logger.Error("Invalid wildcard pattern", zap.String("pattern", path), zap.Error(err))
					continue
				}
				pathMatcher = func(p string) bool {
					return re.MatchString(p)
				}
			} else {
				// Exact path match
				pathMatcher = func(p string) bool {
					return p == path
				}
			}

			// Check each cached item's metadata
			for _, key := range allKeys {
				// Get metadata without loading the full content
				var meta *Metadata

				// Check memory cache first
				memCache := storage.GetMemCache()
				if memCache != nil {
					if cacheItem, ok := memCache.Get(key); ok && cacheItem != nil && (*cacheItem).Metadata != nil {
						meta = (*cacheItem).Metadata
						s.logger.Debug("sidekick - found metadata in memory",
							zap.String("key", key),
							zap.String("path", meta.Path))
					}
				}

				// If not in memory, check disk cache metadata
				if meta == nil {
					diskCache := storage.GetDiskCache()
					if diskCache != nil {
						if item, err := diskCache.Get(key); err == nil && item != nil {
							// Load just the metadata
							metaPath := filepath.Join(item.Path, "metadata.json")
							tempMeta := &Metadata{}
							if err := tempMeta.LoadFromFile(metaPath); err == nil {
								meta = tempMeta
								s.logger.Debug("sidekick - found metadata on disk",
									zap.String("key", key),
									zap.String("path", meta.Path),
									zap.String("metaPath", metaPath))
							} else {
								s.logger.Debug("sidekick - failed to load metadata from disk",
									zap.String("key", key),
									zap.String("metaPath", metaPath),
									zap.Error(err))
							}
						}
					}
				}

				// Check if this entry's path matches
				if meta != nil {
					s.logger.Debug("sidekick - checking path match",
						zap.String("key", key),
						zap.String("metaPath", meta.Path),
						zap.String("targetPath", path),
						zap.Bool("hasPath", meta.Path != ""),
						zap.Bool("matches", meta.Path != "" && pathMatcher(meta.Path)))

					if meta.Path != "" && pathMatcher(meta.Path) {
						keysToPurge = append(keysToPurge, key)
						s.logger.Debug("sidekick - will purge key", zap.String("key", key))
					}
				} else {
					s.logger.Debug("sidekick - no metadata found for key", zap.String("key", key))
				}
			}

			// Purge all matching keys
			s.logger.Debug("sidekick - keys to purge",
				zap.String("path", path),
				zap.Int("count", len(keysToPurge)),
				zap.Strings("keys", keysToPurge))

			for _, key := range keysToPurge {
				if err := storage.Purge(key); err == nil {
					purgedCount++
					s.logger.Debug("sidekick - successfully purged key", zap.String("key", key))
				} else {
					s.logger.Error("sidekick - failed to purge key",
						zap.String("key", key),
						zap.Error(err))
				}
			}

			s.logger.Debug("sidekick - purge complete",
				zap.String("path", path),
				zap.Int("purged_count", purgedCount),
				zap.Int("target_count", len(keysToPurge)))
		}
	}

	w.Header().Set("Content-Type", "application/json")
	if _, err := w.Write([]byte(`{"status":"ok"}`)); err != nil {
		s.logger.Error("Error writing purge response", zap.Error(err))
	}
	return nil
}

func (s *Sidekick) shouldBypass(r *http.Request) bool {
	// Check debug query parameter
	if s.bypassDebugQuery != "" && r.URL.Query().Has(s.bypassDebugQuery) {
		return true
	}

	// Check path prefixes
	for _, prefix := range s.NoCache {
		if prefix != "" && strings.HasPrefix(r.URL.Path, prefix) {
			s.logger.Debug("sidekick - bypass prefix", zap.String("prefix", prefix))
			return true
		}
	}

	// Check regex pattern
	if s.pathRx != nil && s.pathRx.MatchString(r.URL.Path) {
		s.logger.Debug("sidekick - bypass regex", zap.String("regex", s.NoCacheRegex))
		return true
	}

	// Check home page
	if s.NoCacheHome && r.URL.Path == "/" {
		return true
	}

	// Check WordPress login cookie
	cookies := r.Cookies()
	for _, cookie := range cookies {
		if strings.HasPrefix(cookie.Name, "wordpress_logged_in") {
			return true
		}
	}

	return false
}

// getTotalMemory returns the total system memory in bytes
func getTotalMemory() int64 {
	// Try to read from /proc/meminfo on Linux
	data, err := os.ReadFile("/proc/meminfo")
	if err == nil {
		lines := strings.Split(string(data), "\n")
		for _, line := range lines {
			if strings.HasPrefix(line, "MemTotal:") {
				fields := strings.Fields(line)
				if len(fields) >= 2 {
					if kb, err := strconv.ParseInt(fields[1], 10, 64); err == nil {
						return kb * 1024 // Convert KB to bytes
					}
				}
			}
		}
	}

	// Default to 1GB if we can't determine
	return 1024 * 1024 * 1024
}

// getAvailableMemory returns the available system memory in bytes
func getAvailableMemory() int64 {
	// Try to read from /proc/meminfo on Linux
	data, err := os.ReadFile("/proc/meminfo")
	if err == nil {
		lines := strings.Split(string(data), "\n")
		for _, line := range lines {
			// Try MemAvailable first (newer kernels)
			if strings.HasPrefix(line, "MemAvailable:") {
				fields := strings.Fields(line)
				if len(fields) >= 2 {
					if kb, err := strconv.ParseInt(fields[1], 10, 64); err == nil {
						return kb * 1024 // Convert KB to bytes
					}
				}
			}
			// Fallback to MemFree for older kernels
			if strings.HasPrefix(line, "MemFree:") {
				fields := strings.Fields(line)
				if len(fields) >= 2 {
					if kb, err := strconv.ParseInt(fields[1], 10, 64); err == nil {
						return kb * 1024 // Convert KB to bytes
					}
				}
			}
		}
	}

	// Default to 512MB if we can't determine
	return 512 * 1024 * 1024
}

// getTotalDiskSpace returns the total disk space for the given path in bytes
func getTotalDiskSpace(path string) int64 {
	// This is a simplified implementation
	// In production, you'd use syscall.Statfs or similar
	// Default to 100GB if we can't determine
	return 100 * 1024 * 1024 * 1024
}

// getFreeDiskSpace returns the free disk space for the given path in bytes
func getFreeDiskSpace(path string) int64 {
	// This is a simplified implementation
	// In production, you'd use syscall.Statfs or similar
	// Default to 50GB if we can't determine
	return 50 * 1024 * 1024 * 1024
}

// humanizeBytes converts bytes to human-readable format
func humanizeBytes(bytes int64) string {
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

	units := []string{"B", "KB", "MB", "GB", "TB", "PB"}
	return fmt.Sprintf("%.1f%s", float64(bytes)/float64(div), units[exp+1])
}

// buildCacheKey builds a cache key based on configured components
func (s *Sidekick) buildCacheKey(r *http.Request) string {
	h := md5.New()

	// Always include path
	h.Write([]byte(r.URL.Path))

	// Include configured query parameters
	if len(s.CacheKeyQueries) > 0 {
		query := r.URL.Query()
		for _, q := range s.CacheKeyQueries {
			if q == "*" {
				// Include all query parameters
				h.Write([]byte(query.Encode()))
				break
			}
			if val := query.Get(q); val != "" {
				h.Write([]byte(q + "=" + val))
			}
		}
	}

	// Include configured headers
	for _, hdr := range s.CacheKeyHeaders {
		if val := r.Header.Get(hdr); val != "" {
			h.Write([]byte(hdr + ":" + val))
		}
	}

	// Include configured cookies
	for _, cookieName := range s.CacheKeyCookies {
		if cookie, err := r.Cookie(cookieName); err == nil {
			h.Write([]byte(cookieName + "=" + cookie.Value))
		}
	}

	return fmt.Sprintf("%x", h.Sum(nil))
}

// shouldReturn304 checks if we should return a 304 Not Modified response
func (s *Sidekick) shouldReturn304(meta *Metadata, ifNoneMatch, ifModifiedSince string) bool {
	// Check ETag
	if ifNoneMatch != "" {
		for _, kv := range meta.Header {
			if len(kv) == 2 && kv[0] == "Etag" && kv[1] == ifNoneMatch {
				return true
			}
		}
	}

	// Check Last-Modified
	if ifModifiedSince != "" {
		for _, kv := range meta.Header {
			if len(kv) == 2 && kv[0] == "Last-Modified" {
				// Simple string comparison - could be enhanced with proper date parsing
				if kv[1] == ifModifiedSince {
					return true
				}
			}
		}
	}

	return false
}

// Interface guards
var (
	_ caddy.Provisioner           = (*Sidekick)(nil)
	_ caddyhttp.MiddlewareHandler = (*Sidekick)(nil)
	_ caddyfile.Unmarshaler       = (*Sidekick)(nil)

	_ http.ResponseWriter = (*NopResponseWriter)(nil)
)

type NopResponseWriter map[string][]string

func (nop *NopResponseWriter) WriteHeader(statusCode int) {}

func (nop *NopResponseWriter) Write(buf []byte) (int, error) {
	return len(buf), nil
}

func (nop *NopResponseWriter) Header() http.Header {
	return http.Header(*nop)
}
