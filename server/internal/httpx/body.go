package httpx

import (
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

var errWireLimitExceeded = errors.New("wire limit exceeded")

type wireLimitReader struct {
	r    io.Reader
	read int64
	max  int64
}

func (w *wireLimitReader) Read(p []byte) (int, error) {
	if w.read > w.max {
		return 0, errWireLimitExceeded
	}
	remaining := w.max + 1 - w.read
	if int64(len(p)) > remaining {
		p = p[:remaining]
	}
	n, err := w.r.Read(p)
	w.read += int64(n)
	if w.read > w.max {
		return n, errWireLimitExceeded
	}
	return n, err
}

// ReadBody reads an external response with independent wire and decoded limits.
// Gzip responses stream through a wire-bounded reader so compressed and decoded
// payloads do not coexist as two full-size heap buffers.
func ReadBody(resp *http.Response, maxWireBytes, maxDecodedBytes int64) ([]byte, error) {
	if resp.ContentLength > maxWireBytes {
		return nil, fmt.Errorf("response body exceeds %d bytes", maxWireBytes)
	}
	if !strings.EqualFold(resp.Header.Get("Content-Encoding"), "gzip") {
		wire, err := io.ReadAll(io.LimitReader(resp.Body, maxWireBytes+1))
		if err != nil {
			return nil, err
		}
		if int64(len(wire)) > maxWireBytes {
			return nil, fmt.Errorf("response body exceeds %d bytes", maxWireBytes)
		}
		if int64(len(wire)) > maxDecodedBytes {
			return nil, fmt.Errorf("decoded response exceeds %d bytes", maxDecodedBytes)
		}
		return wire, nil
	}
	wireReader := &wireLimitReader{r: resp.Body, max: maxWireBytes}
	zr, err := gzip.NewReader(wireReader)
	if err != nil {
		if errors.Is(err, errWireLimitExceeded) {
			return nil, fmt.Errorf("response body exceeds %d bytes", maxWireBytes)
		}
		return nil, fmt.Errorf("gzip: %w", err)
	}
	defer zr.Close()
	decoded, err := io.ReadAll(io.LimitReader(zr, maxDecodedBytes+1))
	if err != nil {
		if errors.Is(err, errWireLimitExceeded) {
			return nil, fmt.Errorf("response body exceeds %d bytes", maxWireBytes)
		}
		return nil, err
	}
	if int64(len(decoded)) > maxDecodedBytes {
		return nil, fmt.Errorf("decoded response exceeds %d bytes", maxDecodedBytes)
	}
	return decoded, nil
}
