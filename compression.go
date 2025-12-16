package sidekick

import (
	"bytes"
	"compress/gzip"
	"io"

	"github.com/andybalholm/brotli"
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

	return buf.Bytes(), nil
}

// DecompressGzip decompresses gzip data
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
func DecompressBrotli(data []byte) ([]byte, error) {
	reader := brotli.NewReader(bytes.NewReader(data))
	return io.ReadAll(reader)
}
