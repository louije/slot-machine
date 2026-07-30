package proxy

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestDynamicProxyNoTarget(t *testing.T) {
	t.Parallel()
	p := New("", nil)
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/", nil)
	p.ServeHTTP(w, r)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", w.Code)
	}
}

func TestDynamicProxyWithTarget(t *testing.T) {
	t.Parallel()

	// Start a test backend.
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	}))
	defer backend.Close()

	// Extract port from backend URL.
	_, portStr, _ := net.SplitHostPort(backend.Listener.Addr().String())
	var port int
	fmt.Sscanf(portStr, "%d", &port)

	p := New("", nil)
	p.port = port // set directly since addr="" means no listener management

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/", nil)
	p.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if w.Body.String() != "ok" {
		t.Fatalf("body = %q", w.Body.String())
	}
}

func TestDynamicProxyLifecycle(t *testing.T) {
	t.Parallel()

	addr := freeAddr(t)
	p := New(addr, nil)

	// No target — no listener.
	conn, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
	if err == nil {
		conn.Close()
		t.Fatal("expected connection refused with no target")
	}

	// Start a backend.
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("backend"))
	}))
	defer backend.Close()
	_, bPortStr, _ := net.SplitHostPort(backend.Listener.Addr().String())
	var bPort int
	fmt.Sscanf(bPortStr, "%d", &bPort)

	// Set target — listener should start.
	p.SetTarget(bPort)
	time.Sleep(50 * time.Millisecond) // let goroutine start

	resp, err := http.Get(fmt.Sprintf("http://%s/", addr))
	if err != nil {
		t.Fatalf("GET after setTarget: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	// Clear target — the port stays bound and answers 503, so a crashed app is
	// distinguishable from a machine that is gone.
	p.ClearTarget()
	time.Sleep(50 * time.Millisecond)

	resp, err = http.Get(fmt.Sprintf("http://%s/", addr))
	if err != nil {
		t.Fatalf("the proxy must keep listening after clearTarget: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 with no live slot, got %d", resp.StatusCode)
	}
	if !p.Listening() {
		t.Fatal("expected listening() to report true after clearTarget")
	}

	// Re-targeting serves again without re-binding.
	p.SetTarget(bPort)
	resp, err = http.Get(fmt.Sprintf("http://%s/", addr))
	if err != nil {
		t.Fatalf("GET after re-target: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200 after re-target, got %d", resp.StatusCode)
	}

	// shutdown does release the port.
	p.Shutdown()
	time.Sleep(50 * time.Millisecond)

	conn, err = net.DialTimeout("tcp", addr, 100*time.Millisecond)
	if err == nil {
		conn.Close()
		t.Fatal("expected connection refused after shutdown")
	}
}

// freeAddr reserves an address by binding and releasing it. Inherently
// racy — see the note on findFreePort in the orchestrator — but adequate here:
// the proxy under test binds it again immediately.
func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserving a port: %v", err)
	}
	addr := l.Addr().String()
	l.Close()
	return addr
}
