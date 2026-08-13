package agentmailgateway

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	spacepkg "github.com/Mininglamp-OSS/octo-server/pkg/space"
	appwkhttp "github.com/Mininglamp-OSS/octo-server/pkg/wkhttp"
	"go.uber.org/zap"
	"golang.org/x/sync/semaphore"
)

const (
	envGatewayURL     = "OCTO_MAIL_GATEWAY_URL"
	envGatewaySecret  = "OCTO_MAIL_GATEWAY_SECRET"
	envGatewayTimeout = "OCTO_MAIL_GATEWAY_TIMEOUT"

	gatewayRoutePrefix          = "/v1/mail-gateway"
	mailAPIPrefix               = "/webapi/v0"
	maxRequestBytes             = int64(64 << 20)
	maxResponseBytes            = int64(64 << 20)
	largeRequestThreshold       = int64(1 << 20)
	maxConcurrentLargeRequests  = 4
	maxInFlightRequestBodyBytes = maxConcurrentLargeRequests * maxRequestBytes
	defaultLargeRequestSlotWait = 5 * time.Second
	defaultTimeout              = 30 * time.Second
)

var allowedMethods = map[string]struct{}{
	http.MethodGet: {}, http.MethodHead: {}, http.MethodPost: {},
	http.MethodPut: {}, http.MethodPatch: {}, http.MethodDelete: {},
	http.MethodOptions: {},
}

var (
	errGatewayNotConfigured  = errors.New("Agent Mail gateway is not configured")
	errBodyTooLarge          = errors.New("body exceeds gateway limit")
	errLargeRequestSlotWait  = errors.New("timed out waiting for a large request slot")
	errRequestBodyBudgetWait = errors.New("timed out waiting for request body budget")
)

type gatewayConfig struct {
	upstream *url.URL
	secret   []byte
	timeout  time.Duration
}

// Gateway authenticates browser mail requests through OCTO and proxies only
// the octo-mail WebAPI surface using a short-lived request-bound assertion.
type Gateway struct {
	ctx *config.Context
	log.Log
	cfg         gatewayConfig
	client      *http.Client
	now         func() time.Time
	nonceSource io.Reader
	// largeRequestSlots bounds the number of request bodies large enough to
	// create material memory pressure while they are buffered for signing. The
	// weighted budget separately bounds bodies retained by upstream uploads.
	largeRequestSlots    chan struct{}
	largeRequestSlotWait time.Duration
	requestBodyBudget    *semaphore.Weighted
}

func New(ctx *config.Context) *Gateway {
	cfg, err := loadGatewayConfig()
	g := &Gateway{
		ctx:                  ctx,
		Log:                  log.NewTLog("AgentMailGateway"),
		cfg:                  cfg,
		now:                  time.Now,
		nonceSource:          randomNonceSource(),
		largeRequestSlots:    make(chan struct{}, maxConcurrentLargeRequests),
		largeRequestSlotWait: defaultLargeRequestSlotWait,
		requestBodyBudget:    semaphore.NewWeighted(maxInFlightRequestBodyBytes),
	}
	if err != nil {
		if errors.Is(err, errGatewayNotConfigured) {
			g.Warn("Agent Mail gateway is disabled because it is not configured")
		} else {
			g.Error("Agent Mail gateway is disabled because its configuration is invalid", zap.Error(err))
		}
	}
	g.client = newGatewayHTTPClient(cfg.timeout)
	return g
}

