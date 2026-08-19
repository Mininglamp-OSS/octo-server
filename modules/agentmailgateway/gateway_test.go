package agentmailgateway

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
	appmetrics "github.com/Mininglamp-OSS/octo-server/pkg/metrics"
	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/sync/semaphore"
)

func TestProxyBindsAuthenticatedIdentityAndExactRequest(t *testing.T) {
	secret := []byte(strings.Repeat("g", 32))
	fixedNow := time.Date(2026, 7, 29, 11, 0, 0, 0, time.UTC)
	requestBody := []byte(`{"to":["b@demo.octo.test"],"text":"hello"}`)

	gateway := newTestGateway(t, "http://octo-mail.test", secret, fixedNow)
	gateway.client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.RequestURI() != "/webapi/v0/messages?folder=sent&limit=10" {
			t.Errorf("upstream URI = %q", r.URL.RequestURI())
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read upstream body: %v", err)
		}
		if !bytes.Equal(body, requestBody) {
			t.Errorf("upstream body = %q", body)
		}
		claims := verifyAssertionForTest(t, secret, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		if claims.Subject != "octo-user-a" || claims.SpaceID != "space-a" || claims.Method != http.MethodPost {
			t.Errorf("assertion identity = %#v", claims)
		}
		if claims.MailboxID != "42" {
			t.Errorf("assertion mailbox = %q", claims.MailboxID)
		}
		if claims.RequestURI != r.URL.RequestURI() {
			t.Errorf("assertion URI = %q, request URI = %q", claims.RequestURI, r.URL.RequestURI())
		}
		sum := sha256.Sum256(body)
		if claims.BodySHA256 != strings.ToLower(strings.TrimSpace(fmtHex(sum[:]))) {
			t.Errorf("assertion body digest = %q", claims.BodySHA256)
		}
		if r.Header.Get("X-Octo-Mailbox-ID") != "42" {
			t.Errorf("mailbox selection = %q", r.Header.Get("X-Octo-Mailbox-ID"))
		}
		if r.Header.Get("If-Range") != `"revision-1"` {
			t.Errorf("If-Range = %q", r.Header.Get("If-Range"))
		}
		for _, forbidden := range []string{"Token", "X-Space-ID", "Cookie", "X-Octo-UID"} {
			if value := r.Header.Get(forbidden); value != "" {
				t.Errorf("untrusted header %s was forwarded", forbidden)
			}
		}
		return &http.Response{
			StatusCode: http.StatusCreated,
			Header: http.Header{
				"Content-Type":      []string{"application/json"},
				"X-Upstream-Result": []string{"kept"},
			},
			Body: io.NopCloser(strings.NewReader(`{"id":"message-1"}`)),
		}, nil
	})}
	router := newProxyRouter(gateway, "octo-user-a", "space-a")
	req := httptest.NewRequest(http.MethodPost, "/v1/mail-gateway/webapi/v0/messages?folder=sent&limit=10", bytes.NewReader(requestBody))
	req.Header.Set("Authorization", "Basic attacker-controlled")
	req.Header.Set("Token", "browser-session")
	req.Header.Set("X-Space-ID", "space-a")
	req.Header.Set("Cookie", "session=private")
	req.Header.Set("X-Octo-UID", "forged-user")
	req.Header.Set("X-Octo-Mailbox-ID", "42")
	req.Header.Set("If-Range", `"revision-1"`)
	req.Header.Set("Connection", "X-Octo-Mailbox-ID")
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if recorder.Body.String() != `{"id":"message-1"}` {
		t.Fatalf("body = %s", recorder.Body.String())
	}
	if recorder.Header().Get("X-Upstream-Result") != "kept" {
		t.Fatalf("upstream response header was not preserved")
	}
}

func TestProxyFailsClosedWithoutIdentityOrSpace(t *testing.T) {
	for name, test := range map[string]struct {
		uid, spaceID string
		wantStatus   int
		wantCode     string
	}{
		"identity": {"", "space-a", http.StatusUnauthorized, "err.server.agent_mail_gateway.unauthorized"},
		"space":    {"user-a", "", http.StatusBadRequest, "err.server.agent_mail_gateway.space_required"},
	} {
		t.Run(name, func(t *testing.T) {
			gateway := &Gateway{}
			router := newProxyRouter(gateway, test.uid, test.spaceID)
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v1/mail-gateway/webapi/v0/mailboxes", nil))
			if recorder.Code != test.wantStatus {
				t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
			}
			assertErrorCode(t, recorder.Body.Bytes(), test.wantCode)
		})
	}
}

func TestProxyPreservesOctoMailBusinessError(t *testing.T) {
	secret := []byte(strings.Repeat("g", 32))
	gateway := newTestGateway(t, "http://octo-mail.test", secret, time.Now())
	wantBody := `{"error":{"code":"unauthorized","message":"authentication failed"}}`
	gateway.client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusUnauthorized,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(wantBody)),
		}, nil
	})}
	router := newProxyRouter(gateway, "user-a", "space-a")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v1/mail-gateway/webapi/v0/mailboxes", nil))

	if recorder.Code != http.StatusUnauthorized || recorder.Body.String() != wantBody {
		t.Fatalf("proxied error = status %d body %s", recorder.Code, recorder.Body.String())
	}
}

func TestProxyRejectsPathsOutsideWebAPI(t *testing.T) {
	secret := []byte(strings.Repeat("g", 32))
	var upstreamCalls atomic.Int32
	gateway := newTestGateway(t, "http://octo-mail.test", secret, time.Now())
	gateway.client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		upstreamCalls.Add(1)
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("ok"))}, nil
	})}
	router := newProxyRouter(gateway, "user-a", "space-a")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v1/mail-gateway/admin/tenants", nil))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("outside path status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	traversal := &url.URL{
		Path:    "/v1/mail-gateway/webapi/v0/../admin",
		RawPath: "/v1/mail-gateway/webapi/v0/%2e%2e/admin",
	}
	if _, err := gateway.upstreamURL(traversal); err == nil {
		t.Fatal("encoded path traversal was accepted")
	}
	if upstreamCalls.Load() != 0 {
		t.Fatalf("invalid paths reached upstream %d times", upstreamCalls.Load())
	}
}

func TestLoadGatewayConfigRequiresPairedStrongConfiguration(t *testing.T) {
	t.Setenv(envGatewayURL, "http://octo-mail:8090")
	t.Setenv(envGatewaySecret, "short")
	if _, err := loadGatewayConfig(); err == nil {
		t.Fatal("weak gateway secret was accepted")
	}

	t.Setenv(envGatewaySecret, strings.Repeat("s", 32))
	t.Setenv(envGatewayTimeout, "10s")
	cfg, err := loadGatewayConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.upstream.String() != "http://octo-mail:8090" || cfg.timeout != 10*time.Second {
		t.Fatalf("config = %#v", cfg)
	}
}

