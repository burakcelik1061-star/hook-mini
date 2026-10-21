package lifecycle

import (
	"context"
	"net"
	"net/http"
	"testing"
	"time"
)

func TestServeShutsDownOnCancel(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, srv, ln, 2*time.Second) }()

	resp, err := http.Get("http://" + ln.Addr().String())
	if err != nil {
		t.Fatalf("request while running: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve returned %v, want nil on clean shutdown", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Serve did not return after context cancellation")
	}

	if _, err := http.Get("http://" + ln.Addr().String()); err == nil {
		t.Error("server still accepts connections after shutdown")
	}
}

func TestServeWaitsForInFlightRequest(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
		w.WriteHeader(http.StatusOK)
	})}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, srv, ln, 5*time.Second) }()

	result := make(chan int, 1)
	go func() {
		resp, err := http.Get("http://" + ln.Addr().String())
		if err != nil {
			result <- -1
			return
		}
		resp.Body.Close()
		result <- resp.StatusCode
	}()

	<-started
	cancel() // shutdown begins while the request is still being handled
	select {
	case <-done:
		t.Fatal("Serve returned before the in-flight request finished")
	case <-time.After(200 * time.Millisecond):
	}

	close(release)
	if got := <-result; got != http.StatusOK {
		t.Fatalf("in-flight request got %d, want 200", got)
	}
	if err := <-done; err != nil {
		t.Fatalf("Serve: %v", err)
	}
}
