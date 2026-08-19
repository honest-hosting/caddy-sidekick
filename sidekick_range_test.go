package sidekick

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func newTestBufferPool() *sync.Pool {
	return &sync.Pool{
		New: func() interface{} {
			return bytes.NewBuffer(make([]byte, 0, DefaultBufferSize))
		},
	}
}

// mediaBody returns a deterministic body whose every byte is checkable, so a range
// response can be verified against the exact source offsets.
func mediaBody(size int) []byte {
	b := make([]byte, size)
	for i := range b {
		b[i] = byte(i % 251)
	}
	return b
}

// originHandler serves a fixed body with full Range support, standing in for Caddy's
// file_server. It records how many times it was invoked so tests can assert that a
// range fill hits the origin exactly once.
type originHandler struct {
	body     []byte
	modTime  time.Time
	etag     string
	calls    atomic.Int32
	lastMeth string
	lastRnge string
}

func (o *originHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) error {
	o.calls.Add(1)
	o.lastMeth = r.Method
	o.lastRnge = r.Header.Get("Range")

	w.Header().Set("Content-Type", "video/mp4")
	if o.etag != "" {
		w.Header().Set("Etag", o.etag)
	}
	http.ServeContent(w, r, "", o.modTime, bytes.NewReader(o.body))
	return nil
}

func rangeTestSidekick(t *testing.T) (*Sidekick, *originHandler) {
	t.Helper()

	enabled := true
	s := &Sidekick{
		CacheDir:        t.TempDir(),
		CacheTTL:        3600,
		CacheKeyHeaders: []string{"Accept-Encoding"},
		// "2XX" is normalized to the single-digit wildcard during config parsing.
		CacheResponseCodes:     []string{"2", "301", "302"},
		CacheMemoryItemMaxSize: 1024,
		CacheDiskItemMaxSize:   100 * 1024 * 1024,
		PurgePath:              "/__sidekick/purge",
		PurgeHeader:            "X-Sidekick-Purge",
		PurgeToken:             "test-token",
		RangeFill:              &enabled,
		RangeFillMaxSize:       100 * 1024 * 1024,
		logger:                 zap.NewNop(),
		syncHandler:            &SyncHandler{inFlight: make(map[string]*inflightFill)},
	}
	s.bufferPool = newTestBufferPool()
	// Big enough that bodies land on disk rather than in the memory tier.
	s.Storage = NewStorage(s.CacheDir, s.CacheTTL, 1024, 0, 0, 100*1024*1024, 500*1024*1024, 1000, s.logger)

	rx, err := regexp.Compile(DefaultStaticAssetRegex)
	require.NoError(t, err)
	s.staticAssetRx = rx

	origin := &originHandler{
		body:    mediaBody(64 * 1024),
		modTime: time.Now().Add(-time.Hour).UTC().Truncate(time.Second),
		etag:    `"abc123"`,
	}
	return s, origin
}

