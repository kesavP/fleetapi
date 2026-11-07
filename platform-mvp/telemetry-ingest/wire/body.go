// Package wire turns what the Coroot agents send (Prometheus remote-write,
// OTLP traces and logs, pprof profiles) into model rows. It has no network or
// storage code, so every decoder is testable with plain byte slices.
package wire

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"errors"
	"fmt"
	"io"

	"github.com/golang/snappy"
	"github.com/klauspost/compress/zstd"
)

// ErrTooLarge means a body, or what it decompresses to, passed the limit.
var ErrTooLarge = errors.New("request body too large")

// Decompress undoes a Content-Encoding. The output is capped at max bytes so
// a small compressed body cannot expand without bound.
func Decompress(encoding string, body []byte, max int64) ([]byte, error) {
	var r io.Reader
	switch encoding {
	case "", "identity":
		if int64(len(body)) > max {
			return nil, ErrTooLarge
		}
		return body, nil
	case "gzip":
		zr, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		defer zr.Close()
		r = zr
	case "zlib", "deflate":
		zr, err := zlib.NewReader(bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		defer zr.Close()
		r = zr
	case "zstd":
		zr, err := zstd.NewReader(bytes.NewReader(body), zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(uint64(max)))
		if err != nil {
			return nil, err
		}
		defer zr.Close()
		r = zr
	case "snappy":
		r = snappy.NewReader(bytes.NewReader(body)) // framed format, as OTLP exporters use it
	default:
		return nil, fmt.Errorf("unsupported content encoding %q", encoding)
	}
	out, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(out)) > max {
		return nil, ErrTooLarge
	}
	return out, nil
}