func loadGatewayConfig() (gatewayConfig, error) {
	cfg := gatewayConfig{timeout: defaultTimeout}
	rawURL := strings.TrimSpace(os.Getenv(envGatewayURL))
	secret := os.Getenv(envGatewaySecret)
	if rawURL == "" && secret == "" {
		return cfg, errGatewayNotConfigured
	}
	if rawURL == "" || secret == "" {
		return cfg, fmt.Errorf("%s and %s must be configured together", envGatewayURL, envGatewaySecret)
	}
	if len([]byte(secret)) < 32 {
		return cfg, fmt.Errorf("%s must contain at least 32 bytes", envGatewaySecret)
	}
	upstream, err := url.Parse(rawURL)
	if err != nil || (upstream.Scheme != "http" && upstream.Scheme != "https") || upstream.Host == "" ||
		upstream.User != nil || upstream.RawQuery != "" || upstream.Fragment != "" {
		return cfg, fmt.Errorf("%s must be an http(s) origin with an optional path", envGatewayURL)
	}
	upstream.Path = strings.TrimRight(upstream.Path, "/")
	upstream.RawPath = strings.TrimRight(upstream.RawPath, "/")

	if rawTimeout := strings.TrimSpace(os.Getenv(envGatewayTimeout)); rawTimeout != "" {
		parsed, err := time.ParseDuration(rawTimeout)
		if err != nil || parsed <= 0 || parsed > 2*time.Minute {
			return cfg, fmt.Errorf("%s must be between 1ns and 2m", envGatewayTimeout)
		}
		cfg.timeout = parsed
	}
	cfg.upstream = upstream
	cfg.secret = []byte(secret)
	return cfg, nil
}

