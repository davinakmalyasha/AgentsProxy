package notify

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeSender struct {
	mu       sync.Mutex
	calls    []string
	failures int // fail this many times before succeeding
}

func (f *fakeSender) Send(_ context.Context, to, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, to+":"+text)
	if f.failures > 0 {
		f.failures--
		return errors.New("provider down")
	}
	return nil
}

func (f *fakeSender) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeSender) last() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		return ""
	}
	return f.calls[len(f.calls)-1]
}

func TestQueue_SendsOnce(t *testing.T) {
	f := &fakeSender{}
	q := New(4)
	q.Start(f)
	q.Enqueue(Job{To: "+6281", Text: "hello"})
	// Give the worker a moment.
	deadline := time.Now().Add(2 * time.Second)
	for f.count() < 1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	q.Stop()
	if f.count() != 1 {
		t.Fatalf("calls=%d want 1", f.count())
	}
	if !strings.HasPrefix(f.last(), "+6281:") {
		t.Errorf("last=%q", f.last())
	}
}

func TestQueue_RetriesThenSucceeds(t *testing.T) {
	f := &fakeSender{failures: 2} // fail twice, succeed on 3rd
	q := New(4)
	q.Start(f)
	q.Enqueue(Job{To: "+6281", Text: "retry me"})
	deadline := time.Now().Add(5 * time.Second)
	for f.count() < 3 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	q.Stop()
	if f.count() != 3 {
		t.Fatalf("calls=%d want 3 (2 failures + 1 success)", f.count())
	}
}

func TestQueue_GivesUpAfterMaxAttempts(t *testing.T) {
	f := &fakeSender{failures: 99} // never succeeds
	q := New(4)
	q.Start(f)
	q.Enqueue(Job{To: "+6281", Text: "doomed"})
	// With maxAttempts=3 and backoff 1s+2s, this takes ~3s. Use a
	// generous deadline.
	deadline := time.Now().Add(8 * time.Second)
	for f.count() < 3 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	q.Stop()
	if f.count() != 3 {
		t.Fatalf("calls=%d want 3 (max attempts)", f.count())
	}
}

func TestWaMeLink_Encodes(t *testing.T) {
	link := MakeWaMeLink("628123", "Booking confirmed: Court 3")
	if !strings.Contains(link, "wa.me/628123?text=") {
		t.Errorf("link=%q", link)
	}
	if strings.Contains(link, " ") {
		t.Errorf("link has raw space: %q", link)
	}
}

func TestQueue_EnqueueNeverBlocks(t *testing.T) {
	// Buffer of 1, fill it, then Enqueue more — must not block.
	q := New(1)
	// Don't start a worker; the buffer stays full.
	q.ch <- Job{To: "a", Text: "x"}
	done := make(chan struct{})
	go func() {
		q.Enqueue(Job{To: "b", Text: "y"})
		q.Enqueue(Job{To: "c", Text: "z"})
		close(done)
	}()
	select {
	case <-done:
		// good — Enqueue returned without blocking
	case <-time.After(time.Second):
		t.Fatal("Enqueue blocked on a full buffer")
	}
}