func doGet(t *testing.T, s *Sidekick, origin *originHandler, path string, hdrs map[string]string) *http.Response {
	t.Helper()
	req := httptest.NewRequest("GET", path, nil)
	for k, v := range hdrs {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	require.NoError(t, s.ServeHTTP(rec, req, caddyhttp.HandlerFunc(origin.ServeHTTP)))
	return rec.Result()
}

func readAll(t *testing.T, resp *http.Response) []byte {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	return b
}

// TestRangeFill_ColdRangeRequestPopulatesCacheAndServes206 is the headline behavior:
// the exact scenario from the production log, where a cold range request for a video
// previously produced BYPASS forever.
func TestRangeFill_ColdRangeRequestPopulatesCacheAndServes206(t *testing.T) {
	s, origin := rangeTestSidekick(t)
	const path = "/wp-content/uploads/2026/02/video.mp4"

	resp := doGet(t, s, origin, path, map[string]string{"Range": "bytes=1024-2047"})
	body := readAll(t, resp)

	assert.Equal(t, http.StatusPartialContent, resp.StatusCode)
	assert.Equal(t, fmt.Sprintf("bytes 1024-2047/%d", len(origin.body)), resp.Header.Get("Content-Range"))
	assert.Equal(t, origin.body[1024:2048], body, "range bytes must match the source exactly")

	// The origin saw a full request, not a ranged one: that is what makes the entry
	// cacheable.
	assert.Equal(t, "", origin.lastRnge, "Range must be stripped before hitting the origin")

	// A second range request for a different window is now served from cache.
	before := origin.calls.Load()
	resp2 := doGet(t, s, origin, path, map[string]string{"Range": "bytes=4096-4195"})
	body2 := readAll(t, resp2)

	assert.Equal(t, http.StatusPartialContent, resp2.StatusCode)
	assert.Equal(t, origin.body[4096:4196], body2)
	assert.Equal(t, "HIT", resp2.Header.Get(CacheHeaderName))
	assert.Equal(t, before, origin.calls.Load(), "warm range must not touch the origin")
}

func TestRangeHit_SuffixAndOpenEndedRanges(t *testing.T) {
	s, origin := rangeTestSidekick(t)
	const path = "/wp-content/uploads/clip.mp4"
	total := len(origin.body)

	// Prime the cache.
	readAll(t, doGet(t, s, origin, path, nil))

	t.Run("suffix range", func(t *testing.T) {
		resp := doGet(t, s, origin, path, map[string]string{"Range": "bytes=-500"})
		body := readAll(t, resp)
		assert.Equal(t, http.StatusPartialContent, resp.StatusCode)
		assert.Equal(t, origin.body[total-500:], body)
	})

	t.Run("open ended range", func(t *testing.T) {
		resp := doGet(t, s, origin, path, map[string]string{"Range": "bytes=60000-"})
		body := readAll(t, resp)
		assert.Equal(t, http.StatusPartialContent, resp.StatusCode)
		assert.Equal(t, origin.body[60000:], body)
	})

	t.Run("full-cover range still yields 206", func(t *testing.T) {
		resp := doGet(t, s, origin, path, map[string]string{"Range": "bytes=0-"})
		body := readAll(t, resp)
		assert.Equal(t, http.StatusPartialContent, resp.StatusCode)
		assert.Equal(t, origin.body, body)
	})
}

func TestRangeHit_UnsatisfiableYields416(t *testing.T) {
	s, origin := rangeTestSidekick(t)
	const path = "/wp-content/uploads/clip.mp4"

	readAll(t, doGet(t, s, origin, path, nil))

	resp := doGet(t, s, origin, path, map[string]string{"Range": "bytes=99999999-"})
	readAll(t, resp)

	assert.Equal(t, http.StatusRequestedRangeNotSatisfiable, resp.StatusCode)
	assert.Equal(t, fmt.Sprintf("bytes */%d", len(origin.body)), resp.Header.Get("Content-Range"))
}

func TestRangeHit_MultipartRanges(t *testing.T) {
	s, origin := rangeTestSidekick(t)
	const path = "/wp-content/uploads/clip.mp4"

	readAll(t, doGet(t, s, origin, path, nil))

	resp := doGet(t, s, origin, path, map[string]string{"Range": "bytes=0-99,200-299"})
	body := readAll(t, resp)

	assert.Equal(t, http.StatusPartialContent, resp.StatusCode)
	assert.Contains(t, resp.Header.Get("Content-Type"), "multipart/byteranges")
	assert.Contains(t, string(body), "Content-Range: bytes 0-99/")
	assert.Contains(t, string(body), "Content-Range: bytes 200-299/")
}

func TestRangeHit_IfRange(t *testing.T) {
	s, origin := rangeTestSidekick(t)
	const path = "/wp-content/uploads/clip.mp4"

	readAll(t, doGet(t, s, origin, path, nil))

	t.Run("matching etag yields 206", func(t *testing.T) {
		resp := doGet(t, s, origin, path, map[string]string{
			"Range":    "bytes=0-99",
			"If-Range": `"abc123"`,
		})
		readAll(t, resp)
		assert.Equal(t, http.StatusPartialContent, resp.StatusCode)
	})

	t.Run("stale etag yields full 200", func(t *testing.T) {
		resp := doGet(t, s, origin, path, map[string]string{
			"Range":    "bytes=0-99",
			"If-Range": `"stale"`,
		})
		body := readAll(t, resp)
		assert.Equal(t, http.StatusOK, resp.StatusCode)
		assert.Equal(t, len(origin.body), len(body), "a stale If-Range must return the whole entity")
	})
}

// TestRangeRequest_ConditionalsHandledByServeContent covers the §4.4 change: the
// hand-rolled 304 check is skipped on the range path so http.ServeContent owns every
// conditional header with one complete implementation.
//
// Note the RFC 9110 §13.2.1 ordering, which an earlier draft of this test got wrong:
// If-None-Match is step 3 and If-Range is step 5, so a MATCHING If-None-Match
// correctly yields 304 even when Range is present. What must not happen is a range
// request being decided by the less complete local implementation.
func TestRangeRequest_ConditionalsHandledByServeContent(t *testing.T) {
	s, origin := rangeTestSidekick(t)
	const path = "/wp-content/uploads/clip.mp4"

	readAll(t, doGet(t, s, origin, path, nil))

	t.Run("matching If-None-Match yields 304 per step 3", func(t *testing.T) {
		resp := doGet(t, s, origin, path, map[string]string{
			"Range":         "bytes=0-99",
			"If-None-Match": `"abc123"`,
			"If-Range":      `"abc123"`,
		})
		readAll(t, resp)
		assert.Equal(t, http.StatusNotModified, resp.StatusCode)
	})

	t.Run("non-matching If-None-Match falls through to the range", func(t *testing.T) {
		resp := doGet(t, s, origin, path, map[string]string{
			"Range":         "bytes=0-99",
			"If-None-Match": `"stale"`,
			"If-Range":      `"abc123"`,
		})
		body := readAll(t, resp)
		assert.Equal(t, http.StatusPartialContent, resp.StatusCode)
		assert.Equal(t, origin.body[0:100], body)
	})

	t.Run("weak validator is matched weakly", func(t *testing.T) {
		resp := doGet(t, s, origin, path, map[string]string{
			"Range":         "bytes=0-99",
			"If-None-Match": `W/"abc123"`,
		})
		readAll(t, resp)
		assert.Equal(t, http.StatusNotModified, resp.StatusCode)
	})
}

func TestRangeFill_DisabledFallsBackToPassThrough(t *testing.T) {
	s, origin := rangeTestSidekick(t)
	disabled := false
	s.RangeFill = &disabled

	resp := doGet(t, s, origin, "/wp-content/uploads/clip.mp4", map[string]string{"Range": "bytes=0-99"})
	readAll(t, resp)

	// Without the fill the origin sees the Range and answers it directly.
	assert.Equal(t, "bytes=0-99", origin.lastRnge)
	assert.Equal(t, http.StatusPartialContent, resp.StatusCode)
}

func TestRangeFill_OversizedObjectIsRelayedNotCached(t *testing.T) {
	s, origin := rangeTestSidekick(t)
	s.RangeFillMaxSize = 1024 // far below the 64KB body

	resp := doGet(t, s, origin, "/wp-content/uploads/big.mp4", map[string]string{"Range": "bytes=0-99"})
	body := readAll(t, resp)

	// Relayed verbatim: the full 200 the origin produced, not a 206.
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, len(origin.body), len(body))
}

