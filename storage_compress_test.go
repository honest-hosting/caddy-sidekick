package sidekick

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// compressibleBody returns highly compressible text of the requested size, so any
// working compressor comfortably clears the 10% savings threshold.
func compressibleBody(size int) []byte {
	return bytes.Repeat([]byte("the quick brown fox jumps over the lazy dog. "), size/45+1)[:size]
}

func metaWith(pairs ...string) *Metadata {
	m := &Metadata{}
	for i := 0; i+1 < len(pairs); i += 2 {
		m.Header = append(m.Header, []string{pairs[i], pairs[i+1]})
	}
	return m
}

func testStorage(t *testing.T) *Storage {
	t.Helper()
	s := NewStorage(t.TempDir(), 3600, 512*1024, 1024*1024, 100, 10*1024*1024, 100*1024*1024, 1000, zap.NewNop())
	return s
}

func TestCompressData_SkipsIncompressibleContentTypes(t *testing.T) {
	s := testStorage(t)
	body := compressibleBody(64 * 1024)

	// Deliberately compressible bytes: if the guard is not honored these WOULD be
	// compressed, so a "" result proves the content type alone short-circuited it.
	skipped := []string{
		"video/mp4",
		"video/webm",
		"audio/mpeg",
		"image/jpeg",
		"image/png",
		"image/webp",
		"image/avif",
		"application/zip",
		"application/gzip",
		"application/pdf",
		"application/x-7z-compressed",
		"font/woff2",
		"video/mp4; codecs=\"avc1.42E01E\"",
		"IMAGE/PNG",
	}

	for _, ct := range skipped {
		t.Run(ct, func(t *testing.T) {
			out, ctype := s.compressData(body, metaWith("Content-Type", ct))
			assert.Equal(t, "", ctype, "must not compress %s", ct)
			assert.Equal(t, len(body), len(out), "body must be stored verbatim")
		})
	}
}

func TestCompressData_StillCompressesTextTypes(t *testing.T) {
	s := testStorage(t)
	body := compressibleBody(64 * 1024)

	compressed := []string{
		"text/html; charset=UTF-8",
		"text/css",
		"application/javascript",
		"application/json",
		"image/svg+xml",
		"image/bmp",
		"image/x-icon",
	}

	for _, ct := range compressed {
		t.Run(ct, func(t *testing.T) {
			out, ctype := s.compressData(body, metaWith("Content-Type", ct))
			assert.NotEqual(t, "", ctype, "%s should still be compressed", ct)
			assert.Less(t, len(out), len(body))
		})
	}
}

func TestCompressData_SkipsAboveCompressMaxSize(t *testing.T) {
	s := testStorage(t)
	s.SetCompressMaxSize(32 * 1024)

	meta := metaWith("Content-Type", "text/html")

	under := compressibleBody(16 * 1024)
	out, ctype := s.compressData(under, meta)
	assert.NotEqual(t, "", ctype, "bodies under the ceiling are still compressed")
	assert.Less(t, len(out), len(under))

	over := compressibleBody(64 * 1024)
	out, ctype = s.compressData(over, meta)
	assert.Equal(t, "", ctype, "bodies over the ceiling must be stored verbatim")
	assert.Equal(t, len(over), len(out))
}

func TestCompressData_UnlimitedCeilingRestoresOldBehavior(t *testing.T) {
	s := testStorage(t)
	s.SetCompressMaxSize(-1) // unlimited

	body := compressibleBody(4 * 1024 * 1024)
	out, ctype := s.compressData(body, metaWith("Content-Type", "text/html"))

	assert.NotEqual(t, "", ctype, "an unlimited ceiling must not skip on size")
	assert.Less(t, len(out), len(body))
}

func TestCompressData_SkipsAlreadyEncodedBodies(t *testing.T) {
	s := testStorage(t)
	body := compressibleBody(64 * 1024)

	for _, enc := range []string{"gzip", "br", "zstd"} {
		t.Run(enc, func(t *testing.T) {
			out, ctype := s.compressData(body,
				metaWith("Content-Type", "text/html", "Content-Encoding", enc))
			assert.Equal(t, "", ctype, "must not recompress a %s-encoded body", enc)
			assert.Equal(t, len(body), len(out))
		})
	}

	// "none" and "identity" mean unencoded and must NOT suppress compression.
	for _, enc := range []string{"none", "identity"} {
		t.Run(enc, func(t *testing.T) {
			_, ctype := s.compressData(body,
				metaWith("Content-Type", "text/html", "Content-Encoding", enc))
			assert.NotEqual(t, "", ctype, "%q means unencoded and should compress", enc)
		})
	}
}

