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

func NewResponseWriter(rw http.ResponseWriter, r *http.Request, storage *Storage, logger *zap.Logger, s *Sidekick, once *sync.Once, cacheKey string, buf *bytes.Buffer, dims keyDims) *ResponseWriter {
	nw := ResponseWriter{
		ResponseWriter: rw,
		Request:        r,
		Storage:        storage,
		Logger:         logger,
		keyDims:        dims,

		// keep original request info
		origUrl: *r.URL,

		cacheMaxSize:       int(s.CacheMemoryItemMaxSize),
		maxCacheableSize:   int(s.CacheDiskItemMaxSize),
		streamToDiskSize:   int(s.CacheMemoryStreamToDiskSize),
		cacheResponseCodes: s.CacheResponseCodes,
		cacheHeaderName:    CacheHeaderName,
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

	// keyDims records which request dimensions varied the cache key, and drives
	// whether this response may be stored by a shared cache. See
	// Sidekick.applyDownstreamCacheHeaders.
	keyDims keyDims

	// captureOnly suppresses pass-through to the client while still capturing the
	// body for the cache. Used by the range-fill path, which fetches the full
	// representation and then serves only the requested range from the capture.
	captureOnly bool

	// fillAbandoned records that a capture-only fill was given up and the response
	// was streamed to the client normally instead. The caller must not try to serve
	// a range afterwards: the client has already received a complete response.
	fillAbandoned bool

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
			meta := NewMetadataWithPath(int(atomic.LoadInt32(&r.status)), hdr, r.origUrl.Path)
			if meta == nil {
				return
			}

			// Debug logging for caching
			contentEncoding := hdr.Get("Content-Encoding")
			r.Debug("Preparing to cache response",
				zap.String("path", r.origUrl.Path),
				zap.String("contentEncoding", contentEncoding),
				zap.Int("status", int(atomic.LoadInt32(&r.status))))

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

			// Debug what we're caching
			r.Debug("Storing data in cache",
				zap.String("key", r.cacheKey),
				zap.Int("dataSize", len(dataToCache)),
				zap.Bool("isStreaming", r.isStreaming))

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
			// Check if disk caching is disabled (0) or if size exceeds limit
			if r.maxCacheableSize == 0 || (r.maxCacheableSize > 0 && size > int64(r.maxCacheableSize)) {
				bypass = true
				r.Debug("Bypass caching due to Content-Length exceeding disk limit", zap.Int64("size", size), zap.Int("limit", r.maxCacheableSize))
			}
		}
	}

	if bypass {
		hdr.Set(r.cacheHeaderName, "BYPASS")
		r.sidekick.applyDownstreamCacheHeaders(hdr, r.keyDims, cacheStateBypass, nil)
		// Nothing will be cached, so there is no reason to hold the body back.
		// Abandon the fill before any bytes are written and stream normally.
		r.abandonCaptureBeforeBody()
		r.ResponseWriter.WriteHeader(status)
		return
	}

	atomic.StoreInt32(&r.needCache, 1)

	hdr.Set(r.cacheHeaderName, "MISS")
	r.sidekick.applyDownstreamCacheHeaders(hdr, r.keyDims, cacheStateMiss, nil)

	// Only a complete 200 can have a range taken out of it. Anything else (206 from
	// an origin that ignored the stripped Range, 3xx, 404) is served straight through.
	if r.captureOnly && status != http.StatusOK {
		r.abandonCaptureBeforeBody()
	}

	if !r.captureOnly {
		r.ResponseWriter.WriteHeader(status)
	}
}

// abandonCaptureBeforeBody gives up a capture-only fill while no body bytes have been
// written yet. The caller writes the status line itself immediately afterwards.
func (r *ResponseWriter) abandonCaptureBeforeBody() {
	if !r.captureOnly {
		return
	}
	r.captureOnly = false
	r.fillAbandoned = true
}

