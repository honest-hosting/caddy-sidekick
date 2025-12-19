package sidekick

import (
	"bytes"
	"compress/gzip"
	"io"
	"testing"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
)

func TestCompressGzip(t *testing.T) {
	testData := []byte("Hello, World! This is a test string that should be compressed.")

	compressed, err := CompressGzip(testData)
	if err != nil {
		t.Fatalf("Failed to compress with gzip: %v", err)
	}

	// Verify it's actually gzip compressed by trying to decompress it
	reader, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		t.Fatalf("Failed to create gzip reader: %v", err)
	}
	defer func() { _ = reader.Close() }()

	decompressed, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("Failed to decompress gzip: %v", err)
	}

	if !bytes.Equal(decompressed, testData) {
		t.Errorf("Decompressed data doesn't match original")
	}
}

func TestCompressBrotli(t *testing.T) {
	testData := []byte("Hello, World! This is a test string that should be compressed.")

	compressed, err := CompressBrotli(testData)
	if err != nil {
		t.Fatalf("Failed to compress with brotli: %v", err)
	}

	// Verify it's actually brotli compressed by trying to decompress it
	reader := brotli.NewReader(bytes.NewReader(compressed))
	decompressed, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("Failed to decompress brotli: %v", err)
	}

	if !bytes.Equal(decompressed, testData) {
		t.Errorf("Decompressed data doesn't match original")
	}
}

func TestCompressZstd(t *testing.T) {
	testData := []byte("Hello, World! This is a test string that should be compressed.")

	compressed, err := CompressZstd(testData)
	if err != nil {
		t.Fatalf("Failed to compress with zstd: %v", err)
	}

	// Verify it's actually zstd compressed by trying to decompress it
	decoder, err := zstd.NewReader(nil)
	if err != nil {
		t.Fatalf("Failed to create zstd decoder: %v", err)
	}
	defer decoder.Close()

	decompressed, err := decoder.DecodeAll(compressed, nil)
	if err != nil {
		t.Fatalf("Failed to decompress zstd: %v", err)
	}

	if !bytes.Equal(decompressed, testData) {
		t.Errorf("Decompressed data doesn't match original")
	}
}

func TestCompressForClient(t *testing.T) {
	testData := []byte("Hello, World! This is a test string that should be compressed.")

	testCases := []struct {
		name     string
		encoding string
		verify   func(compressed []byte) error
	}{
		{
			name:     "gzip",
			encoding: "gzip",
			verify: func(compressed []byte) error {
				reader, err := gzip.NewReader(bytes.NewReader(compressed))
				if err != nil {
					return err
				}
				defer func() { _ = reader.Close() }()
				decompressed, err := io.ReadAll(reader)
				if err != nil {
					return err
				}
				if !bytes.Equal(decompressed, testData) {
					t.Errorf("gzip: decompressed data doesn't match")
				}
				return nil
			},
		},
		{
			name:     "brotli",
			encoding: "br",
			verify: func(compressed []byte) error {
				reader := brotli.NewReader(bytes.NewReader(compressed))
				decompressed, err := io.ReadAll(reader)
				if err != nil {
					return err
				}
				if !bytes.Equal(decompressed, testData) {
					t.Errorf("brotli: decompressed data doesn't match")
				}
				return nil
			},
		},
		{
			name:     "zstd",
			encoding: "zstd",
			verify: func(compressed []byte) error {
				decoder, err := zstd.NewReader(nil)
				if err != nil {
					return err
				}
				defer decoder.Close()
				decompressed, err := decoder.DecodeAll(compressed, nil)
				if err != nil {
					return err
				}
				if !bytes.Equal(decompressed, testData) {
					t.Errorf("zstd: decompressed data doesn't match")
				}
				return nil
			},
		},
		{
			name:     "identity",
			encoding: "identity",
			verify: func(data []byte) error {
				if !bytes.Equal(data, testData) {
					t.Errorf("identity: data was modified")
				}
				return nil
			},
		},
		{
			name:     "none",
			encoding: "none",
			verify: func(data []byte) error {
				if !bytes.Equal(data, testData) {
					t.Errorf("none: data was modified")
				}
				return nil
			},
		},
		{
			name:     "empty",
			encoding: "",
			verify: func(data []byte) error {
				if !bytes.Equal(data, testData) {
					t.Errorf("empty: data was modified")
				}
				return nil
			},
		},
		{
			name:     "unknown",
			encoding: "unknown-encoding",
			verify: func(data []byte) error {
				if !bytes.Equal(data, testData) {
					t.Errorf("unknown: data was modified (should return uncompressed)")
				}
				return nil
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			compressed, err := CompressForClient(testData, tc.encoding)
			if err != nil {
				t.Fatalf("Failed to compress with %s: %v", tc.encoding, err)
			}

			if err := tc.verify(compressed); err != nil {
				t.Fatalf("Verification failed for %s: %v", tc.encoding, err)
			}
		})
	}
}

func TestCompressionWithVariousSizes(t *testing.T) {
	// Test with various data sizes to ensure compression works correctly
	testCases := []struct {
		name string
		data []byte
	}{
		{"empty", []byte{}},
		{"small", []byte("Hello")},
		{"medium", bytes.Repeat([]byte("Hello, World! "), 100)},
		{"large", bytes.Repeat([]byte("The quick brown fox jumps over the lazy dog. "), 1000)},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Test gzip
			t.Run("gzip", func(t *testing.T) {
				compressed, err := CompressGzip(tc.data)
				if err != nil {
					t.Fatalf("Failed to compress: %v", err)
				}

				// Verify by decompressing
				if len(tc.data) > 0 {
					reader, err := gzip.NewReader(bytes.NewReader(compressed))
					if err != nil {
						t.Fatalf("Failed to create reader: %v", err)
					}
					defer func() { _ = reader.Close() }()
					decompressed, err := io.ReadAll(reader)
					if err != nil {
						t.Fatalf("Failed to decompress: %v", err)
					}
					if !bytes.Equal(decompressed, tc.data) {
						t.Errorf("Data mismatch after compression/decompression")
					}
				}
			})

			// Test brotli
			t.Run("brotli", func(t *testing.T) {
				compressed, err := CompressBrotli(tc.data)
				if err != nil {
					t.Fatalf("Failed to compress: %v", err)
				}

				// Verify by decompressing
				reader := brotli.NewReader(bytes.NewReader(compressed))
				decompressed, err := io.ReadAll(reader)
				if err != nil {
					t.Fatalf("Failed to decompress: %v", err)
				}
				if !bytes.Equal(decompressed, tc.data) {
					t.Errorf("Data mismatch after compression/decompression")
				}
			})

			// Test zstd
			t.Run("zstd", func(t *testing.T) {
				compressed, err := CompressZstd(tc.data)
				if err != nil {
					t.Fatalf("Failed to compress: %v", err)
				}

				// Verify by decompressing
				decoder, err := zstd.NewReader(nil)
				if err != nil {
					t.Fatalf("Failed to create decoder: %v", err)
				}
				defer decoder.Close()
				decompressed, err := decoder.DecodeAll(compressed, nil)
				if err != nil {
					t.Fatalf("Failed to decompress: %v", err)
				}
				if !bytes.Equal(decompressed, tc.data) {
					t.Errorf("Data mismatch after compression/decompression")
				}
			})
		})
	}
}