func TestGatewayHTTPClientBoundsHeadersButNotResponseBodyConsumption(t *testing.T) {
	t.Run("response headers", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			time.Sleep(150 * time.Millisecond)
			_, _ = writer.Write([]byte("late"))
		}))
		defer server.Close()

		_, err := newGatewayHTTPClient(30 * time.Millisecond).Get(server.URL)
		if err == nil {
			t.Fatal("response header timeout was not enforced")
		}
	})

	t.Run("response body", func(t *testing.T) {
		releaseBody := make(chan struct{})
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			writer.WriteHeader(http.StatusOK)
			_, _ = writer.Write([]byte("A"))
			writer.(http.Flusher).Flush()
			<-releaseBody
			_, _ = writer.Write([]byte("B"))
		}))
		defer server.Close()

		response, err := newGatewayHTTPClient(30 * time.Millisecond).Get(server.URL)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		first := make([]byte, 1)
		if _, err := io.ReadFull(response.Body, first); err != nil || string(first) != "A" {
			t.Fatalf("first body byte=%q err=%v", first, err)
		}
		time.Sleep(60 * time.Millisecond)
		close(releaseBody)
		rest, err := io.ReadAll(response.Body)
		if err != nil || string(rest) != "B" {
			t.Fatalf("remaining body=%q err=%v", rest, err)
		}
	})
}

func TestReadBoundedBodyRejectsOverflow(t *testing.T) {
	if _, err := readBoundedBody(strings.NewReader("1234"), 3); err == nil {
		t.Fatal("overflow body was accepted")
	}
}

func TestReadBoundedResponseBodyPreservesChunkedPayloadAndLimit(t *testing.T) {
	payload := bytes.Repeat([]byte("r"), responseBufferChunkBytes+17)
	buffered, length, err := readBoundedResponseBody(bytes.NewReader(payload), int64(len(payload)))
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(buffered)
	if err != nil {
		t.Fatal(err)
	}
	if length != int64(len(payload)) || !bytes.Equal(got, payload) {
		t.Fatalf("length=%d body length=%d", length, len(got))
	}
	if _, _, err := readBoundedResponseBody(bytes.NewReader(payload), int64(len(payload)-1)); !errors.Is(err, errBodyTooLarge) {
		t.Fatalf("overflow error=%v", err)
	}
	if _, _, err := readBoundedResponseBody(&partialFailingReadCloser{
		data: []byte("partial"),
		err:  io.ErrUnexpectedEOF,
	}, int64(len(payload))); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("truncation error=%v", err)
	}
}

func TestCopyBoundedBodyStreamsWithoutExceedingLimit(t *testing.T) {
	var dst bytes.Buffer
	if err := copyBoundedBody(&dst, strings.NewReader("123"), 3); err != nil {
		t.Fatalf("exact-limit response failed: %v", err)
	}
	if dst.String() != "123" {
		t.Fatalf("body=%q", dst.String())
	}

	dst.Reset()
	err := copyBoundedBody(&dst, strings.NewReader("1234"), 3)
	if !errors.Is(err, errBodyTooLarge) {
		t.Fatalf("overflow error=%v", err)
	}
	if dst.String() != "123" {
		t.Fatalf("forwarded body exceeded limit: %q", dst.String())
	}
}

func TestProxyRejectsKnownOversizedResponseBeforeWritingUpstreamStatus(t *testing.T) {
	gateway := newTestGateway(t, "http://octo-mail.test", []byte(strings.Repeat("g", 32)), time.Now())
	gateway.client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode:    http.StatusOK,
			ContentLength: maxResponseBytes + 1,
			Body:          failingReadCloser{err: errors.New("body must not be read")},
		}, nil
	})}
	recorder := httptest.NewRecorder()
	newProxyRouter(gateway, "user-a", "space-a").ServeHTTP(
		recorder, httptest.NewRequest(http.MethodGet, "/v1/mail-gateway/webapi/v0/mailboxes", nil),
	)
	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	assertErrorCode(t, recorder.Body.Bytes(), "err.server.agent_mail_gateway.bad_response")
}

func TestProxyStreamsSelfDelimitedUnknownLengthResponses(t *testing.T) {
	tests := map[string]struct {
		protoMajor       int
		transferEncoding []string
		uncompressed     bool
	}{
		"HTTP/1.1 chunked":           {protoMajor: 1, transferEncoding: []string{"chunked"}},
		"HTTP/1.1 decompressed gzip": {protoMajor: 1, uncompressed: true},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			gateway := newTestGateway(t, "http://octo-mail.test", []byte(strings.Repeat("g", 32)), time.Now())
			gateway.client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode:       http.StatusOK,
					ProtoMajor:       test.protoMajor,
					ContentLength:    -1,
					TransferEncoding: test.transferEncoding,
					Uncompressed:     test.uncompressed,
					Body:             io.NopCloser(strings.NewReader("complete")),
				}, nil
			})}
			server := httptest.NewServer(newProxyRouter(gateway, "user-a", "space-a"))
			defer server.Close()

			response, err := server.Client().Get(server.URL + "/v1/mail-gateway/webapi/v0/mailboxes")
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			if err != nil || response.StatusCode != http.StatusOK || string(body) != "complete" {
				t.Fatalf("status=%d body=%q err=%v", response.StatusCode, body, err)
			}
		})
	}
}

func TestProxyBuffersUnknownLengthResponseForRealHTTP2Client(t *testing.T) {
	gateway := newTestGateway(t, "http://octo-mail.test", []byte(strings.Repeat("g", 32)), time.Now())
	gateway.responseBodyBudget = semaphore.NewWeighted(maxResponseBytes)
	gateway.responseBodyBudgetWait = 100 * time.Millisecond
	gateway.client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode:       http.StatusOK,
			ProtoMajor:       2,
			ContentLength:    -1,
			TransferEncoding: nil,
			Body:             io.NopCloser(strings.NewReader("complete")),
		}, nil
	})}
	server := httptest.NewUnstartedServer(newProxyRouter(gateway, "user-a", "space-a"))
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()

	response, err := server.Client().Get(server.URL + "/v1/mail-gateway/webapi/v0/mailboxes")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.ProtoMajor != 2 || response.StatusCode != http.StatusOK || string(body) != "complete" {
		t.Fatalf("proto=%s status=%d body=%s", response.Proto, response.StatusCode, body)
	}
	if got := response.Header.Get("Content-Length"); got != strconv.Itoa(len("complete")) {
		t.Fatalf("Content-Length=%q", got)
	}
	release, err := gateway.acquireResponseBodyBudget(context.Background())
	if err != nil {
		t.Fatalf("successful response retained its buffering budget: %v", err)
	}
	release()
}

func TestProxyRejectsTruncatedUnknownLengthResponseForRealHTTP2Client(t *testing.T) {
	gateway := newTestGateway(t, "http://octo-mail.test", []byte(strings.Repeat("g", 32)), time.Now())
	gateway.client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode:    http.StatusOK,
			ProtoMajor:    2,
			ContentLength: -1,
			Body: &partialFailingReadCloser{
				data: []byte("partial upstream body"),
				err:  errors.New("truncated response"),
			},
		}, nil
	})}
	server := httptest.NewUnstartedServer(newProxyRouter(gateway, "user-a", "space-a"))
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()

	response, err := server.Client().Get(server.URL + "/v1/mail-gateway/webapi/v0/mailboxes")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.ProtoMajor != 2 || response.StatusCode != http.StatusBadGateway {
		t.Fatalf("proto=%s status=%d body=%s", response.Proto, response.StatusCode, body)
	}
	assertErrorCode(t, body, "err.server.agent_mail_gateway.bad_response")
	if bytes.Contains(body, []byte("partial upstream body")) {
		t.Fatalf("partial upstream body leaked in error response: %s", body)
	}
}

