// Package proxy is the reverse proxy in front of the live slot.
//
// Its whole job is that the app's public port is owned by the daemon rather than
// by any particular app process, so a deploy can swap what is behind it without
// the port ever going away.
package proxy

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"strings"
	"sync"
	"time"
)

// Dynamic routes a fixed public address to whichever port is currently live.
type Dynamic struct {
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

// New returns a proxy bound to addr once a target is set. Requests for the
// agent's own paths go to intercept instead of being forwarded.
func New(addr string, intercept http.Handler) *Dynamic {
	p := &Dynamic{addr: addr, intercept: intercept}

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
			log.Printf("proxy: %s %s: %v", r.Method, r.URL.Path, err)
			http.Error(w, "bad gateway", http.StatusBadGateway)
		},
	}
	return p
}

func (p *Dynamic) SetTarget(port int) {
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
		log.Printf("proxy: cannot listen on %s: %v", p.addr, err)
		return
	}
	p.listenErr = nil

	p.srv = &http.Server{Handler: http.HandlerFunc(p.ServeHTTP)}
	go p.srv.Serve(ln)
}

func (p *Dynamic) listen() (net.Listener, error) {
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

// clearTarget drops the backend but keeps the public port bound, so a crashed
// app presents as 503 rather than as a refused connection.
//
// Closing the listener was strictly worse in three ways. Callers could not tell
// "the app is down" from "the whole machine is gone", which is the difference
// between retrying and paging someone. The port went back to the OS, so
// anything else on the box could take it while the app was down. And the next
// deploy then had to win a race to get its own port back.
//
// serveHTTP already answers 503 whenever there is no target, so this is also
// less code doing more.
func (p *Dynamic) ClearTarget() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.port = 0
}

func (p *Dynamic) Shutdown() {
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
func (p *Dynamic) Listening() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.addr == "" || p.srv != nil
}

func (p *Dynamic) ServeHTTP(w http.ResponseWriter, r *http.Request) {
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
