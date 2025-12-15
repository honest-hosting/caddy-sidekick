package sidekick

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"

	"go.uber.org/zap"
)

func NewResponseWriter(rw http.ResponseWriter, r *http.Request, storage *Storage, logger *zap.Logger, s *Sidekick, once *sync.Once, cacheKey string, buf *bytes.Buffer) *ResponseWriter {
	nw := ResponseWriter{
		ResponseWriter: rw,
		Request:        r,
		Storage:        storage,
		Logger:         logger,

		// keep original request info
		origUrl: *r.URL,

		cacheMaxSize:       s.MemoryItemMaxSize,
		maxCacheableSize:   s.MaxCacheableSize,
		streamToDiskSize:   s.StreamToDiskSize,
		cacheResponseCodes: s.CacheResponseCodes,
		cacheHeaderName:    s.CacheHeaderName,
		status:             -1,
		once:               once,
		cacheKey:           cacheKey,
		cacheMu:            &s.syncHandler.cacheMu,
		buffer:             buf,
		sidekick:           s,
	}
	return &nw
}

var _ http.ResponseWriter = (*ResponseWriter)(nil)

// ResponseWriter handles the response and provide the way to cache the value
type ResponseWriter struct {
	http.ResponseWriter
	*http.Request
	*Storage
	*zap.Logger
	cacheResponseCodes []string
	cacheHeaderName    string
	cacheMaxSize       int
	maxCacheableSize   int
	streamToDiskSize   int

	origUrl url.URL

	// -1 means header not send yet
	status int32

	// flag response data need to be cached
	needCache int32

	// Buffer from pool
	buffer *bytes.Buffer

	// For streaming to disk
	tempFile     *os.File
	tempFilePath string
	isStreaming  bool
	totalSize    int64

	// Concurrency control
	once     *sync.Once
	cacheKey string
	cacheMu  *sync.RWMutex
	bufMu    sync.Mutex

	// Reference to parent
	sidekick *Sidekick
}

func (r *ResponseWriter) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}

// Close sets cache on response end
func (r *ResponseWriter) Close() error {
	// Clean up temp file if streaming
	if r.isStreaming && r.tempFile != nil {
		defer func() {
			if err := r.tempFile.Close(); err != nil {
				r.Error("Failed to close temp file", zap.Error(err))
			}
		}()
		if r.tempFilePath != "" {
			defer func() {
				if err := os.Remove(r.tempFilePath); err != nil {
					r.Debug("Failed to remove temp file", zap.String("path", r.tempFilePath), zap.Error(err))
				}
			}()
		}
	}

	if atomic.LoadInt32(&r.needCache) == 1 {
		// Use sync.Once to ensure caching happens only once for this key
		var cacheErr error
		r.once.Do(func() {
			r.cacheMu.Lock()
			defer r.cacheMu.Unlock()

			hdr := r.ResponseWriter.Header()
			meta := NewMetadata(int(atomic.LoadInt32(&r.status)), hdr)
			if meta == nil {
				return
			}

			// Get the data to cache
			var dataToCache []byte
			if r.isStreaming && r.tempFile != nil {
				// Read from temp file
				if _, err := r.tempFile.Seek(0, 0); err != nil {
					r.Error("Failed to seek temp file", zap.Error(err))
					return
				}
				dataToCache = make([]byte, r.totalSize)
				_, err := io.ReadFull(r.tempFile, dataToCache)
				if err != nil {
					r.Error("Failed to read temp file", zap.Error(err))
					return
				}
			} else {
				// Get data from buffer
				r.bufMu.Lock()
				dataToCache = make([]byte, r.buffer.Len())
				copy(dataToCache, r.buffer.Bytes())
				r.bufMu.Unlock()
			}

			// Store in cache using the cache key we built
			cacheErr = r.SetWithKey(r.cacheKey, meta, dataToCache)
			if cacheErr != nil {
				r.Error("Failed to cache response", zap.Error(cacheErr))
			}
		})
		return cacheErr
	}
	return nil
}

func (r *ResponseWriter) Header() http.Header {
	return r.ResponseWriter.Header()
}

func (r *ResponseWriter) WriteHeader(status int) {
	r.Debug("Setting header", zap.Int("status", status))
	atomic.StoreInt32(&r.status, int32(status))

	r.Debug("Writing response", zap.String("path", r.origUrl.Path))
	bypass := true

	// check if the response code is in the cache response codes
	if bypass {
		statusStr := strconv.Itoa(status)
		for _, code := range r.cacheResponseCodes {
			r.Debug("Checking status code", zap.String("code", code), zap.String("status", statusStr))

			if code == statusStr {
				r.Debug("Caching because of status code", zap.String("code", code), zap.String("status", statusStr))
				bypass = false
				break
			}

			// code may be single digit because of wildcard usage (e.g. 2XX, 4XX, 5XX)
			if len(code) == 1 {
				if code == statusStr[0:1] {
					r.Debug("Caching because of wildcard", zap.String("code", code), zap.String("status", statusStr))
					bypass = false
					break
				}
			}
		}
	}

	hdr := r.Header()

	// check if response should not be cached
	for h := range hdr {
		ok := slices.Contains(hdrResNotCacheList, h)
		if ok {
			bypass = true
			break
		}
	}

	// Check Content-Length if available
	if contentLength := hdr.Get("Content-Length"); contentLength != "" {
		if size, err := strconv.ParseInt(contentLength, 10, 64); err == nil {
			if r.maxCacheableSize > 0 && size > int64(r.maxCacheableSize) {
				bypass = true
				r.Debug("Bypass caching due to Content-Length", zap.Int64("size", size), zap.Int("limit", r.maxCacheableSize))
			}
		}
	}

	cacheState := "BYPASS"
	if bypass {
		hdr.Set(r.cacheHeaderName, cacheState)
		r.ResponseWriter.WriteHeader(status)
		return
	}

	atomic.StoreInt32(&r.needCache, 1)
	cacheState = "MISS"

	hdr.Set(r.cacheHeaderName, cacheState)
	r.ResponseWriter.WriteHeader(status)
}

