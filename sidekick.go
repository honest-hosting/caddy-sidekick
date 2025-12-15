package sidekick

import (
	"bytes"
	"crypto/md5"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"go.uber.org/zap"
)

type Sidekick struct {
	logger             *zap.Logger
	Loc                string
	PurgePath          string
	PurgeKeyHeader     string
	PurgeKey           string
	CacheHeaderName    string
	BypassPathPrefixes []string
	BypassPathRegex    string
	BypassHome         bool
	BypassDebugQuery   string
	CacheResponseCodes []string
	TTL                int
	Storage            *Storage

	MemoryItemMaxSize   int
	MemoryCacheMaxSize  int
	MemoryCacheMaxCount int
	MaxCacheableSize    int // Maximum size for a response to be cached (0 = unlimited)
	StreamToDiskSize    int // Size threshold to stream directly to disk instead of memory

	// Cache key configuration
	CacheKeyHeaders []string // Headers to include in cache key
	CacheKeyQueries []string // Query parameters to include in cache key
	CacheKeyCookies []string // Cookies to include in cache key

	pathRx *regexp.Regexp

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

func (s *Sidekick) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	for d.Next() {
		var value string

		key := d.Val()

		if !d.Args(&value) {
			continue
		}

		switch key {
		case "loc":
			s.Loc = value

		case "bypass_path_prefixes":
			s.BypassPathPrefixes = strings.Split(strings.TrimSpace(value), ",")

		case "bypass_path_regex":
			value = strings.TrimSpace(value)
			if len(value) != 0 {
				_, err := regexp.Compile(value)
				if err != nil {
					return err
				}
			} else {
				// bypass all media, images, css, js, etc
				value = ".*(\\.[^.]+)$"
			}
			s.BypassPathRegex = value

		case "bypass_home":
			if strings.ToLower(value) == "true" {
				s.BypassHome = true
			}

		case "bypass_debug_query":
			s.BypassDebugQuery = strings.TrimSpace(value)

		case "cache_response_codes":
			codes := strings.Split(strings.TrimSpace(value), ",")
			s.CacheResponseCodes = make([]string, len(codes))

			for i, code := range codes {
				code = strings.TrimSpace(code)
				if strings.Contains(code, "XX") {
					code = string(code[0])
				}
				s.CacheResponseCodes[i] = code
			}

		case "ttl":
			ttl, err := strconv.Atoi(value)
			if err != nil {
				s.logger.Error("Invalid TTL value", zap.Error(err))
				continue
			}
			s.TTL = ttl

		case "purge_path":
			s.PurgePath = value

		case "purge_key":
			s.PurgeKey = strings.TrimSpace(value)

		case "purge_key_header":
			s.PurgeKeyHeader = value

		case "cache_header_name":
			s.CacheHeaderName = value

		case "memory_item_max_size":
			if n, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64); err == nil {
				s.MemoryItemMaxSize = int(n)
			}

		case "memory_max_size":
			if n, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64); err == nil {
				s.MemoryCacheMaxSize = int(n)
			}
		case "memory_max_count":
			if n, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64); err == nil {
				s.MemoryCacheMaxCount = int(n)
			}

		case "max_cacheable_size":
			if n, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64); err == nil {
				s.MaxCacheableSize = int(n)
			}

		case "stream_to_disk_size":
			if n, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64); err == nil {
				s.StreamToDiskSize = int(n)
			}

		case "cache_key_headers":
			s.CacheKeyHeaders = strings.Split(strings.TrimSpace(value), ",")
			for i := range s.CacheKeyHeaders {
				s.CacheKeyHeaders[i] = strings.TrimSpace(s.CacheKeyHeaders[i])
			}

		case "cache_key_queries":
			s.CacheKeyQueries = strings.Split(strings.TrimSpace(value), ",")
			for i := range s.CacheKeyQueries {
				s.CacheKeyQueries[i] = strings.TrimSpace(s.CacheKeyQueries[i])
			}

		case "cache_key_cookies":
			s.CacheKeyCookies = strings.Split(strings.TrimSpace(value), ",")
			for i := range s.CacheKeyCookies {
				s.CacheKeyCookies[i] = strings.TrimSpace(s.CacheKeyCookies[i])
			}
		}
	}

	return nil
}

// Constants for default values
const (
	DefaultMemoryItemMaxSize   = 4 * 1024 * 1024   // 4MB
	DefaultMemoryCacheMaxSize  = 128 * 1024 * 1024 // 128MB
	DefaultMemoryCacheMaxCount = 32 * 1024         // 32K items
	DefaultBypassDebugQuery    = "WPEverywhere-NOCACHE"
	DefaultPurgePath           = "/__sidekick/purge"
	DefaultPurgeKeyHeader      = "X-Sidekick-Purge-Key"
	DefaultCacheHeaderName     = "X-Sidekick-Cache"
	DefaultBypassPathRegex     = ".*(\\.[^.]+)$" // bypass all media, images, css, js, etc
	DefaultTTL                 = 6000
	DefaultMaxCacheableSize    = 100 * 1024 * 1024 // 100MB
	DefaultStreamToDiskSize    = 10 * 1024 * 1024  // 10MB
	DefaultBufferSize          = 32 * 1024         // 32KB buffer size
)

