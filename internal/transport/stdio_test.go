package transport

import (
	"context"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

// TestStdioTransportMultiStream drives the core assumption behind the stdio
// transport: multiple concurrent logical streams can be opened over ONE pipe
// (as the executor does — a StreamKindFS stream plus one StreamKindExec stream
// per subprocess). Server echoes each stream; client opens N in parallel and
// checks each round-trips independently.
func TestStdioTransportMultiStream(t *testing.T) {
	c1, c2 := net.Pipe()

	ln, err := NewStdioListener(c1, c1, c1)
	if err != nil {
		t.Fatalf("NewStdioListener: %v", err)
	}
	defer ln.Close()

	// Server: accept streams forever, echo each one back.
	go func() {
		for {
			s, err := ln.Accept()
			if err != nil {
				return
			}
			go func(s Stream) {
				defer s.Close()
				_, _ = io.Copy(s, s)
			}(s)
		}
	}()

	d, err := NewStdioDialer(c2, c2, c2)
	if err != nil {
		t.Fatalf("NewStdioDialer: %v", err)
	}
	defer d.Close()

	const n = 8
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			s, err := d.Dial(ctx)
			if err != nil {
				errs <- fmt.Errorf("stream %d dial: %w", i, err)
				return
			}
			defer s.Close()
			want := fmt.Sprintf("payload-for-stream-%d", i)
			if _, err := io.WriteString(s, want); err != nil {
				errs <- fmt.Errorf("stream %d write: %w", i, err)
				return
			}
			// Half-close our write side so the echo's io.Copy sees EOF and
			// stops — proving yamux stream half-close works in-band (the exact
			// semantic ssh -L mangled for raw unix sockets).
			if cw, ok := s.(interface{ CloseWrite() error }); ok {
				if err := cw.CloseWrite(); err != nil {
					errs <- fmt.Errorf("stream %d closewrite: %w", i, err)
					return
				}
			}
			got, err := io.ReadAll(s)
			if err != nil {
				errs <- fmt.Errorf("stream %d read: %w", i, err)
				return
			}
			if string(got) != want {
				errs <- fmt.Errorf("stream %d: got %q want %q", i, got, want)
				return
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

// TestStdioTransportLargePayload round-trips a payload larger than yamux's
// window so the framing/flow-control path is exercised, not just a single frame.
func TestStdioTransportLargePayload(t *testing.T) {
	c1, c2 := net.Pipe()
	ln, err := NewStdioListener(c1, c1, c1)
	if err != nil {
		t.Fatalf("NewStdioListener: %v", err)
	}
	defer ln.Close()
	go func() {
		s, err := ln.Accept()
		if err != nil {
			return
		}
		defer s.Close()
		_, _ = io.Copy(s, s)
	}()

	d, err := NewStdioDialer(c2, c2, c2)
	if err != nil {
		t.Fatalf("NewStdioDialer: %v", err)
	}
	defer d.Close()

	s, err := d.Dial(context.Background())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	want := make([]byte, 1<<20) // 1 MiB
	for i := range want {
		want[i] = byte(i * 31)
	}
	done := make(chan error, 1)
	go func() {
		if _, err := s.Write(want); err != nil {
			done <- fmt.Errorf("write: %w", err)
			return
		}
		if cw, ok := s.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
		done <- nil
	}()
	got, err := io.ReadAll(s)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if werr := <-done; werr != nil {
		t.Fatal(werr)
	}
	if len(got) != len(want) {
		t.Fatalf("length mismatch: got %d want %d", len(got), len(want))
	}
	if string(got) != string(want) {
		t.Fatal("payload corrupted in round-trip")
	}
}

// TestStdioDialerErrorsAfterPipeClosed proves a dead underlying pipe surfaces as
// a Dial error rather than hanging — the failure mode Fleet must see when its
// ssh child dies.
func TestStdioDialerErrorsAfterPipeClosed(t *testing.T) {
	c1, c2 := net.Pipe()
	ln, err := NewStdioListener(c1, c1, c1)
	if err != nil {
		t.Fatalf("NewStdioListener: %v", err)
	}
	d, err := NewStdioDialer(c2, c2, c2)
	if err != nil {
		t.Fatalf("NewStdioDialer: %v", err)
	}
	defer d.Close()
	// Tear down the server end (as if the ssh child exited).
	_ = ln.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// yamux observes EOF asynchronously; OpenStream may enqueue a SYN before
	// its receive loop sees the closed pipe. Require EOF detection to complete
	// within the deadline before checking subsequent Dial calls.
	select {
	case <-d.sess.CloseChan():
	case <-ctx.Done():
		t.Fatal("session did not detect the closed pipe")
	}
	if _, err := d.Dial(ctx); err == nil {
		t.Fatal("expected Dial to error after the pipe was closed, got nil")
	}
}