func TestProxyRejectsTruncatedChunkedUpstreamForRealHTTP2Client(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	upstreamDone := make(chan error, 1)
	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr != nil {
			upstreamDone <- acceptErr
			return
		}
		defer connection.Close()
		reader := bufio.NewReader(connection)
		for {
			line, readErr := reader.ReadString('\n')
			if readErr != nil {
				upstreamDone <- readErr
				return
			}
			if line == "\r\n" {
				break
			}
		}
		_, writeErr := io.WriteString(connection,
			"HTTP/1.1 200 OK\r\n"+
				"Content-Type: text/plain\r\n"+
				"Transfer-Encoding: chunked\r\n"+
				"Connection: close\r\n\r\n"+
				"10\r\npartial upstream\r\n")
		upstreamDone <- writeErr
	}()

	gateway := newTestGateway(t, "http://"+listener.Addr().String(), []byte(strings.Repeat("g", 32)), time.Now())
	server := httptest.NewUnstartedServer(newProxyRouter(gateway, "user-a", "space-a"))
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()

	response, err := server.Client().Get(server.URL + "/v1/mail-gateway/webapi/v0/mailboxes")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.ProtoMajor != 2 || response.StatusCode != http.StatusBadGateway {
		t.Fatalf("proto=%s status=%d body=%s", response.Proto, response.StatusCode, body)
	}
	assertErrorCode(t, body, "err.server.agent_mail_gateway.bad_response")
	if bytes.Contains(body, []byte("partial upstream")) {
		t.Fatalf("partial upstream body leaked in error response: %s", body)
	}
	if err := <-upstreamDone; err != nil {
		t.Fatalf("truncated upstream: %v", err)
	}
}

func TestProxyRejectsOversizedUnknownLengthResponseForRealHTTP2Client(t *testing.T) {
	gateway := newTestGateway(t, "http://octo-mail.test", []byte(strings.Repeat("g", 32)), time.Now())
	gateway.responseBodyBudget = semaphore.NewWeighted(maxResponseBytes)
	gateway.responseBodyBudgetWait = 100 * time.Millisecond
	gateway.client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode:    http.StatusOK,
			ProtoMajor:    2,
			ContentLength: -1,
			Body:          io.NopCloser(io.LimitReader(zeroReader{}, maxResponseBytes+1)),
		}, nil
	})}
	server := httptest.NewUnstartedServer(newProxyRouter(gateway, "user-a", "space-a"))
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()

	response, err := server.Client().Get(server.URL + "/v1/mail-gateway/webapi/v0/mailboxes")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.ProtoMajor != 2 || response.StatusCode != http.StatusBadGateway {
		t.Fatalf("proto=%s status=%d body=%s", response.Proto, response.StatusCode, body)
	}
	assertErrorCode(t, body, "err.server.agent_mail_gateway.bad_response")
	release, err := gateway.acquireResponseBodyBudget(context.Background())
	if err != nil {
		t.Fatalf("failed response retained its buffering budget: %v", err)
	}
	release()
}

func TestProxyRejectsHTTP2BufferingWhenResponseBudgetIsUnavailable(t *testing.T) {
	gateway := newTestGateway(t, "http://octo-mail.test", []byte(strings.Repeat("g", 32)), time.Now())
	gateway.responseBodyBudget = semaphore.NewWeighted(maxResponseBytes)
	gateway.responseBodyBudgetWait = 10 * time.Millisecond
	if err := gateway.responseBodyBudget.Acquire(context.Background(), maxResponseBytes); err != nil {
		t.Fatal(err)
	}
	defer gateway.responseBodyBudget.Release(maxResponseBytes)
	gateway.client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode:    http.StatusOK,
			ProtoMajor:    2,
			ContentLength: -1,
			Body:          failingReadCloser{err: errors.New("body must not be read without a budget")},
		}, nil
	})}
	server := httptest.NewUnstartedServer(newProxyRouter(gateway, "user-a", "space-a"))
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()

	response, err := server.Client().Get(server.URL + "/v1/mail-gateway/webapi/v0/mailboxes")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.ProtoMajor != 2 || response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("proto=%s status=%d body=%s", response.Proto, response.StatusCode, body)
	}
	assertErrorCode(t, body, "err.server.agent_mail_gateway.unavailable")
}

func TestProxyForwardsGzipResponseToHTTP2WithoutTransparentDecompression(t *testing.T) {
	payload := []byte(`{"messages":[]}`)
	var compressed bytes.Buffer
	gzipWriter := gzip.NewWriter(&compressed)
	if _, err := gzipWriter.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}

	upstreamEncoding := make(chan string, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		upstreamEncoding <- request.Header.Get("Accept-Encoding")
		writer.Header().Set("Content-Encoding", "gzip")
		writer.Header().Set("Content-Type", "application/json")
		writer.Header().Set("Content-Length", strconv.Itoa(compressed.Len()))
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write(compressed.Bytes())
	}))
	defer upstream.Close()

	gateway := newTestGateway(t, upstream.URL, []byte(strings.Repeat("g", 32)), time.Now())
	server := httptest.NewUnstartedServer(newProxyRouter(gateway, "user-a", "space-a"))
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()

	transport := server.Client().Transport.(*http.Transport).Clone()
	transport.DisableCompression = true
	client := &http.Client{Transport: transport}
	request, err := http.NewRequest(http.MethodGet, server.URL+"/v1/mail-gateway/webapi/v0/messages", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Accept-Encoding", "gzip")
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.ProtoMajor != 2 || response.StatusCode != http.StatusOK {
		t.Fatalf("proto=%s status=%d", response.Proto, response.StatusCode)
	}
	if response.Header.Get("Content-Encoding") != "gzip" || response.ContentLength != int64(compressed.Len()) {
		t.Fatalf("encoding=%q contentLength=%d", response.Header.Get("Content-Encoding"), response.ContentLength)
	}
	reader, err := gzip.NewReader(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := io.ReadAll(reader)
	if closeErr := reader.Close(); err == nil {
		err = closeErr
	}
	if err != nil || !bytes.Equal(decoded, payload) {
		t.Fatalf("decoded=%q err=%v", decoded, err)
	}
	if got := <-upstreamEncoding; got != "identity" {
		t.Fatalf("upstream Accept-Encoding=%q, want identity", got)
	}
}

func TestProxyRejectsCloseDelimitedUpstreamBeforeCommittingStatus(t *testing.T) {
	gateway := newTestGateway(t, "http://octo-mail.test", []byte(strings.Repeat("g", 32)), time.Now())
	gateway.client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode:    http.StatusOK,
			ProtoMajor:    1,
			ContentLength: -1,
			Close:         true,
			Body:          io.NopCloser(strings.NewReader(`{"mailboxes":[`)),
		}, nil
	})}
	recorder := httptest.NewRecorder()
	newProxyRouter(gateway, "user-a", "space-a").ServeHTTP(
		recorder, httptest.NewRequest(http.MethodGet, "/v1/mail-gateway/webapi/v0/mailboxes", nil),
	)
	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	assertErrorCode(t, recorder.Body.Bytes(), "err.server.agent_mail_gateway.bad_response")
}

