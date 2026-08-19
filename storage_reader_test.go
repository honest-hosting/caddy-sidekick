package sidekick

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func readerTestStorage(t *testing.T) *Storage {
	t.Helper()
	// Memory item cap of 1KB so anything larger lands on disk.
	return NewStorage(t.TempDir(), 3600, 1024, 64*1024, 100, 10*1024*1024, 100*1024*1024, 1000, zap.NewNop())
}

func TestGetReader_DiskEntryReturnsOpenFile(t *testing.T) {
	s := readerTestStorage(t)
	body := mediaBody(256 * 1024)
	meta := metaWith("Content-Type", "video/mp4")
	meta.StateCode = 200

	require.NoError(t, s.SetWithKey("vid", meta, body))

	reader, gotMeta, size, err := s.GetReader("vid")
	require.NoError(t, err)
	defer func() { _ = reader.Close() }()

	// A disk-backed entry must come back as a real file, not a copy in memory —
	// that is the entire point of this path.
	_, isFile := reader.(*os.File)
	assert.True(t, isFile, "disk entries must be served from an open file")

	assert.Equal(t, int64(len(body)), size)
	assert.Equal(t, 200, gotMeta.StateCode)

	got, err := io.ReadAll(reader)
	require.NoError(t, err)
	assert.True(t, bytes.Equal(body, got), "streamed bytes must match what was stored")
}

func TestGetReader_MemoryEntryIsServedInline(t *testing.T) {
	s := readerTestStorage(t)
	body := []byte("small enough for the memory tier")
	meta := metaWith("Content-Type", "text/plain")
	meta.StateCode = 200

	require.NoError(t, s.SetWithKey("small", meta, body))

	reader, _, size, err := s.GetReader("small")
	require.NoError(t, err)
	defer func() { _ = reader.Close() }()

	assert.Equal(t, int64(len(body)), size)
	got, err := io.ReadAll(reader)
	require.NoError(t, err)
	assert.Equal(t, body, got)
}

func TestGetReader_CompressedEntryIsNotStreamable(t *testing.T) {
	s := readerTestStorage(t)
	// Compressible text above the memory cap, so it is stored on disk AND compressed.
	body := compressibleBody(64 * 1024)
	meta := metaWith("Content-Type", "text/html")
	meta.StateCode = 200

	require.NoError(t, s.SetWithKey("page", meta, body))
	require.NotEqual(t, "", meta.HeaderValue("X-Compression-Type"),
		"precondition: this entry is compressed on disk")

	_, _, _, err := s.GetReader("page")
	assert.ErrorIs(t, err, ErrNotStreamable,
		"a compressed entry must send the caller to the buffered path")

	// ...and the buffered path still works for it.
	data, _, err := s.Get("page")
	require.NoError(t, err)
	assert.True(t, bytes.Equal(body, data))
}

func TestGetReader_MissAndExpiry(t *testing.T) {
	t.Run("miss", func(t *testing.T) {
		s := readerTestStorage(t)
		_, _, _, err := s.GetReader("absent")
		assert.ErrorIs(t, err, ErrCacheNotFound)
	})

	t.Run("expired", func(t *testing.T) {
		s := NewStorage(t.TempDir(), 1, 1024, 64*1024, 100, 10*1024*1024, 100*1024*1024, 1000, zap.NewNop())
		meta := metaWith("Content-Type", "video/mp4")
		meta.StateCode = 200
		require.NoError(t, s.SetWithKey("stale", meta, mediaBody(16*1024)))

		time.Sleep(1100 * time.Millisecond)

		_, _, _, err := s.GetReader("stale")
		assert.ErrorIs(t, err, ErrCacheExpired)
	})
}

// TestGetReader_SurvivesEvictionMidStream covers the safety argument in §6.2 for
// releasing fileMu before returning: eviction unlinks the file, but an already-open
// descriptor keeps the inode alive, so a response in flight still completes.
func TestGetReader_SurvivesEvictionMidStream(t *testing.T) {
	s := readerTestStorage(t)
	body := mediaBody(256 * 1024)
	meta := metaWith("Content-Type", "video/mp4")
	meta.StateCode = 200

	require.NoError(t, s.SetWithKey("doomed", meta, body))

	reader, _, _, err := s.GetReader("doomed")
	require.NoError(t, err)
	defer func() { _ = reader.Close() }()

	// Read a little, then evict the entry out from under the open reader.
	head := make([]byte, 1024)
	_, err = io.ReadFull(reader, head)
	require.NoError(t, err)

	require.NoError(t, s.Purge("doomed"))
	_, _, getErr := s.Get("doomed")
	require.Error(t, getErr, "precondition: the entry is really gone")

	// The rest of the body must still stream correctly.
	rest, err := io.ReadAll(reader)
	require.NoError(t, err)

	assert.Equal(t, body[:1024], head)
	assert.True(t, bytes.Equal(body[1024:], rest),
		"an in-flight response must survive eviction of its entry")
}