func TestRangeFill_NonOKUpstreamIsRelayed(t *testing.T) {
	s, _ := rangeTestSidekick(t)

	notFound := caddyhttp.HandlerFunc(func(w http.ResponseWriter, r *http.Request) error {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusNotFound)
		_, err := w.Write([]byte("nope"))
		return err
	})

	req := httptest.NewRequest("GET", "/wp-content/uploads/missing.mp4", nil)
	req.Header.Set("Range", "bytes=0-99")
	rec := httptest.NewRecorder()
	require.NoError(t, s.ServeHTTP(rec, req, notFound))

	resp := rec.Result()
	body := readAll(t, resp)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	assert.Equal(t, "nope", string(body))
}

// TestRangeHit_CompressedEntryIsNotRanged verifies precondition P2: byte offsets are
// meaningless against a compressed representation, so such an entry must be served
// whole rather than ranged.
func TestRangeHit_CompressedEntryIsNotRanged(t *testing.T) {
	s, _ := rangeTestSidekick(t)

	meta := &Metadata{
		StateCode: http.StatusOK,
		Timestamp: time.Now().Unix(),
		Header: [][]string{
			{"Content-Type", "text/html"},
			{"Content-Encoding", "gzip"},
		},
	}
	payload := []byte("pretend-gzip-bytes")

	req := httptest.NewRequest("GET", "/page.html", nil)
	req.Header.Set("Range", "bytes=0-4")
	req.Header.Set("Accept-Encoding", "gzip")
	key, _ := s.buildCacheKey(req)
	require.NoError(t, s.Storage.SetWithKey(key, meta, payload))

	rec := httptest.NewRecorder()
	require.NoError(t, s.ServeHTTP(rec, req, caddyhttp.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) error {
			t.Fatal("should have been served from cache")
			return nil
		})))

	resp := rec.Result()
	readAll(t, resp)
	assert.Equal(t, http.StatusOK, resp.StatusCode,
		"a compressed entry must not be served as a partial response")
}

func TestShouldReturn304_ConditionalPrecedence(t *testing.T) {
	s := &Sidekick{logger: zap.NewNop()}
	lastMod := time.Date(2026, 2, 24, 21, 6, 11, 0, time.UTC)

	meta := &Metadata{
		StateCode: 200,
		Header: [][]string{
			{"Etag", `"abc123"`},
			{"Last-Modified", lastMod.Format(http.TimeFormat)},
		},
	}

	tests := []struct {
		name            string
		ifNoneMatch     string
		ifModifiedSince string
		want            bool
	}{
		{"exact etag", `"abc123"`, "", true},
		{"etag list containing match", `"other", "abc123"`, "", true},
		{"etag list without match", `"one", "two"`, "", false},
		{"weak request validator", `W/"abc123"`, "", true},
		{"wildcard", `*`, "", true},
		{"no etag match", `"nope"`, "", false},

		{"ims equal", "", lastMod.Format(http.TimeFormat), true},
		{"ims newer than entry", "", lastMod.Add(time.Hour).Format(http.TimeFormat), true},
		{"ims older than entry", "", lastMod.Add(-time.Hour).Format(http.TimeFormat), false},
		{"ims in another valid format", "", lastMod.Format(time.RFC850), true},
		{"ims unparseable", "", "not-a-date", false},

		// Step 4 is consulted ONLY when If-None-Match is absent. A non-matching
		// If-None-Match decides on its own, even though the date would have matched.
		{"non-matching etag wins over matching ims", `"nope"`, lastMod.Format(http.TimeFormat), false},

		{"neither header", "", "", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, s.shouldReturn304(meta, tc.ifNoneMatch, tc.ifModifiedSince))
		})
	}
}

