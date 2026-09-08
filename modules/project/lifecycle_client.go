package project

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/Mininglamp-OSS/octo-server/pkg/octosign"
)

// Wire protocol version, sent as a header rather than in the body so the peer can
// route and validate without parsing the body first. See the contract, §1.
const (
	lifecycleEventVersionHeader = "X-Octo-Event-Version"
	lifecycleEventVersion       = "1"

	// lifecycleEventMinSecretBytes is the HMAC key floor.
	//
	// Same value as projectprovision's minSecretBytes, but an INDEPENDENT
	// constant: that one is unexported, and widening a package's API for a
	// number is the larger change. Nothing enforces that the two stay equal —
	// they are two floors on two different secrets, and either can be raised
	// alone.
	lifecycleEventMinSecretBytes = 32
)

// The outbound half of the project lifecycle outbox .
//
// One POST, and a classification of what came back. The classification is the
// substance of this file: whether a failure retries or is terminal decides
// whether an undelivered revocation eventually lands or stops forever, and
// getting it backwards is silent in both directions — a terminal error retried
// forever hides a bug behind a growing backlog, and a transient error treated as
// terminal abandons a revocation the peer would have accepted a second later.

// Error classes. Low-cardinality enum, used as a metric label and written to
// last_error, so it must never carry a response body or an id.
const (
	lifecycleErrNone = ""
	// lifecycleErrNetwork covers dial failures, timeouts and truncated responses.
	lifecycleErrNetwork = "network"
	// lifecycleErrServer is a peer 5xx.
	lifecycleErrServer = "server"
	// lifecycleErrThrottled is 429 or 408 — the peer is asking for less, not saying no.
	lifecycleErrThrottled = "throttled"
	// lifecycleErrAuth is 401/403.
	lifecycleErrAuth = "auth"
	// lifecycleErrConflict is 409: the peer already holds this event id with
	// different content, or the project id collides with a different entity.
	lifecycleErrConflict = "conflict"
	// lifecycleErrRejected is any other 4xx: the peer will not accept this payload.
	lifecycleErrRejected = "rejected"
)

// lifecycleDeliveryResult is what one attempt produced.
type lifecycleDeliveryResult struct {
	// OK means the peer accepted the event. It covers both "applied" and
	// "ignored": ignored is a SUCCESS — the peer decided this statement was
	// superseded by one it already applied, which is exactly the ordering
	// behaviour the version field exists to produce. Retrying an ignored event
	// would loop forever.
	OK bool
	// Retryable distinguishes "try again later" from "never".
	Retryable bool
	Class     string
	// Detail is a short, low-cardinality summary safe for last_error and logs.
	// It carries the status code and, when present, the peer error code — never
	// a response body, never the credential.
	Detail string
}

// lifecycleSender is the seam the worker talks to.
//
// An interface so worker behaviour — backoff, abandonment, lease handling — is
// testable without an HTTP server, and so a test can produce a 409 or a timeout
// deterministically rather than by arranging one.
type lifecycleSender interface {
	Send(ctx context.Context, env lifecycleEventEnvelope) lifecycleDeliveryResult
}

// lifecycleHTTPClient is the production sender.
//
// The endpoint is configured ABSOLUTE, path included, rather than assembled from
// a base URL here. The signature covers the PATH, so a path this side builds and
// a path the peer serves have to be the same string; configuring the whole thing
// is the only way to be sure of that, and internal/projectprovision made the same
// call for the same reason.
type lifecycleHTTPClient struct {
	url    string
	path   string
	secret string
	clock  func() time.Time
	client *http.Client
}

func newLifecycleHTTPClient(rawURL, secret string, timeout time.Duration) (*lifecycleHTTPClient, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("project: lifecycle event url: %w", err)
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return nil, fmt.Errorf("project: lifecycle event url must be absolute, got %q", rawURL)
	}
	// A URL with no path would sign the empty string, and the peer would verify
	// against whatever path its router matched — the two would never agree.
	if parsed.EscapedPath() == "" || parsed.EscapedPath() == "/" {
		return nil, fmt.Errorf("project: lifecycle event url must include the event path")
	}
	// A floor on the key, the same 32 bytes projectprovision.ValidateTarget
	// requires of the provisioning secrets, and here for the same reason: HMAC's
	// strength is the key's, and a short shared secret is guessable offline from
	// one captured request. Length only — the message never reports the observed
	// length, which would be a (small) oracle in a log.
	if len(secret) < lifecycleEventMinSecretBytes {
		return nil, fmt.Errorf("project: lifecycle event secret must be at least %d bytes",
			lifecycleEventMinSecretBytes)
	}
	if parsed.RawQuery != "" {
		// Refused rather than silently dropped: the signature does NOT cover the
		// query, so anything put there is unauthenticated and an on-path rewrite
		// of it would go undetected. Better to fail at boot than to ship a
		// parameter the operator believes is protected.
		return nil, fmt.Errorf("project: lifecycle event url must not carry a query string; " +
			"the signature does not cover it")
	}
	return &lifecycleHTTPClient{
		url:    rawURL,
		path:   parsed.EscapedPath(),
		secret: secret,
		clock:  time.Now,
		// One client, reused: a per-request client leaks a connection pool per
		// call and defeats keep-alive against a peer this talks to constantly.
		client: &http.Client{Timeout: timeout},
	}, nil
}

