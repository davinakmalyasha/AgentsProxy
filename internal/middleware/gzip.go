// DONE: perf (gzip responses) / STANDARDS H
package middleware

import (
	"compress/gzip"
	"io"
	"net/http"
	"strings"
	"sync"
)

// gzipThreshold is the minimum response size worth compressing. Below
// this the gzip framing costs more bytes than it saves.
const gzipThreshold = 1024

var gzipWriterPool = sync.Pool{
	New: func() any { return gzip.NewWriter(io.Discard) },
}

// Gzip compresses responses for clients that accept it. Threshold-gated:
// small JSON (errors, single rows) skips compression entirely, while
// list endpoints (200-5000 rows) and .xlsx downloads shrink 5-10x.
//
// Content-Length handling: the header is stripped before wrapping —
// net/http does not do that for middleware, and a stale length would
// truncate the compressed stream.
func Gzip(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			next.ServeHTTP(w, r)
			return
		}
		if r.Method == http.MethodHead || r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Del("Content-Length")
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Add("Vary", "Accept-Encoding")
		rec := &gzipRecorder{ResponseWriter: w}
		defer func() {
			rec.finish()
			if rec.gz != nil {
				_ = rec.gz.Close()
				rec.gz.Reset(io.Discard)
				gzipWriterPool.Put(rec.gz)
				rec.gz = nil
			}
		}()
		next.ServeHTTP(rec, r)
	})
}

// gzipRecorder buffers until the threshold is crossed: responses under
// it are sent uncompressed (headers rewritten back), so errors and
// tiny payloads never pay the gzip framing.
type gzipRecorder struct {
	http.ResponseWriter
	gz          *gzip.Writer
	wroteHeader bool
	passthrough bool
	buffered    []byte
}

// compressible reports whether the response body is worth compressing.
// Already-compressed formats (.xlsx, zip, images) cost CPU for zero
// bytes saved — worse, they can grow.
func (g *gzipRecorder) compressible() bool {
	ct := g.ResponseWriter.Header().Get("Content-Type")
	if ct == "" {
		return true // JSON handlers set it; default to compressing
	}
	for _, prefix := range []string{
		"application/json", "text/", "application/xml", "application/javascript",
	} {
		if strings.HasPrefix(ct, prefix) {
			return true
		}
	}
	return false
}

func (g *gzipRecorder) WriteHeader(status int) {
	if g.wroteHeader {
		return
	}
	g.wroteHeader = true
	// The handler may set Content-Length after Gzip stripped it at entry.
	// The wire body can be gzipped or pass through, so never trust it.
	g.ResponseWriter.Header().Del("Content-Length")
	if !g.compressible() {
		// Binary formats (.pdf, .xlsx, images) are already compressed:
		// remove the gzip envelope and stream the bytes untouched.
		g.passthrough = true
		g.ResponseWriter.Header().Del("Content-Encoding")
		g.ResponseWriter.Header().Del("Vary")
	}
	g.ResponseWriter.WriteHeader(status)
}

func (g *gzipRecorder) Write(b []byte) (int, error) {
	if g.passthrough {
		return g.ResponseWriter.Write(b)
	}
	if !g.wroteHeader {
		if !g.compressible() {
			// Already-compressed body: strip the gzip headers and
			// pass everything through untouched.
			g.passthrough = true
			g.ResponseWriter.Header().Del("Content-Encoding")
			g.ResponseWriter.Header().Del("Vary")
			if len(g.buffered) > 0 {
				if _, err := g.ResponseWriter.Write(g.buffered); err != nil {
					return 0, err
				}
				g.buffered = nil
			}
			return g.ResponseWriter.Write(b)
		}
		g.buffered = append(g.buffered, b...)
		if len(g.buffered) >= gzipThreshold {
			g.WriteHeader(http.StatusOK)
			n, err := g.writer().Write(g.buffered)
			g.buffered = nil
			return n, err
		}
		return len(b), nil
	}
	return g.writer().Write(b)
}

// writer lazily binds a pooled gzip writer: Close on an unused writer
// would emit bare framing bytes into an uncompressed response.
func (g *gzipRecorder) writer() *gzip.Writer {
	if g.gz == nil {
		g.gz = gzipWriterPool.Get().(*gzip.Writer)
		g.gz.Reset(g.ResponseWriter)
	}
	return g.gz
}

// finish flushes a response that never crossed the threshold:
// headers are rewritten back and the body goes out uncompressed.
func (g *gzipRecorder) finish() {
	if g.wroteHeader || g.passthrough {
		return
	}
	g.ResponseWriter.Header().Del("Content-Encoding")
	g.ResponseWriter.Header().Del("Vary")
	g.WriteHeader(http.StatusOK)
	if len(g.buffered) > 0 {
		_, _ = g.ResponseWriter.Write(g.buffered)
		g.buffered = nil
	}
}

// Flush implements http.Flusher so streaming handlers keep working.
// Flushing commits the response to gzip (no uncompressed fallback).
func (g *gzipRecorder) Flush() {
	if !g.wroteHeader {
		g.WriteHeader(http.StatusOK)
		if len(g.buffered) > 0 {
			_, _ = g.writer().Write(g.buffered)
			g.buffered = nil
		}
	}
	_ = g.writer().Flush()
	if f, ok := g.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap exposes the underlying writer for http.ResponseController.
func (g *gzipRecorder) Unwrap() http.ResponseWriter {
	return g.ResponseWriter
}