func TestProxyRejectsUnknownLengthResponseForHTTP10Client(t *testing.T) {
	gateway := newTestGateway(t, "http://octo-mail.test", []byte(strings.Repeat("g", 32)), time.Now())
	gateway.client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode:       http.StatusOK,
			ProtoMajor:       1,
			ContentLength:    -1,
			TransferEncoding: []string{"chunked"},
			Body:             io.NopCloser(strings.NewReader("complete")),
		}, nil
	})}
	request := httptest.NewRequest(http.MethodGet, "/v1/mail-gateway/webapi/v0/mailboxes", nil)
	request.Proto = "HTTP/1.0"
	request.ProtoMajor = 1
	request.ProtoMinor = 0
	recorder := httptest.NewRecorder()
	newProxyRouter(gateway, "user-a", "space-a").ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	assertErrorCode(t, recorder.Body.Bytes(), "err.server.agent_mail_gateway.bad_response")
}

func TestProxyStreamsUnknownLengthResponseBeforeUpstreamCompletes(t *testing.T) {
	releaseUpstream := make(chan struct{})
	released := false
	defer func() {
		if !released {
			close(releaseUpstream)
		}
	}()
	gateway := newTestGateway(t, "http://octo-mail.test", []byte(strings.Repeat("g", 32)), time.Now())
	gateway.client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode:    http.StatusOK,
			ContentLength: -1,
			Body: &stagedReadCloser{
				first:   []byte("A"),
				rest:    []byte("B"),
				release: releaseUpstream,
			},
		}, nil
	})}
	server := httptest.NewServer(newProductionWrappedProxyRouter(gateway, "user-a", "space-a"))
	defer server.Close()

	type responseResult struct {
		response *http.Response
		err      error
	}
	responseCh := make(chan responseResult, 1)
	go func() {
		response, err := server.Client().Get(server.URL + "/v1/mail-gateway/webapi/v0/attachments/E1")
		responseCh <- responseResult{response: response, err: err}
	}()
	var result responseResult
	select {
	case result = <-responseCh:
	case <-time.After(time.Second):
		t.Fatal("response headers waited for the complete unknown-length upstream body")
	}
	if result.err != nil {
		t.Fatal(result.err)
	}
	defer result.response.Body.Close()
	first := make([]byte, 1)
	if _, err := io.ReadFull(result.response.Body, first); err != nil || string(first) != "A" {
		t.Fatalf("first body byte=%q err=%v", first, err)
	}
	close(releaseUpstream)
	released = true
	rest, err := io.ReadAll(result.response.Body)
	if err != nil || string(rest) != "B" {
		t.Fatalf("remaining body=%q err=%v", rest, err)
	}
}

func TestProxyAbortsUnknownLengthResponseOnStreamFailure(t *testing.T) {
	tests := map[string]struct {
		body      io.ReadCloser
		wantBytes int64
	}{
		"overflow": {
			body:      io.NopCloser(io.LimitReader(zeroReader{}, maxResponseBytes+1)),
			wantBytes: maxResponseBytes,
		},
		"read failure": {
			body:      &partialFailingReadCloser{data: []byte("AAA"), err: errors.New("truncated response")},
			wantBytes: 3,
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			gateway := newTestGateway(t, "http://octo-mail.test", []byte(strings.Repeat("g", 32)), time.Now())
			gateway.client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusOK, ContentLength: -1, Body: test.body}, nil
			})}
			server := httptest.NewServer(newProxyRouter(gateway, "user-a", "space-a"))
			defer server.Close()

			response, err := server.Client().Get(server.URL + "/v1/mail-gateway/webapi/v0/attachments/E1")
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			written, readErr := io.Copy(io.Discard, response.Body)
			if response.StatusCode != http.StatusOK || written != test.wantBytes || readErr == nil {
				t.Fatalf("status=%d bytes=%d readErr=%v", response.StatusCode, written, readErr)
			}
		})
	}
}

func TestProxyPreservesKnownLengthSoMidstreamFailureIsVisible(t *testing.T) {
	gateway := newTestGateway(t, "http://octo-mail.test", []byte(strings.Repeat("g", 32)), time.Now())
	gateway.client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode:    http.StatusOK,
			ContentLength: 1000,
			Body:          &partialFailingReadCloser{data: []byte("AAA"), err: errors.New("upstream reset")},
		}, nil
	})}
	server := httptest.NewServer(newProxyRouter(gateway, "user-a", "space-a"))
	defer server.Close()

	response, err := server.Client().Get(server.URL + "/v1/mail-gateway/webapi/v0/attachments/E1")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, readErr := io.ReadAll(response.Body)
	if string(body) != "AAA" || !errors.Is(readErr, io.ErrUnexpectedEOF) {
		t.Fatalf("body=%q readErr=%v, want a visible truncated-response error", body, readErr)
	}
}

func TestProxyAllowsHeadMetadataAboveResponseBodyLimit(t *testing.T) {
	gateway := newTestGateway(t, "http://octo-mail.test", []byte(strings.Repeat("g", 32)), time.Now())
	gateway.client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode:    http.StatusOK,
			ContentLength: maxResponseBytes + 1,
			Body:          http.NoBody,
		}, nil
	})}
	recorder := httptest.NewRecorder()
	newProxyRouter(gateway, "user-a", "space-a").ServeHTTP(
		recorder,
		httptest.NewRequest(http.MethodHead, "/v1/mail-gateway/webapi/v0/attachments/E1", nil),
	)
	if recorder.Code != http.StatusOK {
		t.Fatalf("HEAD status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get("Content-Length"); got != strconv.FormatInt(maxResponseBytes+1, 10) {
		t.Fatalf("HEAD Content-Length=%q", got)
	}
}

func TestLargeRequestBufferingHasConcurrencyBound(t *testing.T) {
	gateway := &Gateway{largeRequestSlots: make(chan struct{}, 1), largeRequestSlotWait: 10 * time.Millisecond}
	first := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("body"))
	first.ContentLength = largeRequestThreshold + 1
	release, err := gateway.acquireLargeRequestSlot(first)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	second := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("body")).WithContext(ctx)
	second.ContentLength = -1
	if _, err := gateway.acquireLargeRequestSlot(second); !errors.Is(err, context.Canceled) {
		t.Fatalf("second acquire error=%v, want context.Canceled", err)
	}

	third := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("body"))
	third.ContentLength = -1
	started := time.Now()
	if _, err := gateway.acquireLargeRequestSlot(third); !errors.Is(err, errLargeRequestSlotWait) {
		t.Fatalf("third acquire error=%v, want errLargeRequestSlotWait", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("bounded slot wait took %v", elapsed)
	}
}

func TestProxyRejectsKnownOversizedRequestBeforeWaitingForSlot(t *testing.T) {
	var upstreamCalls atomic.Int32
	gateway := newTestGateway(t, "http://octo-mail.test", []byte(strings.Repeat("g", 32)), time.Now())
	gateway.largeRequestSlots = make(chan struct{}, 1)
	gateway.largeRequestSlots <- struct{}{}
	gateway.largeRequestSlotWait = time.Second
	gateway.client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		upstreamCalls.Add(1)
		return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}, nil
	})}
	request := httptest.NewRequest(http.MethodPost, "/v1/mail-gateway/webapi/v0/messages", strings.NewReader("body"))
	request.ContentLength = maxRequestBytes + 1
	recorder := httptest.NewRecorder()
	started := time.Now()
	newProxyRouter(gateway, "user-a", "space-a").ServeHTTP(recorder, request)

	if recorder.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if time.Since(started) > 250*time.Millisecond {
		t.Fatal("known oversized request waited for a buffering slot")
	}
	if upstreamCalls.Load() != 0 {
		t.Fatalf("oversized request reached upstream %d times", upstreamCalls.Load())
	}
}

