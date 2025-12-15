package sidekick

import (
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"

	"go.uber.org/zap"
)

func NewResponseWriter(rw http.ResponseWriter, r *http.Request, storage *Storage, logger *zap.Logger, s *Sidekick, once *sync.Once, cacheKey string) *ResponseWriter {
	nw := ResponseWriter{
		ResponseWriter: rw,
		Request:        r,
		Storage:        storage,
		Logger:         logger,

		// keep original request info
		origUrl: *r.URL,

		cacheMaxSize:       s.MemoryItemMaxSize,
		cacheResponseCodes: s.CacheResponseCodes,
		cacheHeaderName:    s.CacheHeaderName,
		status:             -1,
		once:               once,
		cacheKey:           cacheKey,
		cacheMu:            &s.syncHandler.cacheMu,
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

	origUrl url.URL

	// -1 means header not send yet
	status int32

	// flag response data need to be cached
	needCache int32

	// currently cache in memory
	// assume response data not too large
	buf []byte

	// Concurrency control
	once     *sync.Once
	cacheKey string
	cacheMu  *sync.RWMutex
	bufMu    sync.Mutex
}

func (r *ResponseWriter) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}

// Close sets cache on response end
func (r *ResponseWriter) Close() error {
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

			// Lock buffer for reading
			r.bufMu.Lock()
			bufCopy := make([]byte, len(r.buf))
			copy(bufCopy, r.buf)
			r.bufMu.Unlock()

			cacheErr = r.Set(r.origUrl.Path, "", meta, bufCopy)
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

	// save response data
	if atomic.LoadInt32(&r.needCache) == 1 {
		r.bufMu.Lock()
		sz := len(r.buf) + len(b)
		if sz <= r.cacheMaxSize {
			r.buf = append(r.buf, b...)
			r.bufMu.Unlock()
		} else {
			// too large, skip cache in memory
			atomic.StoreInt32(&r.needCache, 0)
			r.buf = nil
			r.bufMu.Unlock()
			r.Debug("Bypass caching because of data size", zap.Int("sz", sz), zap.Int("limit", r.cacheMaxSize))
		}
	}

	return r.ResponseWriter.Write(b)
}