func TestCompressData_NilMetadataStillCompresses(t *testing.T) {
	s := testStorage(t)
	body := compressibleBody(64 * 1024)

	out, ctype := s.compressData(body, nil)
	assert.NotEqual(t, "", ctype, "nil metadata must not disable compression")
	assert.Less(t, len(out), len(body))
}

func TestCompressData_BelowMinimumSizeIsUntouched(t *testing.T) {
	s := testStorage(t)
	body := compressibleBody(512)

	out, ctype := s.compressData(body, metaWith("Content-Type", "text/html"))
	assert.Equal(t, "", ctype)
	assert.Equal(t, len(body), len(out))
}

// TestCompressionBaseline covers the nil-gzip comparison bug directly. Brotli was
// compared against len(gzipData), which is 0 when CompressGzip errors, making the
// condition unsatisfiable so brotli could never be selected on that path.
//
// This is unit-tested rather than driven through compressData because the path is
// only reachable when gzip itself fails, which cannot be induced from the outside.
func TestCompressionBaseline(t *testing.T) {
	dataSize := 100_000

	t.Run("gzip succeeded", func(t *testing.T) {
		gzip := make([]byte, 4_000)
		assert.Equal(t, 4_000, compressionBaseline(dataSize, gzip, nil),
			"a later compressor must beat gzip's output")
	})

	t.Run("gzip errored", func(t *testing.T) {
		assert.Equal(t, dataSize, compressionBaseline(dataSize, nil, assert.AnError),
			"with no gzip result the baseline is the original size, not zero")
	})

	t.Run("gzip returned empty without error", func(t *testing.T) {
		assert.Equal(t, dataSize, compressionBaseline(dataSize, []byte{}, nil),
			"an empty result must not produce an unsatisfiable zero baseline")
	})
}

// TestCompressData_GzipShortCircuits documents the selection semantics this change
// deliberately preserved: gzip is tried first and returned immediately when it saves
// at least 10%, so brotli is not attempted for ordinary text. Changing that would
// alter stored artifacts and CPU cost, which is out of scope here.
func TestCompressData_GzipShortCircuits(t *testing.T) {
	s := testStorage(t)

	body := compressibleBody(256 * 1024)
	s.SetCompressMaxSize(-1) // keep the size guard out of this assertion

	out, ctype := s.compressData(body, metaWith("Content-Type", "text/html"))

	require.Equal(t, "gzip", ctype, "gzip clears the ratio threshold and short-circuits")
	gz, err := CompressGzip(body)
	require.NoError(t, err)
	assert.Equal(t, len(gz), len(out))
}

func TestIsIncompressibleContentType(t *testing.T) {
	cases := map[string]bool{
		"video/mp4":                true,
		"audio/ogg":                true,
		"image/jpeg":               true,
		"image/heic":               true, // unknown image types default to compressed
		"image/svg+xml":            true, // carve-out below flips this
		"application/pdf":          true,
		"text/html":                false,
		"application/json":         false,
		"application/x-tar":        false, // plain tar is NOT compressed
		"":                         false,
		"text/html; charset=utf-8": false,
	}
	// image/svg+xml is a carve-out: XML text, genuinely compressible.
	cases["image/svg+xml"] = false

	for ct, want := range cases {
		assert.Equal(t, want, isIncompressibleContentType(ct), "content type %q", ct)
	}
}

// TestStoreDataToDisk_MediaRoundTripsUncompressed is the end-to-end check: a media
// body must land on disk verbatim, with no X-Compression-Type recorded, and read back
// byte-identical. Storing it uncompressed is also the precondition for serving byte
// ranges directly out of the cached representation.
func TestStoreDataToDisk_MediaRoundTripsUncompressed(t *testing.T) {
	s := testStorage(t)

	body := compressibleBody(128 * 1024)
	meta := metaWith("Content-Type", "video/mp4")
	meta.StateCode = 200

	require.NoError(t, s.SetWithKey("media-key", meta, body))

	assert.Equal(t, "", meta.HeaderValue("X-Compression-Type"),
		"media must not be marked as compressed on disk")

	got, gotMeta, err := s.Get("media-key")
	require.NoError(t, err)
	require.NotNil(t, gotMeta)
	assert.True(t, bytes.Equal(body, got), "body must round-trip byte-identical")
}
