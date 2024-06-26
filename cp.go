package clientproxy

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"sync/atomic"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"golang.org/x/net/http2"
)

var h2t = http2.Transport{}

const (
	shutdownTimeout = time.Minute
)

func init() {
	caddy.RegisterModule(&Middleware{})
	httpcaddyfile.RegisterHandlerDirective("client_proxy", parseCaddyfile)
}

type handler struct {
	proxy *httputil.ReverseProxy
	done  chan struct{}
}

// Middleware implements an HTTP handler that allows for a client to become the
// reverse proxy.
type Middleware struct {
	// The secret to allow for registering a client.
	Secret string `json:"secret,omitempty"`

	// stores a *handler, when available
	handler atomic.Value
}

// CaddyModule returns the Caddy module information.
func (*Middleware) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.handlers.client_proxy",
		New: func() caddy.Module { return new(Middleware) },
	}
}

// Provision implements caddy.Provisioner.
func (m *Middleware) Provision(ctx caddy.Context) error {
	return nil
}

// Validate implements caddy.Validator.
func (m *Middleware) Validate() error {
	if m.Secret == "" {
		return fmt.Errorf("no secret")
	}
	return nil
}

func (m *Middleware) acceptProxy(w http.ResponseWriter, r *http.Request) error {
	rc := http.NewResponseController(w)
	if err := rc.EnableFullDuplex(); err != nil {
		return fmt.Errorf("client_proxy: must connect using HTTP/1.1: %w", err)
	}
	conn, buf, err := rc.Hijack()
	if err != nil {
		return fmt.Errorf("client_proxy: must connect using HTTP/1.1: %w", err)
	}
	defer conn.Close() // backup close, normally h2conn.Shutdown will handle this
	if err := buf.Flush(); err != nil {
		return fmt.Errorf("client_proxy: unexpected flush error: %w", err)
	}
	if buf.Reader.Buffered() > 0 {
		conn = &bufConn{Conn: conn, Reader: buf.Reader}
	}
	h2conn, err := h2t.NewClientConn(conn)
	if err != nil {
		return fmt.Errorf("client_proxy: unable to create ClientConn: %w", err)
	}

	// close the old one, if one is there
	if handler, ok := m.handler.Load().(*handler); ok {
		close(handler.done)
	}

	done := make(chan struct{})
	m.handler.Store(&handler{
		done: done,
		proxy: &httputil.ReverseProxy{
			Transport: h2conn,
			Director: func(r *http.Request) {
				// TODO: what
				r.URL.Scheme = "https"
			},
		},
	})
	<-done // wait until we're being replaced
	ctx, cancel := context.WithTimeout(r.Context(), shutdownTimeout)
	defer cancel()
	if err := h2conn.Shutdown(ctx); err != nil {
		if errors.Is(err, net.ErrClosed) {
			return nil
		}
		return fmt.Errorf("client_proxy: error shutting down ClientConn: %w", err)
	}
	return nil
}

// ServeHTTP implements caddyhttp.MiddlewareHandler.
func (m *Middleware) ServeHTTP(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
	if r.Header.Get("X-Client-Proxy") == m.Secret {
		return m.acceptProxy(w, r)
	}
	if handler, ok := m.handler.Load().(*handler); ok {
		handler.proxy.ServeHTTP(w, r)
		return nil
	}
	return next.ServeHTTP(w, r)
}

// UnmarshalCaddyfile implements caddyfile.Unmarshaler.
func (m *Middleware) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	d.Next() // consume directive name

	// require an argument
	if !d.NextArg() {
		return d.ArgErr()
	}

	// store the argument
	m.Secret = d.Val()
	return nil
}

// parseCaddyfile unmarshals tokens from h into a new Middleware.
func parseCaddyfile(h httpcaddyfile.Helper) (caddyhttp.MiddlewareHandler, error) {
	var m Middleware
	err := m.UnmarshalCaddyfile(h.Dispenser)
	return &m, err
}

type bufConn struct {
	net.Conn
	*bufio.Reader
}

func (c *bufConn) Read(p []byte) (int, error) {
	if c.Reader == nil {
		return c.Conn.Read(p)
	}
	n := c.Buffered()
	if n == 0 {
		c.Reader = nil
		return c.Conn.Read(p)
	}
	if n < len(p) {
		p = p[:n]
	}
	return c.Reader.Read(p)
}

// Interface guards
var (
	_ caddy.Provisioner           = (*Middleware)(nil)
	_ caddy.Validator             = (*Middleware)(nil)
	_ caddyhttp.MiddlewareHandler = (*Middleware)(nil)
	_ caddyfile.Unmarshaler       = (*Middleware)(nil)
)