// TestGetReader_DoesNotHoldFileLock is the other half of §6.2. fileMu is
// Storage-wide, so holding it for the duration of a response would stall every write
// in the process while one slow client drains a large body.
func TestGetReader_DoesNotHoldFileLock(t *testing.T) {
	s := readerTestStorage(t)
	meta := metaWith("Content-Type", "video/mp4")
	meta.StateCode = 200
	require.NoError(t, s.SetWithKey("held", meta, mediaBody(256*1024)))

	reader, _, _, err := s.GetReader("held")
	require.NoError(t, err)
	defer func() { _ = reader.Close() }()

	// With the reader still open, an unrelated write must not block.
	done := make(chan error, 1)
	go func() {
		other := metaWith("Content-Type", "video/mp4")
		other.StateCode = 200
		done <- s.SetWithKey("other", other, mediaBody(128*1024))
	}()

	select {
	case err := <-done:
		assert.NoError(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("a write blocked while a streamed read was open: fileMu is being held across the response")
	}
}

func TestClientWantsCompression(t *testing.T) {
	cases := []struct {
		header string
		want   bool
	}{
		{"gzip, deflate, br", true},
		{"br", true},
		{"zstd", true},
		{"identity", false},
		{"*", false},
		// The exact header Chrome sends for media, which must stream rather than
		// be recompressed.
		{"identity;q=1, *;q=0", false},
		{"", false},
		{"deflate", false},
		{"gzip;q=1.0", true},
	}

	for _, tc := range cases {
		t.Run(tc.header, func(t *testing.T) {
			// Matches how ServeHTTP splits the header.
			assert.Equal(t, tc.want, clientWantsCompression(strings.Split(tc.header, ",")))
		})
	}
}

// TestStreamedHit_EndToEnd exercises the streaming path through ServeHTTP for both a
// whole-body hit and a range hit, confirming it is byte-equivalent to the buffered
// path it replaces.
func TestStreamedHit_EndToEnd(t *testing.T) {
	s, origin := rangeTestSidekick(t)
	const path = "/wp-content/uploads/streamed.mp4"

	// Prime.
	readAll(t, doGet(t, s, origin, path, nil))

	t.Run("whole body", func(t *testing.T) {
		resp := doGet(t, s, origin, path, nil)
		body := readAll(t, resp)
		assert.Equal(t, http.StatusOK, resp.StatusCode)
		assert.Equal(t, "HIT", resp.Header.Get(CacheHeaderName))
		assert.Equal(t, origin.body, body)
		assert.Equal(t, "video/mp4", resp.Header.Get("Content-Type"))
	})

	t.Run("range", func(t *testing.T) {
		resp := doGet(t, s, origin, path, map[string]string{"Range": "bytes=1000-1999"})
		body := readAll(t, resp)
		assert.Equal(t, http.StatusPartialContent, resp.StatusCode)
		assert.Equal(t, origin.body[1000:2000], body)
	})

	t.Run("conditional still 304s", func(t *testing.T) {
		resp := doGet(t, s, origin, path, map[string]string{"If-None-Match": `"abc123"`})
		readAll(t, resp)
		assert.Equal(t, http.StatusNotModified, resp.StatusCode)
	})
}

// TestStreamedHit_NonOKEntriesFallThrough ensures cached redirects keep their own
// status code rather than being flattened to 200 by ServeContent.
func TestStreamedHit_NonOKEntriesFallThrough(t *testing.T) {
	s, _ := rangeTestSidekick(t)

	meta := &Metadata{
		StateCode: http.StatusMovedPermanently,
		Timestamp: time.Now().Unix(),
		Header: [][]string{
			{"Location", "https://example.com/new"},
			{"Content-Type", "text/html"},
		},
	}
	req := httptest.NewRequest("GET", "/old-page", nil)
	key, _ := s.buildCacheKey(req)
	require.NoError(t, s.Storage.SetWithKey(key, meta, []byte("moved")))

	rec := httptest.NewRecorder()
	require.NoError(t, s.ServeHTTP(rec, req, caddyhttp.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) error {
			t.Fatal("should have been served from cache")
			return nil
		})))

	resp := rec.Result()
	readAll(t, resp)
	assert.Equal(t, http.StatusMovedPermanently, resp.StatusCode,
		"a cached redirect must keep its status")
	assert.Equal(t, "https://example.com/new", resp.Header.Get("Location"))
}

// TestStreamedHit_CompressionStillAppliesWhenRequested confirms the fast path yields
// to the buffered path when the client actually wants an encoding applied.
func TestStreamedHit_CompressionStillAppliesWhenRequested(t *testing.T) {
	s, _ := rangeTestSidekick(t)

	meta := &Metadata{
		StateCode: http.StatusOK,
		Timestamp: time.Now().Unix(),
		Header:    [][]string{{"Content-Type", "text/html"}},
	}
	payload := compressibleBody(8 * 1024)

	req := httptest.NewRequest("GET", "/page.html", nil)
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
	body := readAll(t, resp)

	assert.Equal(t, "gzip", resp.Header.Get("Content-Encoding"),
		"a client asking for gzip must still get gzip")
	assert.Less(t, len(body), len(payload))
}