func (s *Sidekick) Provision(ctx caddy.Context) error {
	s.logger = ctx.Logger(s)
	s.syncHandler = &SyncHandler{
		inFlight: make(map[string]*sync.Once),
	}

	// Initialize buffer pool
	s.bufferPool = &sync.Pool{
		New: func() interface{} {
			return bytes.NewBuffer(make([]byte, 0, DefaultBufferSize))
		},
	}

	if s.Loc == "" {
		s.Loc = os.Getenv("CACHE_LOC")
		if s.Loc == "" {
			s.Loc = "/var/www/html/wp-content/cache"
		}
	}

	if s.CacheResponseCodes == nil {
		codes := os.Getenv("CACHE_RESPONSE_CODES")
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

	if s.BypassPathPrefixes == nil {
		prefixes := os.Getenv("BYPASS_PATH_PREFIX")
		if prefixes == "" {
			prefixes = "/wp-admin,/wp-json"
		}
		s.BypassPathPrefixes = strings.Split(strings.TrimSpace(prefixes), ",")
	}

	if s.BypassPathRegex == "" {
		s.BypassPathRegex = DefaultBypassPathRegex
	}
	if s.BypassPathRegex != "" {
		rx, err := regexp.Compile(s.BypassPathRegex)
		if err != nil {
			return err
		}
		s.pathRx = rx
	}

	if !s.BypassHome {
		if strings.ToLower(os.Getenv("BYPASS_HOME")) == "true" {
			s.BypassHome = true
		}
	}

	if s.BypassDebugQuery == "" {
		s.BypassDebugQuery = os.Getenv("BYPASS_DEBUG_QUERY")
		if s.BypassDebugQuery == "" {
			s.BypassDebugQuery = DefaultBypassDebugQuery
		}
	}

	if s.TTL == 0 {
		ttl, err := strconv.Atoi(os.Getenv("TTL"))
		if err == nil && ttl > 0 {
			s.TTL = ttl
		} else {
			s.TTL = DefaultTTL
		}
	}

	if s.PurgePath == "" {
		s.PurgePath = os.Getenv("PURGE_PATH")
		if s.PurgePath == "" {
			s.PurgePath = DefaultPurgePath
		}
	}

	if s.PurgeKey == "" {
		s.PurgeKey = os.Getenv("PURGE_KEY")
	}

	if s.PurgeKeyHeader == "" {
		s.PurgeKeyHeader = os.Getenv("PURGE_KEY_HEADER")
		if s.PurgeKeyHeader == "" {
			s.PurgeKeyHeader = DefaultPurgeKeyHeader
		}
	}

	if s.CacheHeaderName == "" {
		s.CacheHeaderName = os.Getenv("CACHE_HEADER_NAME")
		if s.CacheHeaderName == "" {
			s.CacheHeaderName = DefaultCacheHeaderName
		}
	}

	if s.MemoryItemMaxSize == 0 {
		s.MemoryItemMaxSize = DefaultMemoryItemMaxSize
	}
	if s.MemoryItemMaxSize < 0 {
		s.MemoryItemMaxSize = math.MaxInt
	}

	if s.MemoryCacheMaxSize == 0 {
		s.MemoryCacheMaxSize = DefaultMemoryCacheMaxSize
	}

	if s.MemoryCacheMaxCount == 0 {
		s.MemoryCacheMaxCount = DefaultMemoryCacheMaxCount
	}

	if s.MaxCacheableSize == 0 {
		s.MaxCacheableSize = DefaultMaxCacheableSize
	}

	if s.StreamToDiskSize == 0 {
		s.StreamToDiskSize = DefaultStreamToDiskSize
	}

	s.Storage = NewStorage(s.Loc, s.TTL, s.MemoryCacheMaxSize, s.MemoryCacheMaxCount, s.logger)

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
	if strings.HasPrefix(r.URL.Path, s.PurgePath) {
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
		hdr.Set(s.CacheHeaderName, "BYPASS")
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
		hdr.Set(s.CacheHeaderName, "HIT")
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
	reqHdr := r.Header
	key := reqHdr.Get(s.PurgeKeyHeader)

	// Validate purge key
	if s.PurgeKey != "" && key != s.PurgeKey {
		s.logger.Warn("sidekick - purge - invalid key", zap.String("path", r.URL.Path))
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return nil
	}

	switch r.Method {
	case "GET":
		cacheList := storage.List()
		if err := json.NewEncoder(w).Encode(cacheList); err != nil {
			s.logger.Error("Error encoding cache list", zap.Error(err))
			return err
		}
		return nil

	case "POST":
		pathToPurge := strings.Replace(r.URL.Path, s.PurgePath, "", 1)
		s.logger.Debug("sidekick - purge", zap.String("path", pathToPurge))

		// Use write lock for purge operations
		s.syncHandler.cacheMu.Lock()
		if len(pathToPurge) < 2 {
			err := storage.Flush()
			s.syncHandler.cacheMu.Unlock()
			if err != nil {
				s.logger.Error("Error flushing cache", zap.Error(err))
				http.Error(w, "Internal Server Error", http.StatusInternalServerError)
				return nil
			}
		} else {
			storage.Purge(pathToPurge)
			s.syncHandler.cacheMu.Unlock()
		}

		if _, err := w.Write([]byte("OK")); err != nil {
			s.logger.Error("Error writing purge response", zap.Error(err))
		}
		return nil

	default:
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return nil
	}
}

func (s *Sidekick) shouldBypass(r *http.Request) bool {
	// Check debug query parameter
	if s.BypassDebugQuery != "" && r.URL.Query().Has(s.BypassDebugQuery) {
		return true
	}

	// Check path prefixes
	for _, prefix := range s.BypassPathPrefixes {
		if prefix != "" && strings.HasPrefix(r.URL.Path, prefix) {
			s.logger.Debug("sidekick - bypass prefix", zap.String("prefix", prefix))
			return true
		}
	}

	// Check regex pattern
	if s.pathRx != nil && s.pathRx.MatchString(r.URL.Path) {
		s.logger.Debug("sidekick - bypass regex", zap.String("regex", s.BypassPathRegex))
		return true
	}

	// Check home page
	if s.BypassHome && r.URL.Path == "/" {
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
