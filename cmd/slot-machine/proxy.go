package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"strings"
	"sync"
	"time"
)

type dynamicProxy struct {
	mu        sync.RWMutex
	port      int
	addr      string
	srv       *http.Server
	listenErr error
	proxy     *httputil.ReverseProxy
	intercept http.Handler // handles /agent/* and /chat before forwarding
}

// listenRetryWindow bounds how long we keep trying to bind the public port.
// A daemon restarted while its predecessor is still draining will briefly find
// the port taken; failing instantly there turns an ordinary restart into an
// outage that needs a human.
const listenRetryWindow = 3 * time.Second

func newDynamicProxy(addr string, intercept http.Handler) *dynamicProxy {
	p := &dynamicProxy{addr: addr, intercept: intercept}

	// One ReverseProxy for the lifetime of the daemon rather than one per
	// request. A per-request proxy gets a fresh Transport, so nothing is ever
	// reused: every proxied request pays a new TCP handshake, and connection
	// state that should be pooled is thrown away.
	p.proxy = &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			p.mu.RLock()
			port := p.port
			p.mu.RUnlock()
			req.URL.Scheme = "http"
			req.URL.Host = fmt.Sprintf("127.0.0.1:%d", port)
		},
		// Streaming responses (SSE, long polls) must not sit in a buffer.
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			logf("proxy: %s %s: %v", r.Method, r.URL.Path, err)
			http.Error(w, "bad gateway", http.StatusBadGateway)
		},
	}
	return p
}

func (p *dynamicProxy) setTarget(port int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.port = port
	if port <= 0 || p.srv != nil || p.addr == "" {
		return
	}

	ln, err := p.listen()
	if err != nil {
		// Silently returning here left the daemon reporting itself healthy with
		// nothing listening on the public port, and no way to find out why.
		p.listenErr = err
		logf("proxy: cannot listen on %s: %v", p.addr, err)
		return
	}
	p.listenErr = nil

	p.srv = &http.Server{Handler: http.HandlerFunc(p.serveHTTP)}
	go p.srv.Serve(ln)
}

func (p *dynamicProxy) listen() (net.Listener, error) {
	deadline := time.Now().Add(listenRetryWindow)
	for {
		ln, err := net.Listen("tcp", p.addr)
		if err == nil {
			return ln, nil
		}
		if time.Now().After(deadline) {
			return nil, err
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// clearTarget drops the backend and stops listening on the public port.
//
// Whether a crashed app should present as connection-refused or as a 503 is a
// real design question — a 503 is more actionable, and holding the port stops
// anything else claiming it — but it is a change to the contract in
// docs/orchestrator-spec.md (scenario 7), not a bug. Left as it was; the retry
// in listen() covers the re-bind race that closing creates.
func (p *dynamicProxy) clearTarget() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.port = 0
	if p.srv != nil {
		p.srv.Close()
		p.srv = nil
	}
}

func (p *dynamicProxy) shutdown() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.port = 0
	if p.srv != nil {
		p.srv.Shutdown(context.Background())
		p.srv = nil
	}
}

// listening reports whether the public port is actually bound, so /status can
// tell the truth about it.
func (p *dynamicProxy) listening() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.addr == "" || p.srv != nil
}

func (p *dynamicProxy) serveHTTP(w http.ResponseWriter, r *http.Request) {
	// Intercept /agent/* and /chat — handled by slot-machine, not forwarded.
	if p.intercept != nil && (strings.HasPrefix(r.URL.Path, "/agent/") || r.URL.Path == "/chat" || strings.HasPrefix(r.URL.Path, "/chat/") || r.URL.Path == "/chat.css") {
		p.intercept.ServeHTTP(w, r)
		return
	}

	p.mu.RLock()
	port := p.port
	p.mu.RUnlock()

	if port == 0 {
		http.Error(w, "no live slot", http.StatusServiceUnavailable)
		return
	}

	p.proxy.ServeHTTP(w, r)
}