func TestProxyReleasesLargeRequestSlotAfterBuffering(t *testing.T) {
	gateway := newTestGateway(t, "http://octo-mail.test", []byte(strings.Repeat("g", 32)), time.Now())
	gateway.nonceSource = zeroReader{}
	gateway.largeRequestSlots = make(chan struct{}, 1)
	gateway.largeRequestSlotWait = 100 * time.Millisecond
	firstAtUpstream := make(chan struct{})
	releaseFirst := make(chan struct{})
	var upstreamCalls atomic.Int32
	gateway.client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		if upstreamCalls.Add(1) == 1 {
			close(firstAtUpstream)
			<-releaseFirst
		}
		return &http.Response{
			StatusCode:    http.StatusOK,
			ContentLength: 2,
			Body:          io.NopCloser(strings.NewReader("ok")),
		}, nil
	})}
	router := newProxyRouter(gateway, "user-a", "space-a")
	firstDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		request := httptest.NewRequest(http.MethodPost, "/v1/mail-gateway/webapi/v0/messages", strings.NewReader("first"))
		request.ContentLength = largeRequestThreshold + 1
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, request)
		firstDone <- recorder
	}()
	<-firstAtUpstream

	secondRequest := httptest.NewRequest(http.MethodPost, "/v1/mail-gateway/webapi/v0/messages", strings.NewReader("second"))
	secondRequest.ContentLength = largeRequestThreshold + 1
	secondRecorder := httptest.NewRecorder()
	router.ServeHTTP(secondRecorder, secondRequest)
	if secondRecorder.Code != http.StatusOK || secondRecorder.Body.String() != "ok" {
		t.Fatalf("second response status=%d body=%q", secondRecorder.Code, secondRecorder.Body.String())
	}

	close(releaseFirst)
	firstRecorder := <-firstDone
	if firstRecorder.Code != http.StatusOK || firstRecorder.Body.String() != "ok" {
		t.Fatalf("first response status=%d body=%q", firstRecorder.Code, firstRecorder.Body.String())
	}
}

func TestProxyStalledRequestBodyReleasesLargeRequestSlot(t *testing.T) {
	gateway := newTestGateway(t, "http://octo-mail.test", []byte(strings.Repeat("g", 32)), time.Now())
	gateway.cfg.timeout = 20 * time.Millisecond
	gateway.largeRequestSlots = make(chan struct{}, 1)
	gateway.largeRequestSlotWait = 100 * time.Millisecond
	gateway.client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, ContentLength: 2, Body: io.NopCloser(strings.NewReader("ok"))}, nil
	})}
	router := newProxyRouter(gateway, "user-a", "space-a")
	stalled := newBlockingReadCloser()
	request := httptest.NewRequest(http.MethodPost, "/v1/mail-gateway/webapi/v0/messages", stalled)
	request.ContentLength = largeRequestThreshold + 1
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, request)
		done <- recorder
	}()

	select {
	case recorder := <-done:
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("stalled request status=%d body=%s", recorder.Code, recorder.Body.String())
		}
	case <-time.After(250 * time.Millisecond):
		_ = stalled.Close()
		<-done
		t.Fatal("stalled request body held the large-request slot past the gateway timeout")
	}

	second := httptest.NewRequest(http.MethodPost, "/v1/mail-gateway/webapi/v0/messages", strings.NewReader("next"))
	second.ContentLength = largeRequestThreshold + 1
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, second)
	if recorder.Code != http.StatusOK || recorder.Body.String() != "ok" {
		t.Fatalf("released slot status=%d body=%q", recorder.Code, recorder.Body.String())
	}
}

func TestProxyStalledNetworkRequestBodyReleasesLargeRequestSlot(t *testing.T) {
	gateway := newTestGateway(t, "http://octo-mail.test", []byte(strings.Repeat("g", 32)), time.Now())
	gateway.cfg.timeout = 50 * time.Millisecond
	gateway.largeRequestSlots = make(chan struct{}, 1)
	gateway.largeRequestSlotWait = 250 * time.Millisecond
	gateway.client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, ContentLength: 2, Body: io.NopCloser(strings.NewReader("ok"))}, nil
	})}
	server := httptest.NewServer(newProductionWrappedProxyRouter(gateway, "user-a", "space-a"))
	defer server.Close()

	reader, writer := io.Pipe()
	request, err := http.NewRequest(http.MethodPost, server.URL+"/v1/mail-gateway/webapi/v0/messages", reader)
	if err != nil {
		t.Fatal(err)
	}
	request.ContentLength = largeRequestThreshold + 1
	done := make(chan *http.Response, 1)
	errCh := make(chan error, 1)
	go func() {
		response, requestErr := server.Client().Do(request)
		if requestErr != nil {
			errCh <- requestErr
			return
		}
		done <- response
	}()
	if _, err := writer.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}

	select {
	case response := <-done:
		defer response.Body.Close()
		if response.StatusCode != http.StatusBadRequest {
			body, _ := io.ReadAll(response.Body)
			t.Fatalf("stalled network request status=%d body=%s", response.StatusCode, body)
		}
	case err := <-errCh:
		t.Fatalf("stalled network request failed before the gateway response: %v", err)
	case <-time.After(time.Second):
		_ = writer.Close()
		t.Fatal("real network request body outlived the gateway progress timeout")
	}
	_ = writer.Close()

	body := bytes.Repeat([]byte("a"), int(largeRequestThreshold+1))
	response, err := server.Client().Post(
		server.URL+"/v1/mail-gateway/webapi/v0/messages",
		"application/octet-stream",
		bytes.NewReader(body),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || string(responseBody) != "ok" {
		t.Fatalf("released network slot status=%d body=%q", response.StatusCode, responseBody)
	}
}

