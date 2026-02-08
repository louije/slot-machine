package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"strings"
	"sync"
)

type dynamicProxy struct {
	mu        sync.RWMutex
	port      int
	addr      string
	srv       *http.Server
	intercept http.Handler // handles /agent/* and /chat before forwarding
}

func newDynamicProxy(addr string, intercept http.Handler) *dynamicProxy {
	return &dynamicProxy{addr: addr, intercept: intercept}
}

func (p *dynamicProxy) setTarget(port int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.port = port
	if port > 0 && p.srv == nil && p.addr != "" {
		ln, err := net.Listen("tcp", p.addr)
		if err != nil {
			return
		}
		p.srv = &http.Server{Handler: http.HandlerFunc(p.serveHTTP)}
		go p.srv.Serve(ln)
	}
}

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

func (p *dynamicProxy) serveHTTP(w http.ResponseWriter, r *http.Request) {
	// Intercept /agent/* and /chat — handled by slot-machine, not forwarded.
	if p.intercept != nil && (strings.HasPrefix(r.URL.Path, "/agent/") || r.URL.Path == "/chat") {
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

	proxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = "http"
			req.URL.Host = fmt.Sprintf("127.0.0.1:%d", port)
		},
	}
	proxy.ServeHTTP(w, r)
}