func TestShouldReturn304_NoValidatorsStored(t *testing.T) {
	s := &Sidekick{logger: zap.NewNop()}
	meta := &Metadata{StateCode: 200, Header: [][]string{{"Content-Type", "text/html"}}}

	assert.False(t, s.shouldReturn304(meta, `"abc"`, ""))
	assert.False(t, s.shouldReturn304(meta, "", time.Now().Format(http.TimeFormat)))
	assert.False(t, s.shouldReturn304(nil, `"abc"`, ""))
}

func TestCachedModTime(t *testing.T) {
	want := time.Date(2026, 2, 24, 21, 6, 11, 0, time.UTC)

	got := cachedModTime(&Metadata{Header: [][]string{{"Last-Modified", want.Format(http.TimeFormat)}}})
	assert.True(t, want.Equal(got))

	assert.True(t, cachedModTime(nil).IsZero())
	assert.True(t, cachedModTime(&Metadata{}).IsZero())
	assert.True(t, cachedModTime(&Metadata{Header: [][]string{{"Last-Modified", "garbage"}}}).IsZero())
}

func TestEtagWeakMatch(t *testing.T) {
	assert.True(t, etagWeakMatch(`"x"`, `"x"`))
	assert.True(t, etagWeakMatch(`W/"x"`, `"x"`))
	assert.True(t, etagWeakMatch(`"x"`, `W/"x"`))
	assert.False(t, etagWeakMatch(`"x"`, `"y"`))
}

// TestRangeFill_CapturedBodyIsCachedIntact guards against the range fill storing a
// truncated or range-shaped body: the cached entry must be the whole representation.
func TestRangeFill_CapturedBodyIsCachedIntact(t *testing.T) {
	s, origin := rangeTestSidekick(t)
	const path = "/wp-content/uploads/clip.mp4"

	readAll(t, doGet(t, s, origin, path, map[string]string{"Range": "bytes=100-199"}))

	req := httptest.NewRequest("GET", path, nil)
	key, _ := s.buildCacheKey(req)

	data, meta, err := s.Storage.Get(key)
	require.NoError(t, err)
	require.NotNil(t, meta)
	assert.Equal(t, http.StatusOK, meta.StateCode, "the cached entry must be the full 200")
	assert.True(t, bytes.Equal(origin.body, data), "the whole representation must be cached")
	assert.Equal(t, "", meta.HeaderValue("Content-Range"),
		"a partial representation must never be stored")
}

func TestRangeFill_StreamedCaptureServesRange(t *testing.T) {
	s, origin := rangeTestSidekick(t)
	// Force the capture to spool to a temp file rather than stay in the buffer, so
	// the range is served straight off disk.
	s.CacheMemoryStreamToDiskSize = 4 * 1024
	origin.body = mediaBody(256 * 1024)

	resp := doGet(t, s, origin, "/wp-content/uploads/large.mp4",
		map[string]string{"Range": "bytes=200000-200099"})
	body := readAll(t, resp)

	assert.Equal(t, http.StatusPartialContent, resp.StatusCode)
	assert.Equal(t, origin.body[200000:200100], body,
		"a disk-spooled capture must yield the correct byte window")
}

func TestNonRangeRequestsUnaffected(t *testing.T) {
	s, origin := rangeTestSidekick(t)
	const path = "/wp-content/uploads/clip.mp4"

	resp := doGet(t, s, origin, path, nil)
	body := readAll(t, resp)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "MISS", resp.Header.Get(CacheHeaderName))
	assert.Equal(t, origin.body, body)

	resp2 := doGet(t, s, origin, path, nil)
	body2 := readAll(t, resp2)
	assert.Equal(t, http.StatusOK, resp2.StatusCode)
	assert.Equal(t, "HIT", resp2.Header.Get(CacheHeaderName))
	assert.Equal(t, origin.body, body2)

	assert.False(t, strings.Contains(resp2.Header.Get("Vary"), "Cookie"))
}