func TestProxyUnknownLengthOverflowReturns413AndReleasesLargeRequestSlot(t *testing.T) {
	gateway := newTestGateway(t, "http://octo-mail.test", []byte(strings.Repeat("g", 32)), time.Now())
	gateway.cfg.timeout = time.Second
	gateway.largeRequestSlots = make(chan struct{}, 1)
	gateway.largeRequestSlotWait = time.Second
	gateway.requestBodyBudget = semaphore.NewWeighted(maxRequestBytes)
	gateway.client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, ContentLength: 2, Body: io.NopCloser(strings.NewReader("ok"))}, nil
	})}
	server := httptest.NewServer(newProductionWrappedProxyRouter(gateway, "user-a", "space-a"))
	defer server.Close()

	reader, writer := io.Pipe()
	defer writer.Close()
	request, err := http.NewRequest(http.MethodPost, server.URL+"/v1/mail-gateway/webapi/v0/messages", reader)
	if err != nil {
		t.Fatal(err)
	}
	request.ContentLength = -1
	if request.ContentLength != -1 {
		t.Fatalf("request ContentLength=%d, want unknown", request.ContentLength)
	}
	type responseResult struct {
		response *http.Response
		err      error
	}
	responseCh := make(chan responseResult, 1)
	go func() {
		response, requestErr := server.Client().Do(request)
		responseCh <- responseResult{response: response, err: requestErr}
	}()
	uploadCh := make(chan error, 1)
	go func() {
		_, uploadErr := io.CopyN(writer, zeroReader{}, maxRequestBytes+1)
		uploadCh <- uploadErr
	}()

	select {
	case uploadErr := <-uploadCh:
		if uploadErr != nil {
			t.Fatalf("upload overflow probe: %v", uploadErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("unknown-length overflow upload did not reach the gateway limit")
	}

	var result responseResult
	select {
	case result = <-responseCh:
	case <-time.After(2 * time.Second):
		t.Fatal("unknown-length overflow did not produce a response")
	}
	if result.err != nil {
		t.Fatalf("unknown-length overflow request: %v", result.err)
	}
	defer result.response.Body.Close()
	responseBody, err := io.ReadAll(result.response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if result.response.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("overflow status=%d body=%s", result.response.StatusCode, responseBody)
	}
	assertErrorCode(t, responseBody, "err.server.agent_mail_gateway.payload_too_large")

	// The rejected chunked body must not retain either the only concurrency
	// slot or its full 64 MiB budget reservation.
	nextBody := bytes.Repeat([]byte("a"), int(largeRequestThreshold+1))
	nextResponse, err := server.Client().Post(
		server.URL+"/v1/mail-gateway/webapi/v0/messages",
		"application/octet-stream",
		bytes.NewReader(nextBody),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer nextResponse.Body.Close()
	nextResponseBody, err := io.ReadAll(nextResponse.Body)
	if err != nil {
		t.Fatal(err)
	}
	if nextResponse.StatusCode != http.StatusOK || string(nextResponseBody) != "ok" {
		t.Fatalf("request after overflow status=%d body=%q", nextResponse.StatusCode, nextResponseBody)
	}
}

func TestProxyBoundsStalledUpstreamUpload(t *testing.T) {
	gateway := newTestGateway(t, "http://octo-mail.test", []byte(strings.Repeat("g", 32)), time.Now())
	gateway.cfg.timeout = 20 * time.Millisecond
	gateway.client = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		<-request.Context().Done()
		return nil, request.Context().Err()
	})}
	requestContext, cancel := context.WithCancel(context.Background())
	defer cancel()
	request := httptest.NewRequest(http.MethodPost, "/v1/mail-gateway/webapi/v0/messages", strings.NewReader("body")).WithContext(requestContext)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		recorder := httptest.NewRecorder()
		newProxyRouter(gateway, "user-a", "space-a").ServeHTTP(recorder, request)
		done <- recorder
	}()
	select {
	case recorder := <-done:
		if recorder.Code != http.StatusServiceUnavailable {
			t.Fatalf("stalled upstream status=%d body=%s", recorder.Code, recorder.Body.String())
		}
	case <-time.After(250 * time.Millisecond):
		cancel()
		<-done
		t.Fatal("stalled upstream upload outlived the gateway progress timeout")
	}
}

func TestProxyBoundsStalledUpstreamResponseBody(t *testing.T) {
	gateway := newTestGateway(t, "http://octo-mail.test", []byte(strings.Repeat("g", 32)), time.Now())
	gateway.cfg.timeout = 20 * time.Millisecond
	stalled := newBlockingReadCloser()
	gateway.client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, ContentLength: 1, Body: stalled}, nil
	})}
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		recorder := httptest.NewRecorder()
		newProxyRouter(gateway, "user-a", "space-a").ServeHTTP(
			recorder,
			httptest.NewRequest(http.MethodGet, "/v1/mail-gateway/webapi/v0/messages", nil),
		)
		done <- recorder
	}()
	select {
	case recorder := <-done:
		if recorder.Code != http.StatusOK {
			t.Fatalf("stalled response status=%d body=%s", recorder.Code, recorder.Body.String())
		}
	case <-time.After(250 * time.Millisecond):
		_ = stalled.Close()
		<-done
		t.Fatal("stalled upstream response body outlived the gateway progress timeout")
	}
}

func TestProxyBoundsStalledDownstreamWriteAndReleasesRequestBudget(t *testing.T) {
	const requestSize = largeRequestThreshold + 1
	gateway := newTestGateway(t, "http://octo-mail.test", []byte(strings.Repeat("g", 32)), time.Now())
	gateway.cfg.timeout = 100 * time.Millisecond
	gateway.requestBodyBudget = semaphore.NewWeighted(requestSize)
	gateway.largeRequestSlotWait = 100 * time.Millisecond
	upstreamBody := newCloseTrackingReadCloser(strings.NewReader("response"))
	gateway.client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode:    http.StatusOK,
			ContentLength: int64(len("response")),
			Body:          upstreamBody,
		}, nil
	})}

	request := httptest.NewRequest(
		http.MethodPost,
		"/v1/mail-gateway/webapi/v0/messages",
		bytes.NewReader(make([]byte, requestSize)),
	)
	writer := newDeadlineBlockingResponseWriter()
	done := make(chan struct{})
	go func() {
		newProxyRouter(gateway, "user-a", "space-a").ServeHTTP(writer, request)
		close(done)
	}()

	select {
	case <-writer.writeStarted:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("downstream response write did not start")
	}
	if !gateway.requestBodyBudget.TryAcquire(requestSize) {
		t.Fatal("request body budget remained held after the upstream request body closed")
	}
	gateway.requestBodyBudget.Release(requestSize)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("stalled downstream write outlived the gateway progress timeout")
	}
	select {
	case <-upstreamBody.closed:
	default:
		t.Fatal("stalled downstream write did not close the upstream response body")
	}
	if !writer.sawWriteDeadline() {
		t.Fatal("downstream response write was not given a progress deadline")
	}
	if !writer.deadlineCleared() {
		t.Fatal("downstream response deadline was not cleared after the handler returned")
	}
	if !gateway.requestBodyBudget.TryAcquire(requestSize) {
		t.Fatal("request body budget remained held after the stalled handler returned")
	}
	gateway.requestBodyBudget.Release(requestSize)
}