// lifecycleErrorBody is the shape a refusal is read from. The peer states that the
// error code in the body is authoritative and the HTTP status alone is not
// enough, so both are read and the code wins where they disagree.
type lifecycleErrorBody struct {
	Error struct {
		Code string `json:"code"`
	} `json:"error"`
	// Some refusals carry the code at the top level rather than nested. Accepting
	// both costs one field and removes a class of "we saw a 409 but could not say
	// which one" incident.
	Code string `json:"code"`
}

func (b lifecycleErrorBody) code() string {
	if b.Error.Code != "" {
		return b.Error.Code
	}
	return b.Code
}

// Send posts one event.
func (c *lifecycleHTTPClient) Send(ctx context.Context, env lifecycleEventEnvelope) lifecycleDeliveryResult {
	body, err := json.Marshal(env)
	if err != nil {
		// Unmarshalable envelope is our bug and will never marshal on a retry.
		return lifecycleDeliveryResult{Class: lifecycleErrRejected, Detail: "marshal envelope"}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return lifecycleDeliveryResult{Class: lifecycleErrRejected, Detail: "build request"}
	}
	timestamp := strconv.FormatInt(c.clock().Unix(), 10)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "octo-server/project-lifecycle-v1")
	req.Header.Set(octosign.HeaderTimestamp, timestamp)
	// The SAME event id as the body, on every attempt including retries. That is
	// what makes a delivery whose response was lost recognisable as a duplicate
	// rather than applied twice — the case this whole outbox exists to survive.
	req.Header.Set(octosign.HeaderEventID, env.EventID)
	req.Header.Set(lifecycleEventVersionHeader, lifecycleEventVersion)
	// Signed over method + path + timestamp + event id + body. The version header
	// is deliberately NOT in the signature: it selects how the peer parses the
	// request, so it must be readable before verification, and it carries no
	// authority of its own — a tampered version yields a body that fails to parse
	// under it, not a request that means something else.
	req.Header.Set(octosign.HeaderSignature, octosign.Sign(
		c.secret, http.MethodPost, c.path, timestamp, env.EventID, body))

	resp, err := c.client.Do(req)
	if err != nil {
		// Network failures are retryable and their text is NOT recorded: a dial
		// error embeds the host and port, which does not belong in a column an
		// operator pastes into a ticket.
		return lifecycleDeliveryResult{Retryable: true, Class: lifecycleErrNetwork, Detail: "transport failure"}
	}
	defer resp.Body.Close()

	// Read a bounded prefix. The body is only used to extract an error code, and
	// an unbounded read from a peer that is misbehaving is how a delivery worker
	// becomes a memory incident.
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8*1024))
	return classifyLifecycleResponse(resp.StatusCode, raw)
}

// classifyLifecycleResponse maps one response to a delivery outcome.
//
// Split from Send so every branch is testable without a server, and so the
// policy is readable in one place.
func classifyLifecycleResponse(status int, body []byte) lifecycleDeliveryResult {
	var parsed lifecycleErrorBody
	_ = json.Unmarshal(body, &parsed) // best effort; absence is not an error
	code := parsed.code()

	switch {
	case status >= 200 && status < 300:
		// Both "applied" and "ignored" are successes; see lifecycleDeliveryResult.OK.
		return lifecycleDeliveryResult{OK: true, Class: lifecycleErrNone}

	case status == http.StatusConflict:
		// TERMINAL, and the one refusal that must never retry. The peer holds
		// this event id with a different payload, or the project id names a
		// different entity on its side. Retrying cannot change either, and both
		// mean a human has to look: the first is a bug in how events are built,
		// the second is two systems disagreeing about what an id refers to.
		return lifecycleDeliveryResult{Class: lifecycleErrConflict, Detail: detail(status, code)}

	case status == http.StatusUnauthorized, status == http.StatusForbidden:
		// RETRYABLE, which is the non-obvious call. A rotation that reached the
		// peer before it reached this deployment produces exactly this, and it
		// resolves on its own within a deploy. Treating it as terminal would
		// abandon every queued revocation during a routine credential rotation.
		// The alert comes from the error class, not from giving up.
		return lifecycleDeliveryResult{Retryable: true, Class: lifecycleErrAuth, Detail: detail(status, code)}

	case status == http.StatusRequestTimeout, status == http.StatusTooManyRequests:
		return lifecycleDeliveryResult{Retryable: true, Class: lifecycleErrThrottled, Detail: detail(status, code)}

	case status >= 500:
		return lifecycleDeliveryResult{Retryable: true, Class: lifecycleErrServer, Detail: detail(status, code)}

	default:
		// Any other 4xx: a malformed payload, an unknown event type, a route that
		// does not exist. The peer will answer identically forever, so retrying
		// only delays the alert.
		return lifecycleDeliveryResult{Class: lifecycleErrRejected, Detail: detail(status, code)}
	}
}

// detail renders the low-cardinality summary. Status always; peer code only when
// the peer supplied one.
func detail(status int, code string) string {
	if code == "" {
		return fmt.Sprintf("http %d", status)
	}
	return fmt.Sprintf("http %d %s", status, code)
}