func newGatewayHTTPClient(timeout time.Duration) *http.Client {
	timeout = normalizedGatewayTimeout(timeout)
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = (&net.Dialer{
		Timeout:   timeout,
		KeepAlive: 30 * time.Second,
	}).DialContext
	transport.TLSHandshakeTimeout = timeout
	transport.ResponseHeaderTimeout = timeout
	return &http.Client{
		Transport: transport,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func normalizedGatewayTimeout(timeout time.Duration) time.Duration {
	if timeout <= 0 {
		return defaultTimeout
	}
	return timeout
}

// Route requires the standard authenticated UID and validated Space before the
// request can reach the proxy trust boundary.
func (g *Gateway) Route(r *wkhttp.WKHttp) {
	group := r.Group(
		gatewayRoutePrefix,
		g.ctx.AuthMiddleware(r),
		appwkhttp.SharedUIDRateLimiter(r, g.ctx),
		spacepkg.SpaceMiddleware(g.ctx),
	)
	group.RouterGroup.Any("/*path", r.WKHttpHandler(g.proxy))
}

func (g *Gateway) proxy(c *wkhttp.Context) {
	uid := strings.TrimSpace(c.GetLoginUID())
	spaceID := strings.TrimSpace(spacepkg.GetSpaceID(c))
	if uid == "" {
		respondGatewayError(c, errcode.ErrAgentMailGatewayUnauthorized)
		return
	}
	if spaceID == "" {
		respondGatewayError(c, errcode.ErrAgentMailGatewaySpaceRequired)
		return
	}
	if g.cfg.upstream == nil || len(g.cfg.secret) < 32 || g.client == nil {
		respondGatewayError(c, errcode.ErrAgentMailGatewayUnavailable)
		return
	}
	if _, ok := allowedMethods[c.Request.Method]; !ok {
		respondGatewayError(c, errcode.ErrAgentMailGatewayRequestInvalid)
		return
	}

	target, err := g.upstreamURL(c.Request.URL)
	if err != nil {
		respondGatewayError(c, errcode.ErrAgentMailGatewayRequestInvalid)
		return
	}
	if c.Request.ContentLength > maxRequestBytes {
		respondGatewayError(c, errcode.ErrAgentMailGatewayPayloadTooLarge)
		return
	}
	releaseLargeRequest, err := g.acquireLargeRequestSlot(c.Request)
	if err != nil {
		respondGatewayError(c, errcode.ErrAgentMailGatewayUnavailable)
		return
	}
	defer releaseLargeRequest()
	releaseRequestBodyBudget, err := g.acquireRequestBodyBudget(c.Request)
	if err != nil {
		respondGatewayError(c, errcode.ErrAgentMailGatewayUnavailable)
		return
	}
	releaseBudgetOnReturn := true
	defer func() {
		if releaseBudgetOnReturn {
			releaseRequestBodyBudget()
		}
	}()
	timeout := normalizedGatewayTimeout(g.cfg.timeout)
	responseController := http.NewResponseController(c.Writer)
	requestBody := newIdleTimeoutReadCloser(c.Request.Body, timeout, func() {
		// A real net/http request body holds its own mutex while blocked in Read,
		// so Close alone cannot reliably interrupt a slow client. Expiring the
		// connection read deadline first makes that Read return; Close remains a
		// fallback for test/custom bodies and non-standard servers.
		if err := responseController.SetReadDeadline(time.Now()); err != nil {
			g.Warn("Failed to expire stalled Agent Mail request body", zap.Error(err))
		}
	})
	requestBodyClosed := false
	defer func() {
		if requestBodyClosed {
			return
		}
		// A panic must not leave the watchdog holding a writer that Gin may
		// reuse. Expire the connection first so closing an unread chunked body
		// cannot block in net/http's connection-reuse drain.
		_ = responseController.SetReadDeadline(time.Now())
		c.Writer.Header().Set("Connection", "close")
		_ = requestBody.Close()
	}()
	body, err := readBoundedBody(requestBody, maxRequestBytes)
	if err != nil {
		// An unknown-length body may still have bytes pending after LimitReader
		// detects overflow. net/http otherwise tries to drain up to 256 KiB in
		// Body.Close, which can block forever once the watchdog is stopped.
		// Expire the read side before Close and make the connection non-reusable;
		// the response side remains available for the explicit 4xx envelope.
		if deadlineErr := responseController.SetReadDeadline(time.Now()); deadlineErr != nil {
			g.Warn("Failed to expire rejected Agent Mail request body", zap.Error(deadlineErr))
		}
		c.Writer.Header().Set("Connection", "close")
		_ = requestBody.Close()
		requestBodyClosed = true
		releaseLargeRequest()
		if errors.Is(err, errBodyTooLarge) {
			respondGatewayError(c, errcode.ErrAgentMailGatewayPayloadTooLarge)
		} else {
			respondGatewayError(c, errcode.ErrAgentMailGatewayRequestInvalid)
		}
		return
	}
	_ = requestBody.Close()
	requestBodyClosed = true
	if deadlineErr := responseController.SetReadDeadline(time.Time{}); deadlineErr != nil {
		g.Warn("Failed to clear Agent Mail request body deadline", zap.Error(deadlineErr))
	}
	releaseLargeRequest()
	mailboxID := strings.TrimSpace(c.GetHeader("X-Octo-Mailbox-ID"))
	token, err := signAssertionForMailbox(g.cfg.secret, uid, spaceID, mailboxID, c.Request.Method, target.RequestURI(), body, g.now(), g.nonceSource)
	if err != nil {
		g.Error("Failed to sign Agent Mail gateway assertion", zap.Error(err))
		respondGatewayError(c, errcode.ErrAgentMailGatewayUnavailable)
		return
	}

	upstreamContext, cancelUpstream := context.WithCancel(c.Request.Context())
	defer cancelUpstream()
	uploadWatchdog := newProgressWatchdog(timeout, cancelUpstream)
	defer uploadWatchdog.Stop()
	var uploadBody *releasingRequestBody
	var upstreamRequestBody io.Reader
	if len(body) > 0 {
		uploadBody = newReleasingRequestBody(body, uploadWatchdog, releaseRequestBodyBudget)
		upstreamRequestBody = uploadBody
	}
	request, err := http.NewRequestWithContext(upstreamContext, c.Request.Method, target.String(), upstreamRequestBody)
	if err != nil {
		g.Error("Failed to construct Agent Mail upstream request", zap.Error(err))
		respondGatewayError(c, errcode.ErrAgentMailGatewayUnavailable)
		return
	}
	if len(body) > 0 {
		request.ContentLength = int64(len(body))
		// The RoundTripper owns request.Body after Do starts. Its Close method
		// drops the final request-held reference to body before releasing the
		// corresponding byte reservation. Do not mutate request.Body here: a
		// RoundTripper may still access it asynchronously after Do returns.
		releaseBudgetOnReturn = false
		body = nil
	} else {
		releaseRequestBodyBudget()
		releaseBudgetOnReturn = false
	}
	copyRequestHeaders(request.Header, c.Request.Header)
	// Browser compression preferences are intentionally not forwarded. Asking
	// for the identity representation also prevents net/http from transparently
	// decompressing gzip into an unknown-length response that HTTP/2 cannot
	// safely truncate on a later upstream failure.
	request.Header.Set("Accept-Encoding", "identity")
	request.Header.Set("Authorization", "Bearer "+token)
	// Reconstruct the mailbox selector from the exact value covered by the
	// assertion. A caller-controlled Connection header must not be able to make
	// the signed claim and the downstream request disagree.
	if mailboxID != "" {
		request.Header.Set("X-Octo-Mailbox-ID", mailboxID)
	}

	response, err := g.client.Do(request)
	uploadWatchdog.Stop()
	if err != nil {
		g.Warn("Agent Mail upstream request failed", zap.Error(upstreamErrorCause(err)))
		respondGatewayError(c, errcode.ErrAgentMailGatewayUnavailable)
		return
	}
	response.Body = newIdleTimeoutReadCloser(response.Body, timeout, nil)
	defer response.Body.Close()
	hasResponseBody := responseHasBody(c.Request.Method, response.StatusCode)
	if hasResponseBody && response.ContentLength > maxResponseBytes {
		g.Error("Agent Mail upstream response exceeded the gateway limit",
			zap.Int64("contentLength", response.ContentLength), zap.Int64("limit", maxResponseBytes))
		respondGatewayError(c, errcode.ErrAgentMailGatewayBadResponse)
		return
	}

	unknownLength := hasResponseBody && response.ContentLength < 0
	if hasResponseBody && isCloseDelimitedResponse(response) {
		g.Error("Agent Mail upstream returned an unsafe close-delimited response")
		respondGatewayError(c, errcode.ErrAgentMailGatewayBadResponse)
		return
	}
	if unknownLength && !(c.Request.ProtoMajor == 1 && c.Request.ProtoMinor >= 1) {
		g.Error("Agent Mail response stream cannot expose truncation to a non-HTTP/1.1 client")
		respondGatewayError(c, errcode.ErrAgentMailGatewayBadResponse)
		return
	}

	responseWriter, err := newProgressDeadlineWriter(c.Writer, responseController, timeout)
	if err != nil {
		g.Error("Failed to bound Agent Mail downstream response write", zap.Error(err))
		respondGatewayError(c, errcode.ErrAgentMailGatewayBadResponse)
		return
	}
	defer func() {
		if err := responseWriter.Close(); err != nil {
			g.Warn("Failed to clear Agent Mail downstream response deadline", zap.Error(err))
		}
	}()
	copyResponseHeaders(c.Writer.Header(), response.Header)
	// Every response varies by authenticated user, validated Space, and selected
	// mailbox even though those identities are absent from the URL. Never allow
	// an upstream cache directive to make private mail reusable across callers.
	c.Writer.Header().Set("Cache-Control", "private, no-store")
	c.Writer.Header().Set("Vary", "token, X-Space-ID, X-Octo-Mailbox-ID")
	if response.ContentLength > 0 {
		c.Writer.Header().Set("Content-Length", strconv.FormatInt(response.ContentLength, 10))
	}
	c.Status(response.StatusCode)
	if hasResponseBody {
		var downstreamWriter io.Writer = responseWriter
		if unknownLength {
			// Commit headers immediately and flush each copied chunk. These responses
			// must remain streaming rather than becoming a temp-file dependency.
			c.Writer.Flush()
			downstreamWriter = flushingResponseWriter{ResponseWriter: responseWriter, Flusher: c.Writer}
		}
		if err := copyBoundedBody(downstreamWriter, response.Body, maxResponseBytes); err != nil {
			g.Error("Agent Mail upstream response stream failed", zap.Error(err))
			if unknownLength {
				// gin.Recovery intercepts http.ErrAbortHandler, so explicitly close
				// the current HTTP/1.1 connection. Without a terminating chunk the
				// client observes a transport error instead of a clean truncated 2xx.
				if abortErr := abortResponseStream(c.Writer); abortErr != nil {
					g.Error("Failed to abort Agent Mail response stream", zap.Error(abortErr))
				}
			}
		}
	}
}

func (g *Gateway) upstreamURL(incoming *url.URL) (*url.URL, error) {
	if incoming == nil || g.cfg.upstream == nil {
		return nil, fmt.Errorf("missing URL")
	}
	escapedPath := incoming.EscapedPath()
	if !strings.HasPrefix(escapedPath, gatewayRoutePrefix+"/") {
		return nil, fmt.Errorf("path is outside gateway route")
	}
	relativeEscaped := strings.TrimPrefix(escapedPath, gatewayRoutePrefix)
	relative, err := url.ParseRequestURI(relativeEscaped)
	if err != nil || (relative.Path != mailAPIPrefix && !strings.HasPrefix(relative.Path, mailAPIPrefix+"/")) || hasUnsafePathSegment(relative.Path) {
		return nil, fmt.Errorf("path is outside mail WebAPI")
	}

	target := *g.cfg.upstream
	target.Path = strings.TrimRight(g.cfg.upstream.Path, "/") + relative.Path
	baseEscaped := strings.TrimRight(g.cfg.upstream.EscapedPath(), "/")
	target.RawPath = baseEscaped + relative.EscapedPath()
	if target.RawPath == target.Path {
		target.RawPath = ""
	}
	target.RawQuery = incoming.RawQuery
	return &target, nil
}

func hasUnsafePathSegment(value string) bool {
	if strings.Contains(value, `\`) || path.Clean(value) != value {
		return true
	}
	for _, segment := range strings.Split(value, "/") {
		if segment == "." || segment == ".." {
			return true
		}
	}
	return false
}

func readBoundedBody(reader io.Reader, limit int64) ([]byte, error) {
	if reader == nil {
		return nil, nil
	}
	body, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, errBodyTooLarge
	}
	return body, nil
}

func copyBoundedBody(dst io.Writer, src io.Reader, limit int64) error {
	if src == nil {
		return nil
	}
	written, err := io.Copy(dst, io.LimitReader(src, limit))
	if err != nil {
		return err
	}
	if written < limit {
		return nil
	}
	var probe [1]byte
	_, err = io.ReadFull(src, probe[:])
	switch {
	case err == nil:
		return errBodyTooLarge
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return nil
	default:
		return err
	}
}

func responseHasBody(method string, status int) bool {
	return method != http.MethodHead && status >= http.StatusOK &&
		status != http.StatusNoContent && status != http.StatusNotModified
}

func isCloseDelimitedResponse(response *http.Response) bool {
	return response != nil && response.ContentLength < 0 && response.ProtoMajor == 1 &&
		len(response.TransferEncoding) == 0 && !response.Uncompressed
}

type flushingResponseWriter struct {
	ResponseWriter io.Writer
	Flusher        http.Flusher
}

// progressWatchdog invokes cancel after one timeout interval without a Touch.
// It is used around client.Do so request upload and response-header waits both
// have a bound even when a custom or stalled transport never returns.
type progressWatchdog struct {
	mu       sync.Mutex
	timer    *time.Timer
	timeout  time.Duration
	deadline time.Time
	stopped  bool
	cancel   func()
	done     chan struct{}
	doneOnce sync.Once
}

func newProgressWatchdog(timeout time.Duration, cancel func()) *progressWatchdog {
	w := &progressWatchdog{
		timeout: normalizedGatewayTimeout(timeout),
		cancel:  cancel,
		done:    make(chan struct{}),
	}
	w.deadline = time.Now().Add(w.timeout)
	w.timer = time.AfterFunc(w.timeout, w.expire)
	return w
}

func (w *progressWatchdog) expire() {
	w.mu.Lock()
	if w.stopped {
		w.mu.Unlock()
		w.finish()
		return
	}
	if remaining := time.Until(w.deadline); remaining > 0 {
		w.timer.Reset(remaining)
		w.mu.Unlock()
		return
	}
	w.stopped = true
	cancel := w.cancel
	w.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	w.finish()
}

func (w *progressWatchdog) Touch() {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.stopped {
		w.deadline = time.Now().Add(w.timeout)
		w.timer.Reset(w.timeout)
	}
}

func (w *progressWatchdog) Stop() {
	if w == nil {
		return
	}
	w.mu.Lock()
	stoppedTimer := false
	if !w.stopped {
		w.stopped = true
		stoppedTimer = w.timer.Stop()
	}
	done := w.done
	w.mu.Unlock()
	if stoppedTimer {
		w.finish()
	}
	<-done
}

func (w *progressWatchdog) finish() {
	w.doneOnce.Do(func() { close(w.done) })
}

type progressReader struct {
	io.Reader
	watchdog *progressWatchdog
}

func (r *progressReader) Read(body []byte) (int, error) {
	n, err := r.Reader.Read(body)
	if n > 0 {
		r.watchdog.Touch()
	}
	return n, err
}

// releasingRequestBody retains the buffered request only until the upstream
// RoundTripper closes the request body. RoundTrip is allowed to close Body
// asynchronously after returning a response, so the handler must not clear the
// request fields or release the byte reservation based only on Do returning.
type releasingRequestBody struct {
	mu        sync.Mutex
	reader    io.Reader
	release   func()
	closeOnce sync.Once
}

func newReleasingRequestBody(body []byte, watchdog *progressWatchdog, release func()) *releasingRequestBody {
	return &releasingRequestBody{
		reader:  &progressReader{Reader: bytes.NewReader(body), watchdog: watchdog},
		release: release,
	}
}

func (r *releasingRequestBody) Read(body []byte) (int, error) {
	if r == nil {
		return 0, io.EOF
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.reader == nil {
		return 0, io.EOF
	}
	return r.reader.Read(body)
}

func (r *releasingRequestBody) Close() error {
	if r == nil {
		return nil
	}
	r.closeOnce.Do(func() {
		r.mu.Lock()
		r.reader = nil
		release := r.release
		r.release = nil
		r.mu.Unlock()
		if release != nil {
			release()
		}
	})
	return nil
}

// idleTimeoutReadCloser closes a body after one timeout interval without a
// successful read. Closing the upstream response body interrupts the transport;
// the optional callback lets request-body reads first expire the server-side
// connection deadline.
type idleTimeoutReadCloser struct {
	body      io.ReadCloser
	watchdog  *progressWatchdog
	closeOnce sync.Once
}

func newIdleTimeoutReadCloser(body io.ReadCloser, timeout time.Duration, beforeClose func()) *idleTimeoutReadCloser {
	r := &idleTimeoutReadCloser{body: body}
	r.watchdog = newProgressWatchdog(timeout, func() {
		if beforeClose != nil {
			beforeClose()
		}
		r.closeBody()
	})
	return r
}

func (r *idleTimeoutReadCloser) Read(body []byte) (int, error) {
	if r == nil || r.body == nil {
		return 0, io.EOF
	}
	n, err := r.body.Read(body)
	if n > 0 {
		r.watchdog.Touch()
	}
	if err != nil {
		r.watchdog.Stop()
	}
	return n, err
}

func (r *idleTimeoutReadCloser) Close() error {
	if r == nil {
		return nil
	}
	r.watchdog.Stop()
	return r.closeBody()
}

func (r *idleTimeoutReadCloser) closeBody() error {
	var closeErr error
	r.closeOnce.Do(func() {
		if r.body != nil {
			closeErr = r.body.Close()
		}
	})
	return closeErr
}

func (w flushingResponseWriter) Write(body []byte) (int, error) {
	written, err := w.ResponseWriter.Write(body)
	if written > 0 {
		w.Flusher.Flush()
	}
	return written, err
}

// progressDeadlineWriter bounds each downstream response write independently.
// A successful write refreshes the deadline for the next chunk. Test/custom
// writers may not expose connection deadlines; production net/http writers do
// through the response-writer Unwrap chain.
type progressDeadlineWriter struct {
	writer     io.Writer
	controller *http.ResponseController
	timeout    time.Duration
	enabled    bool
}

func newProgressDeadlineWriter(
	writer io.Writer,
	controller *http.ResponseController,
	timeout time.Duration,
) (*progressDeadlineWriter, error) {
	w := &progressDeadlineWriter{
		writer:     writer,
		controller: controller,
		timeout:    normalizedGatewayTimeout(timeout),
		enabled:    true,
	}
	if err := w.refresh(); err != nil {
		if errors.Is(err, http.ErrNotSupported) {
			w.enabled = false
			return w, nil
		}
		return nil, err
	}
	return w, nil
}

func (w *progressDeadlineWriter) Write(body []byte) (int, error) {
	if err := w.refresh(); err != nil {
		return 0, err
	}
	written, err := w.writer.Write(body)
	if written > 0 {
		if refreshErr := w.refresh(); err == nil && refreshErr != nil {
			err = refreshErr
		}
	}
	return written, err
}

func (w *progressDeadlineWriter) Close() error {
	if w == nil || !w.enabled {
		return nil
	}
	return w.controller.SetWriteDeadline(time.Time{})
}

func (w *progressDeadlineWriter) refresh() error {
	if w == nil || !w.enabled {
		return nil
	}
	return w.controller.SetWriteDeadline(time.Now().Add(w.timeout))
}

func abortResponseStream(writer http.ResponseWriter) error {
	hijacker, ok := writer.(http.Hijacker)
	if !ok {
		return errors.New("response writer does not support connection hijacking")
	}
	connection, _, err := hijacker.Hijack()
	if err != nil {
		return err
	}
	return connection.Close()
}

func (g *Gateway) acquireLargeRequestSlot(request *http.Request) (func(), error) {
	if request == nil || request.Body == nil || request.ContentLength == 0 ||
		(request.ContentLength > 0 && request.ContentLength <= largeRequestThreshold) || g.largeRequestSlots == nil {
		return func() {}, nil
	}
	wait := g.largeRequestSlotWait
	if wait <= 0 {
		wait = defaultLargeRequestSlotWait
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case g.largeRequestSlots <- struct{}{}:
		var once sync.Once
		return func() { once.Do(func() { <-g.largeRequestSlots }) }, nil
	case <-request.Context().Done():
		return func() {}, request.Context().Err()
	case <-timer.C:
		return func() {}, errLargeRequestSlotWait
	}
}

func (g *Gateway) acquireRequestBodyBudget(request *http.Request) (func(), error) {
	if request == nil || request.Body == nil || request.ContentLength == 0 ||
		(request.ContentLength > 0 && request.ContentLength <= largeRequestThreshold) || g.requestBodyBudget == nil {
		return func() {}, nil
	}
	reservation := request.ContentLength
	if reservation < 0 {
		reservation = maxRequestBytes
	}
	wait := g.largeRequestSlotWait
	if wait <= 0 {
		wait = defaultLargeRequestSlotWait
	}
	waitCtx, cancel := context.WithTimeout(request.Context(), wait)
	defer cancel()
	if err := g.requestBodyBudget.Acquire(waitCtx, reservation); err != nil {
		if request.Context().Err() != nil {
			return func() {}, request.Context().Err()
		}
		return func() {}, errRequestBodyBudgetWait
	}
	var once sync.Once
	return func() {
		once.Do(func() { g.requestBodyBudget.Release(reservation) })
	}, nil
}

func copyRequestHeaders(dst, src http.Header) {
	connectionHeaders := connectionHeaderNames(src)
	for _, name := range []string{
		"Accept", "Accept-Language", "Cache-Control", "Content-Type", "If-Match",
		"If-Modified-Since", "If-None-Match", "If-Range", "If-Unmodified-Since", "Range", "User-Agent",
	} {
		if _, nominated := connectionHeaders[http.CanonicalHeaderKey(name)]; nominated {
			continue
		}
		for _, value := range src.Values(name) {
			dst.Add(name, value)
		}
	}
}

func copyResponseHeaders(dst, src http.Header) {
	connectionHeaders := connectionHeaderNames(src)
	for name, values := range src {
		canonical := http.CanonicalHeaderKey(name)
		_, nominated := connectionHeaders[canonical]
		if nominated || isHopByHopHeader(canonical) || canonical == "Content-Length" || canonical == "Set-Cookie" {
			continue
		}
		for _, value := range values {
			dst.Add(canonical, value)
		}
	}
}

func connectionHeaderNames(headers http.Header) map[string]struct{} {
	names := make(map[string]struct{})
	for _, value := range headers.Values("Connection") {
		for _, token := range strings.Split(value, ",") {
			if name := http.CanonicalHeaderKey(strings.TrimSpace(token)); name != "" {
				names[name] = struct{}{}
			}
		}
	}
	return names
}

func isHopByHopHeader(name string) bool {
	switch http.CanonicalHeaderKey(name) {
	case "Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
		"Proxy-Connection", "Te", "Trailer", "Transfer-Encoding", "Upgrade":
		return true
	default:
		return false
	}
}

func upstreamErrorCause(err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) && urlErr.Err != nil {
		return urlErr.Err
	}
	return err
}