// Write will write the response body
func (r *ResponseWriter) Write(b []byte) (int, error) {
	// check header has been written or not
	if atomic.CompareAndSwapInt32(&r.status, -1, 200) {
		r.WriteHeader(200)
	}

	// Always write to the actual response writer first
	n, err := r.ResponseWriter.Write(b)

	// save response data for caching
	if atomic.LoadInt32(&r.needCache) == 1 {
		r.bufMu.Lock()
		defer r.bufMu.Unlock()

		newSize := r.totalSize + int64(len(b))

		// Check if we exceed max cacheable size
		if r.maxCacheableSize > 0 && newSize > int64(r.maxCacheableSize) {
			// Too large to cache
			atomic.StoreInt32(&r.needCache, 0)
			if r.tempFile != nil {
				if err := r.tempFile.Close(); err != nil {
					r.Error("Failed to close temp file", zap.Error(err))
				}
				if err := os.Remove(r.tempFilePath); err != nil {
					r.Debug("Failed to remove temp file", zap.String("path", r.tempFilePath), zap.Error(err))
				}
				r.tempFile = nil
				r.tempFilePath = ""
			}
			r.buffer.Reset()
			r.Debug("Bypass caching because of data size", zap.Int64("size", newSize), zap.Int("limit", r.maxCacheableSize))
			return n, err
		}

		// Check if we should switch to streaming to disk
		if !r.isStreaming && r.streamToDiskSize > 0 && newSize > int64(r.streamToDiskSize) {
			// Switch to streaming to disk
			if err := r.switchToStreaming(); err != nil {
				r.Error("Failed to switch to streaming", zap.Error(err))
				atomic.StoreInt32(&r.needCache, 0)
				return n, err
			}
		}

		// Write to buffer or temp file
		if r.isStreaming {
			if _, writeErr := r.tempFile.Write(b); writeErr != nil {
				r.Error("Failed to write to temp file", zap.Error(writeErr))
				atomic.StoreInt32(&r.needCache, 0)
				return n, err
			}
		} else {
			r.buffer.Write(b)
		}

		r.totalSize = newSize
	}

	return n, err
}

// switchToStreaming switches from memory buffering to disk streaming
func (r *ResponseWriter) switchToStreaming() error {
	// Create temp file
	tempDir := path.Join(r.loc, "temp")
	if err := os.MkdirAll(tempDir, 0755); err != nil {
		return fmt.Errorf("failed to create temp dir: %w", err)
	}

	tempFile, err := os.CreateTemp(tempDir, "sidekick-*.tmp")
	if err != nil {
		return fmt.Errorf("failed to create temp file: %w", err)
	}

	// Write existing buffer content to temp file
	if r.buffer.Len() > 0 {
		if _, err := tempFile.Write(r.buffer.Bytes()); err != nil {
			closeErr := tempFile.Close()
			removeErr := os.Remove(tempFile.Name())
			if closeErr != nil {
				r.Error("Failed to close temp file after write error", zap.Error(closeErr))
			}
			if removeErr != nil {
				r.Debug("Failed to remove temp file after write error", zap.String("name", tempFile.Name()), zap.Error(removeErr))
			}
			return fmt.Errorf("failed to write buffer to temp file: %w", err)
		}
	}

	r.tempFile = tempFile
	r.tempFilePath = tempFile.Name()
	r.isStreaming = true
	r.buffer.Reset() // Clear the buffer to free memory

	r.Debug("Switched to streaming to disk", zap.String("tempFile", r.tempFilePath))
	return nil
}

// Implement http.Flusher interface if the underlying ResponseWriter supports it
func (r *ResponseWriter) Flush() {
	if flusher, ok := r.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

// Implement http.Hijacker interface if the underlying ResponseWriter supports it
func (r *ResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if hijacker, ok := r.ResponseWriter.(http.Hijacker); ok {
		return hijacker.Hijack()
	}
	return nil, nil, fmt.Errorf("hijacking not supported")
}

// Implement http.Pusher interface if the underlying ResponseWriter supports it
func (r *ResponseWriter) Push(target string, opts *http.PushOptions) error {
	if pusher, ok := r.ResponseWriter.(http.Pusher); ok {
		return pusher.Push(target, opts)
	}
	return fmt.Errorf("push not supported")
}
