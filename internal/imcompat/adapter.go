// Package imcompat adapts the private WuKongIM v3 service protocols to the
// contracts consumed by Octo Server. It does not expose user-facing routes.
package imcompat

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"
)

const (
	DefaultMaxBodyBytes = 2 << 20
	MaxEventItems       = 1000
)

type Options struct {
	CoreURL          string
	ServerWebhookURL string
	APIToken         string
	WebhookSecret    string
	ManagerURL       string
	ManagerUser      string
	ManagerPassword  string
	Timeout          time.Duration
	MaxBodyBytes     int64
}

type Adapter struct {
	core          *url.URL
	serverWebhook *url.URL
	manager       *url.URL
	opts          Options
	client        *http.Client
	proxy         *httputil.ReverseProxy
	apiSlots      chan struct{}
	webhookSlots  chan struct{}
}

func New(opts Options) (*Adapter, error) {
	if len(opts.APIToken) < 32 || len(opts.WebhookSecret) < 32 {
		return nil, errors.New("API token and webhook secret must each contain at least 32 bytes")
	}
	if opts.Timeout == 0 {
		opts.Timeout = 10 * time.Second
	}
	if opts.Timeout < time.Second || opts.Timeout > time.Minute {
		return nil, errors.New("request timeout must be between one second and one minute")
	}
	if opts.MaxBodyBytes == 0 {
		opts.MaxBodyBytes = DefaultMaxBodyBytes
	}
	if opts.MaxBodyBytes < 1024 || opts.MaxBodyBytes > 64<<20 {
		return nil, errors.New("body limit must be between 1 KiB and 64 MiB")
	}
	core, err := targetURL(opts.CoreURL, true)
	if err != nil {
		return nil, fmt.Errorf("core URL: %w", err)
	}
	webhook, err := targetURL(opts.ServerWebhookURL, false)
	if err != nil {
		return nil, fmt.Errorf("server webhook URL: %w", err)
	}
	var manager *url.URL
	if opts.ManagerURL != "" || opts.ManagerUser != "" || opts.ManagerPassword != "" {
		if opts.ManagerURL == "" || opts.ManagerUser == "" || opts.ManagerPassword == "" {
			return nil, errors.New("Manager URL, user and password must be configured together")
		}
		manager, err = targetURL(opts.ManagerURL, true)
		if err != nil {
			return nil, fmt.Errorf("manager URL: %w", err)
		}
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil // Fixed private destinations; do not forward service credentials to an environment proxy.
	transport.ResponseHeaderTimeout = opts.Timeout
	a := &Adapter{
		core: core, serverWebhook: webhook, manager: manager, opts: opts,
		apiSlots: make(chan struct{}, 64), webhookSlots: make(chan struct{}, 16),
		client: &http.Client{Transport: transport, Timeout: opts.Timeout,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
	}
	a.proxy = &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(a.core)
			r.Out.Header.Del("token")
			r.Out.Header.Del("Authorization")
		},
		Transport:      transport,
		ModifyResponse: a.modifySendResponse,
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
			protocolError(w, http.StatusBadGateway, "IM upstream unavailable or invalid response")
		},
	}
	return a, nil
}

func targetURL(raw string, rootOnly bool) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u == nil || (u.Scheme != "http" && u.Scheme != "https") ||
		u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return nil, errors.New("expected an HTTP(S) URL without credentials, query or fragment")
	}
	if rootOnly && u.Path != "" && u.Path != "/" {
		return nil, errors.New("expected a root URL")
	}
	return u, nil
}

// LoopbackAddress is also checked by the executable before opening its callback
// socket. RemoteAddr validation alone is insufficient if an ingress proxies to it.
func LoopbackAddress(address string) bool {
	host, _, err := net.SplitHostPort(address)
	ip := net.ParseIP(host)
	return err == nil && ip != nil && ip.IsLoopback()
}

// APIHandler uses the private IM service token, not end-user/Space middleware.
// Caller authorization and user-facing error localization remain in Octo Server.
func (a *Adapter) APIHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && (r.URL.Path == "/health" || r.URL.Path == "/healthz" || r.URL.Path == "/readyz") {
			if r.URL.Path == "/health" {
				r = r.Clone(r.Context())
				r.URL.Path = "/readyz"
			}
			a.proxy.ServeHTTP(w, r)
			return
		}
		provided, expected := sha256.Sum256([]byte(r.Header.Get("token"))), sha256.Sum256([]byte(a.opts.APIToken))
		if subtle.ConstantTimeCompare(provided[:], expected[:]) != 1 {
			protocolError(w, http.StatusUnauthorized, "IM service authentication required")
			return
		}
		if !acquire(a.apiSlots) {
			protocolError(w, http.StatusServiceUnavailable, "IM adapter is busy")
			return
		}
		defer func() { <-a.apiSlots }()
		ctx, cancel := context.WithTimeout(r.Context(), a.opts.Timeout*3)
		defer cancel()
		r = r.Clone(ctx)
		if r.Body != nil {
			body, err := readBounded(r.Body, a.opts.MaxBodyBytes)
			_ = r.Body.Close()
			if err != nil {
				protocolError(w, http.StatusRequestEntityTooLarge, "invalid or oversized IM request")
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
			r.ContentLength = int64(len(body))
		}
		switch {
		case r.URL.Path == "/message/sendbatch" && r.Method == http.MethodPost:
			a.sendBatch(w, r)
		case r.URL.Path == "/channel/max_message_seq" && r.Method == http.MethodGet:
			a.maxMessageSeq(w, r)
		default:
			a.proxy.ServeHTTP(w, r)
		}
	})
}

func acquire(slots chan struct{}) bool {
	select {
	case slots <- struct{}{}:
		return true
	default:
		return false
	}
}

func readBounded(r io.Reader, limit int64) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil || int64(len(body)) > limit {
		return nil, errors.New("invalid or oversized body")
	}
	return body, nil
}

func protocolError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{"status": status, "msg": message})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func (a *Adapter) request(ctx context.Context, method string, target *url.URL, body []byte, headers http.Header) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, target.String(), bytes.NewReader(body))
	if err != nil {
		return 0, nil, errors.New("could not construct upstream request")
	}
	req.Header = headers.Clone()
	if req.Header == nil {
		req.Header = make(http.Header)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.client.Do(req)
	if err != nil {
		// Do not include destination URLs, response bodies or credentials in errors.
		return 0, nil, errors.New("upstream request failed; delivery may be unknown")
	}
	defer resp.Body.Close()
	result, err := readBounded(resp.Body, a.opts.MaxBodyBytes)
	return resp.StatusCode, result, err
}

func endpoint(base *url.URL, path string) *url.URL {
	u := *base
	u.Path = strings.TrimSuffix(base.Path, "/") + path
	u.RawPath = ""
	return &u
}
