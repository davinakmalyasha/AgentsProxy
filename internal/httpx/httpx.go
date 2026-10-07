// Package httpx owns the shared HTTP transport for all outbound
// provider/storage calls (LLM, ADK model, Supabase Storage).
//
// WHY: every client was built with &http.Client{Timeout} and a nil
// Transport, i.e. http.DefaultTransport with MaxIdleConnsPerHost: 2.
// The LLM API is ONE host and every agent turn makes 2-5 sequential
// provider calls — above 2 in-flight requests Go closes the connection
// after each response and pays a fresh TCP+TLS handshake per call.
// One shared transport with a sized per-host pool fixes all three
// clients at once. ResponseHeaderTimeout bounds a provider that
// accepts TCP and never sends headers (otherwise the full 60s burns).
package httpx

import (
	"net/http"
	"sync"
	"time"
)

var (
	transportOnce sync.Once
	transport     *http.Transport
)

// SharedTransport returns the process-wide tuned transport, built once.
func SharedTransport() *http.Transport {
	transportOnce.Do(func() {
		base, ok := http.DefaultTransport.(*http.Transport)
		if !ok {
			base = &http.Transport{}
		}
		t := base.Clone()
		t.MaxIdleConns = 100
		t.MaxIdleConnsPerHost = 32
		t.MaxConnsPerHost = 32
		t.IdleConnTimeout = 90 * time.Second
		t.TLSHandshakeTimeout = 5 * time.Second
		t.ResponseHeaderTimeout = 30 * time.Second
		t.ForceAttemptHTTP2 = true
		transport = t
	})
	return transport
}

// NewClient returns an HTTP client on the shared transport with the
// given total timeout.
func NewClient(timeout time.Duration) *http.Client {
	return &http.Client{Transport: SharedTransport(), Timeout: timeout}
}
