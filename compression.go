package sidekick

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
)

// CompressGzip compresses data using gzip
func CompressGzip(data []byte) ([]byte, error) {
	var buf bytes.Buffer
	writer := gzip.NewWriter(&buf)

	if _, err := writer.Write(data); err != nil {
		_ = writer.Close()
		return nil, err
	}

	if err := writer.Close(); err != nil {
		return nil, err
	}

	compressed := buf.Bytes()

	// Validate that we produced valid gzip data
	// Gzip files must start with magic bytes 0x1f 0x8b
	if len(compressed) < 2 || compressed[0] != 0x1f || compressed[1] != 0x8b {
		return nil, fmt.Errorf("failed to produce valid gzip data")
	}

	return compressed, nil
}

// DecompressGzip decompresses gzip data
// Used internally for disk storage decompression
func DecompressGzip(data []byte) ([]byte, error) {
	reader, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer func() { _ = reader.Close() }()

	return io.ReadAll(reader)
}

// CompressBrotli compresses data using brotli
func CompressBrotli(data []byte) ([]byte, error) {
	var buf bytes.Buffer
	writer := brotli.NewWriter(&buf)

	if _, err := writer.Write(data); err != nil {
		_ = writer.Close()
		return nil, err
	}

	if err := writer.Close(); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

// DecompressBrotli decompresses brotli data
// Used internally for disk storage decompression
func DecompressBrotli(data []byte) ([]byte, error) {
	reader := brotli.NewReader(bytes.NewReader(data))
	return io.ReadAll(reader)
}

// CompressZstd compresses data using zstd
func CompressZstd(data []byte) ([]byte, error) {
	encoder, err := zstd.NewWriter(nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = encoder.Close() }()

	return encoder.EncodeAll(data, nil), nil
}

// CompressForClient compresses data based on the requested encoding
func CompressForClient(data []byte, encoding string) ([]byte, error) {
	switch encoding {
	case "gzip":
		return CompressGzip(data)
	case "br":
		return CompressBrotli(data)
	case "zstd":
		return CompressZstd(data)
	case "none", "identity", "":
		return data, nil
	default:
		// Unknown encoding, return uncompressed
		return data, nil
	}
}