func TestRequestBodyBudgetReleasesOnlyAfterUpstreamRequestBodyCloses(t *testing.T) {
	const requestSize = largeRequestThreshold + 1
	gateway := newTestGateway(t, "http://octo-mail.test", []byte(strings.Repeat("g", 32)), time.Now())
	gateway.nonceSource = zeroReader{}
	gateway.requestBodyBudget = semaphore.NewWeighted(requestSize)
	gateway.largeRequestSlotWait = 100 * time.Millisecond
	firstAtUpstream := make(chan struct{})
	closeUpstreamRequestBody := make(chan struct{})
	upstreamRequestBodyClosed := make(chan struct{})
	upstreamResponseBody := newBlockingReadCloser()
	gateway.client = &http.Client{Transport: rawRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		close(firstAtUpstream)
		go func() {
			<-closeUpstreamRequestBody
			_ = request.Body.Close()
			close(upstreamRequestBodyClosed)
		}()
		return &http.Response{
			StatusCode:    http.StatusOK,
			ContentLength: 1,
			Body:          upstreamResponseBody,
		}, nil
	})}
	router := newProxyRouter(gateway, "user-a", "space-a")
	firstDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/v1/mail-gateway/webapi/v0/messages", strings.NewReader("1234"))
		request.ContentLength = requestSize
		router.ServeHTTP(recorder, request)
		firstDone <- recorder
	}()
	<-firstAtUpstream

	if gateway.requestBodyBudget.TryAcquire(requestSize) {
		gateway.requestBodyBudget.Release(requestSize)
		t.Fatal("request body budget was released before the upstream request body closed")
	}

	close(closeUpstreamRequestBody)
	select {
	case <-upstreamRequestBodyClosed:
	case <-time.After(time.Second):
		t.Fatal("upstream request body did not close")
	}
	if !gateway.requestBodyBudget.TryAcquire(requestSize) {
		t.Fatal("request body budget remained held after the upstream request body closed")
	}
	gateway.requestBodyBudget.Release(requestSize)
	select {
	case <-firstDone:
		t.Fatal("handler completed before its upstream response body was released")
	default:
	}

	_ = upstreamResponseBody.Close()
	select {
	case first := <-firstDone:
		if first.Code != http.StatusOK {
			t.Fatalf("first status=%d body=%q", first.Code, first.Body.String())
		}
	case <-time.After(time.Second):
		t.Fatal("handler did not finish after the upstream response body closed")
	}
}

func TestReleasingRequestBodyDropsBufferedReaderBeforeReleasingBudget(t *testing.T) {
	releaseCalls := 0
	var requestBody *releasingRequestBody
	requestBody = newReleasingRequestBody([]byte("buffered"), nil, func() {
		requestBody.mu.Lock()
		defer requestBody.mu.Unlock()
		if requestBody.reader != nil {
			t.Error("buffered reader was still reachable when the budget was released")
		}
		releaseCalls++
	})
	if err := requestBody.Close(); err != nil {
		t.Fatal(err)
	}
	if err := requestBody.Close(); err != nil {
		t.Fatal(err)
	}
	if releaseCalls != 1 {
		t.Fatalf("release calls=%d, want 1", releaseCalls)
	}
	var probe [1]byte
	if n, err := requestBody.Read(probe[:]); n != 0 || !errors.Is(err, io.EOF) {
		t.Fatalf("read after close = (%d, %v), want EOF", n, err)
	}
}

func TestProxyOverridesUpstreamCachePolicyForPrivateMail(t *testing.T) {
	gateway := newTestGateway(t, "http://octo-mail.test", []byte(strings.Repeat("g", 32)), time.Now())
	gateway.client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode:    http.StatusOK,
			ContentLength: 2,
			Header: http.Header{
				"Cache-Control": []string{"public, max-age=3600"},
				"Vary":          []string{"Accept-Encoding"},
			},
			Body: io.NopCloser(strings.NewReader("ok")),
		}, nil
	})}
	recorder := httptest.NewRecorder()
	newProxyRouter(gateway, "user-a", "space-a").ServeHTTP(
		recorder,
		httptest.NewRequest(http.MethodGet, "/v1/mail-gateway/webapi/v0/messages", nil),
	)
	if recorder.Code != http.StatusOK || recorder.Body.String() != "ok" {
		t.Fatalf("status=%d body=%q", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get("Cache-Control"); got != "private, no-store" {
		t.Fatalf("Cache-Control=%q", got)
	}
	if got := recorder.Header().Get("Vary"); got != "token, X-Space-ID, X-Octo-Mailbox-ID" {
		t.Fatalf("Vary=%q", got)
	}
}

func TestUnknownLengthRequestReservesMaximumBodyBudget(t *testing.T) {
	gateway := &Gateway{
		requestBodyBudget:    semaphore.NewWeighted(maxRequestBytes),
		largeRequestSlotWait: 10 * time.Millisecond,
	}
	unknown := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("body"))
	unknown.ContentLength = -1
	release, err := gateway.acquireRequestBodyBudget(unknown)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	small := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("x"))
	small.ContentLength = 1
	releaseSmall, err := gateway.acquireRequestBodyBudget(small)
	if err != nil {
		t.Fatalf("small request became collateral damage of the large-body budget: %v", err)
	}
	releaseSmall()

	large := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("large"))
	large.ContentLength = largeRequestThreshold + 1
	if _, err := gateway.acquireRequestBodyBudget(large); !errors.Is(err, errRequestBodyBudgetWait) {
		t.Fatalf("large request error=%v, want unknown request to reserve the full limit", err)
	}
}