// releaseCapture gives up a capture-only fill after some body has already been
// buffered. It writes the status line, flushes everything captured so far to the
// client, and reverts to pass-through for the remainder.
//
// This exists so that abandoning a fill mid-stream still produces a complete, correct
// response. Without it the held-back bytes would simply be dropped and the client
// would receive a truncated body.
//
// Caller must hold bufMu.
func (r *ResponseWriter) releaseCaptureLocked() error {
	r.captureOnly = false
	r.fillAbandoned = true

	status := int(atomic.LoadInt32(&r.status))
	if status < 0 {
		status = http.StatusOK
	}
	r.ResponseWriter.WriteHeader(status)

	if r.isStreaming && r.tempFile != nil {
		if _, err := r.tempFile.Seek(0, io.SeekStart); err != nil {
			return fmt.Errorf("failed to rewind capture while abandoning fill: %w", err)
		}
		if _, err := io.Copy(r.ResponseWriter, r.tempFile); err != nil {
			return fmt.Errorf("failed to flush capture while abandoning fill: %w", err)
		}
		return nil
	}

	if r.buffer.Len() > 0 {
		if _, err := r.ResponseWriter.Write(r.buffer.Bytes()); err != nil {
			return fmt.Errorf("failed to flush capture while abandoning fill: %w", err)
		}
	}
	return nil
}

// Write will write the response body
func (r *ResponseWriter) Write(b []byte) (int, error) {
	// check header has been written or not
	if atomic.CompareAndSwapInt32(&r.status, -1, 200) {
		r.WriteHeader(200)
	}

	// Always write to the actual response writer first, unless we are capturing the
	// body without serving it (range fill), in which case the caller serves the
	// requested range from the capture once the upstream response is complete.
	var n int
	var err error
	if r.captureOnly {
		n = len(b)
	} else {
		n, err = r.ResponseWriter.Write(b)
	}

	// save response data for caching
	if atomic.LoadInt32(&r.needCache) == 1 {
		r.bufMu.Lock()
		defer r.bufMu.Unlock()

		newSize := r.totalSize + int64(len(b))

		// Check if we exceed max disk cacheable size (0 = disabled, -1 = unlimited)
		if r.maxCacheableSize == 0 || (r.maxCacheableSize > 0 && newSize > int64(r.maxCacheableSize)) {
			// Too large to cache on disk or disk caching disabled
			atomic.StoreInt32(&r.needCache, 0)

			// A capture-only fill must not silently swallow the body it was holding:
			// flush it and revert to pass-through before discarding the capture.
			if r.captureOnly {
				if relErr := r.releaseCaptureLocked(); relErr != nil {
					r.Error("Failed to release capture", zap.Error(relErr))
					return n, relErr
				}
				if _, wErr := r.ResponseWriter.Write(b); wErr != nil {
					return n, wErr
				}
			}

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
			r.isStreaming = false
			r.buffer.Reset()
			r.Debug("Bypass caching because data size exceeds disk limit", zap.Int64("size", newSize), zap.Int("limit", r.maxCacheableSize))
			return n, err
		}

		// Check if we should switch to streaming to disk (0 = disabled, -1 = unlimited/never stream)
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

// Status returns the status code the upstream produced, or 0 if it never wrote one.
func (r *ResponseWriter) Status() int {
	s := atomic.LoadInt32(&r.status)
	if s < 0 {
		return 0
	}
	return int(s)
}

// CapturedReader returns a seekable reader over the captured body along with its
// length. When the body was spooled to disk the temp file is returned directly — it
// is already an io.ReadSeeker, so the range is served straight off disk with no
// additional copy in memory.
//
// Only valid in captureOnly mode, after the upstream handler has returned. The
// returned reader stays owned by the ResponseWriter; Close() cleans it up.
func (r *ResponseWriter) CapturedReader() (io.ReadSeeker, int64, error) {
	r.bufMu.Lock()
	defer r.bufMu.Unlock()

	if r.isStreaming {
		if r.tempFile == nil {
			return nil, 0, fmt.Errorf("streaming capture has no temp file")
		}
		if _, err := r.tempFile.Seek(0, io.SeekStart); err != nil {
			return nil, 0, fmt.Errorf("failed to rewind capture: %w", err)
		}
		return r.tempFile, r.totalSize, nil
	}

	return bytes.NewReader(r.buffer.Bytes()), int64(r.buffer.Len()), nil
}

// ReplayCaptured writes the captured status and body through to the client verbatim.
// Used when a range fill cannot be completed — a non-200 upstream, or a capture that
// could not be read back — so the client still receives a correct response.
func (r *ResponseWriter) ReplayCaptured(w http.ResponseWriter) error {
	reader, size, err := r.CapturedReader()
	if err != nil {
		return err
	}

	status := r.Status()
	if status == 0 {
		status = http.StatusOK
	}

	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	w.WriteHeader(status)
	_, err = io.Copy(w, reader)
	return err
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