func TestResponseBufferingHasAggregateBudget(t *testing.T) {
	gateway := &Gateway{
		responseBodyBudget:     semaphore.NewWeighted(maxResponseBytes),
		responseBodyBudgetWait: 10 * time.Millisecond,
	}
	release, err := gateway.acquireResponseBodyBudget(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := gateway.acquireResponseBodyBudget(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled acquire error=%v, want context.Canceled", err)
	}
	if _, err := gateway.acquireResponseBodyBudget(context.Background()); !errors.Is(err, errResponseBodyBudgetWait) {
		t.Fatalf("bounded acquire error=%v, want errResponseBodyBudgetWait", err)
	}

	release()
	releaseAfterWait, err := gateway.acquireResponseBodyBudget(context.Background())
	if err != nil {
		t.Fatalf("released response budget remained unavailable: %v", err)
	}
	releaseAfterWait()
}

func TestConnectionNominatedHeadersAreNotForwarded(t *testing.T) {
	requestSource := http.Header{
		"Connection":        []string{"Content-Type, X-Octo-Mailbox-ID"},
		"Content-Type":      []string{"application/json"},
		"X-Octo-Mailbox-ID": []string{"42"},
	}
	requestDestination := make(http.Header)
	copyRequestHeaders(requestDestination, requestSource)
	if requestDestination.Get("Content-Type") != "" || requestDestination.Get("X-Octo-Mailbox-ID") != "" {
		t.Fatalf("connection-nominated request headers forwarded: %#v", requestDestination)
	}

	responseSource := http.Header{
		"Connection":        []string{"X-Upstream-Result"},
		"X-Upstream-Result": []string{"private"},
		"Content-Type":      []string{"application/json"},
	}
	responseDestination := make(http.Header)
	copyResponseHeaders(responseDestination, responseSource)
	if responseDestination.Get("X-Upstream-Result") != "" {
		t.Fatalf("connection-nominated response header forwarded: %#v", responseDestination)
	}
	if responseDestination.Get("Content-Type") != "application/json" {
		t.Fatalf("end-to-end response header was dropped: %#v", responseDestination)
	}
}

func newTestGateway(t *testing.T, upstreamURL string, secret []byte, now time.Time) *Gateway {
	t.Helper()
	parsed, err := url.Parse(upstreamURL)
	if err != nil {
		t.Fatal(err)
	}
	return &Gateway{
		Log:                    log.NewTLog("AgentMailGatewayTest"),
		cfg:                    gatewayConfig{upstream: parsed, secret: secret, timeout: 5 * time.Second},
		client:                 newGatewayHTTPClient(5 * time.Second),
		now:                    func() time.Time { return now },
		nonceSource:            bytes.NewReader(make([]byte, 16)),
		largeRequestSlots:      make(chan struct{}, maxConcurrentLargeRequests),
		largeRequestSlotWait:   defaultLargeRequestSlotWait,
		requestBodyBudget:      semaphore.NewWeighted(maxInFlightRequestBodyBytes),
		responseBodyBudget:     semaphore.NewWeighted(maxInFlightResponseBodyBytes),
		responseBodyBudgetWait: defaultResponseBodyBudgetWait,
		provisionIdentity:      func(context.Context, string, string) error { return nil },
		resolveLocalpart:       func(context.Context, string) (string, error) { return "test-user", nil },
	}
}

func newProxyRouter(gateway *Gateway, uid, spaceID string) *wkhttp.WKHttp {
	router := wkhttp.New()
	router.SetErrorRenderer(i18n.NewErrorRenderer(i18n.NewLocalizer(i18n.SourceLanguage)))
	registerProxyRoute(router, gateway, uid, spaceID)
	return router
}

func newProductionWrappedProxyRouter(gateway *Gateway, uid, spaceID string) *wkhttp.WKHttp {
	router := wkhttp.New()
	router.SetErrorRenderer(i18n.NewErrorRenderer(i18n.NewLocalizer(i18n.SourceLanguage)))
	router.UseGin(i18n.EarlyMiddleware(i18n.MiddlewareOptions{DefaultLanguage: i18n.SourceLanguage}))
	router.UseGin(appmetrics.NewHTTPMetrics(prometheus.NewRegistry()).GinMiddleware())
	registerProxyRoute(router, gateway, uid, spaceID)
	return router
}

func registerProxyRoute(router *wkhttp.WKHttp, gateway *Gateway, uid, spaceID string) {
	router.Any(gatewayRoutePrefix+"/*path", func(c *wkhttp.Context) {
		if uid != "" {
			c.Set("uid", uid)
		}
		if spaceID != "" {
			c.Set("space_id", spaceID)
		}
		gateway.proxy(c)
	})
}

func verifyAssertionForTest(t *testing.T, secret []byte, token string) assertionClaims {
	t.Helper()
	if !strings.HasPrefix(token, assertionPrefix) {
		t.Fatalf("missing assertion prefix: %q", token)
	}
	payloadText, signatureText, ok := strings.Cut(strings.TrimPrefix(token, assertionPrefix), ".")
	if !ok {
		t.Fatalf("invalid assertion shape")
	}
	signature, err := base64.RawURLEncoding.DecodeString(signatureText)
	if err != nil {
		t.Fatal(err)
	}
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write([]byte(payloadText))
	if !hmac.Equal(signature, mac.Sum(nil)) {
		t.Fatal("invalid assertion signature")
	}
	payload, err := base64.RawURLEncoding.DecodeString(payloadText)
	if err != nil {
		t.Fatal(err)
	}
	var claims assertionClaims
	if err := json.Unmarshal(payload, &claims); err != nil {
		t.Fatal(err)
	}
	return claims
}

func fmtHex(value []byte) string {
	const digits = "0123456789abcdef"
	result := make([]byte, len(value)*2)
	for i, b := range value {
		result[i*2] = digits[b>>4]
		result[i*2+1] = digits[b&0x0f]
	}
	return string(result)
}

func assertErrorCode(t *testing.T, body []byte, want string) {
	t.Helper()
	var envelope struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("decode error envelope: %v; body=%s", err, body)
	}
	if envelope.Error.Code != want {
		t.Fatalf("error code = %q, want %q", envelope.Error.Code, want)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (response *http.Response, err error) {
	if request.Body != nil {
		defer request.Body.Close()
	}
	return f(request)
}

// rawRoundTripFunc is reserved for tests that exercise the RoundTripper
// contract allowing Request.Body to close asynchronously after RoundTrip
// returns. Most test transports use roundTripFunc so they close bodies just as
// production net/http transports must.
type rawRoundTripFunc func(*http.Request) (*http.Response, error)

func (f rawRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

type failingReadCloser struct {
	err error
}

func (r failingReadCloser) Read([]byte) (int, error) { return 0, r.err }
func (failingReadCloser) Close() error               { return nil }

type partialFailingReadCloser struct {
	data []byte
	err  error
}

func (r *partialFailingReadCloser) Read(dst []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, r.err
	}
	n := copy(dst, r.data)
	r.data = r.data[n:]
	return n, nil
}

func (*partialFailingReadCloser) Close() error { return nil }

type stagedReadCloser struct {
	first   []byte
	rest    []byte
	release <-chan struct{}
	stage   int
}

func (r *stagedReadCloser) Read(dst []byte) (int, error) {
	switch r.stage {
	case 0:
		r.stage = 1
		return copy(dst, r.first), nil
	case 1:
		<-r.release
		r.stage = 2
		return copy(dst, r.rest), nil
	default:
		return 0, io.EOF
	}
}

func (*stagedReadCloser) Close() error { return nil }

type blockingReadCloser struct {
	closed chan struct{}
	once   sync.Once
}

func newBlockingReadCloser() *blockingReadCloser {
	return &blockingReadCloser{closed: make(chan struct{})}
}

func (r *blockingReadCloser) Read([]byte) (int, error) {
	<-r.closed
	return 0, errors.New("body closed")
}

func (r *blockingReadCloser) Close() error {
	r.once.Do(func() { close(r.closed) })
	return nil
}

type closeTrackingReadCloser struct {
	io.Reader
	closed chan struct{}
	once   sync.Once
}

func newCloseTrackingReadCloser(reader io.Reader) *closeTrackingReadCloser {
	return &closeTrackingReadCloser{Reader: reader, closed: make(chan struct{})}
}

func (r *closeTrackingReadCloser) Close() error {
	r.once.Do(func() { close(r.closed) })
	return nil
}

type deadlineBlockingResponseWriter struct {
	header       http.Header
	mu           sync.Mutex
	deadlines    []time.Time
	writeStarted chan struct{}
	writeOnce    sync.Once
}

func newDeadlineBlockingResponseWriter() *deadlineBlockingResponseWriter {
	return &deadlineBlockingResponseWriter{
		header:       make(http.Header),
		writeStarted: make(chan struct{}),
	}
}

func (w *deadlineBlockingResponseWriter) Header() http.Header { return w.header }

func (*deadlineBlockingResponseWriter) WriteHeader(int) {}

func (w *deadlineBlockingResponseWriter) Write([]byte) (int, error) {
	w.writeOnce.Do(func() { close(w.writeStarted) })
	w.mu.Lock()
	deadline := w.deadlines[len(w.deadlines)-1]
	w.mu.Unlock()
	if wait := time.Until(deadline); wait > 0 {
		timer := time.NewTimer(wait)
		defer timer.Stop()
		<-timer.C
	}
	return 0, context.DeadlineExceeded
}

func (*deadlineBlockingResponseWriter) Flush() {}

func (w *deadlineBlockingResponseWriter) SetWriteDeadline(deadline time.Time) error {
	w.mu.Lock()
	w.deadlines = append(w.deadlines, deadline)
	w.mu.Unlock()
	return nil
}

func (w *deadlineBlockingResponseWriter) sawWriteDeadline() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, deadline := range w.deadlines {
		if !deadline.IsZero() {
			return true
		}
	}
	return false
}

func (w *deadlineBlockingResponseWriter) deadlineCleared() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.deadlines) > 0 && w.deadlines[len(w.deadlines)-1].IsZero()
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}
