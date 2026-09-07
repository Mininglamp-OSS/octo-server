// Package projectprovision is octo-server's ONLY outbound path to the two
// subsystems that hold a Project's per-project container: octo-fleet (a
// `workspace`) and octo-drive (a shared `drive_space`).
//
// It is a separate package on purpose, and the separation is the enforcement
// mechanism rather than tidiness. The brief's "outbound confinement" rule says no
// request handler may reach fleet or drive, because a handler that does makes a
// user request depend on another service being up. A package boundary makes that
// checkable: modules/project's provisioning worker is the only file in this
// repository allowed to import this package, and a source guard in that module
// asserts it (TestProvisioningClientIsConfinedToTheWorker).
//
// # What this package deliberately does NOT do
//
//   - It does not retry. Retry, backoff and the give-up decision belong to the
//     durable outbox row, not to an in-process loop that a pod restart forgets.
//     Ensure classifies the failure and returns; the worker schedules.
//   - It does not log. Nothing here holds a logger, because the one field it
//     handles that must never reach a log line is the container id (see below),
//     and the cheapest way to guarantee that is to have no log call at all.
//   - It does not read configuration. Targets arrive fully resolved, so a
//     misconfigured URL or a short secret is rejected at process start by the
//     caller rather than on the first delivery attempt.
//
// # The container id is a capability, not an identifier
//
// Until fleet's R2 and drive's R3 narrow their authorization by Project, knowing
// a container id is close enough to holding access to it: fleet's workspace gate
// admits on octo Space membership alone and then materializes the caller as a
// workspace member. So the id must not appear in an error message, a log line,
// or an error `details` map. Every error string this package produces is built
// from a fixed low-cardinality category plus an HTTP status, never from the
// request body — see EnsureError.Error.
//
// It also does not travel in a HEADER. The signature's event-id slot carries
// sha256(container_id), not the id itself, and the reason is asymmetric exposure
// rather than principle: request bodies are almost never logged, while headers
// routinely are — a reverse proxy's custom log format, an APM agent's default
// header capture. Putting a capability where the id would be picked up by
// infrastructure nobody in this repository controls is a measurable widening for
// no gain, since the receiver reads the real id out of the body anyway. The hash
// keeps everything the slot is for: it is stable across replays of the same job,
// and it still binds the signature to one specific resource.
//
// # What the receiver must do
//
// Stated here because it is NOT optional and because neither subsystem has
// implemented its ensure endpoint yet — this is the contract being handed over,
// and the first two clauses have no enforcement on this side at all:
//
//  1. **Verify the signature** over the canonical string
//     (internal/octosign.CanonicalRequest), using the shared per-target
//     secret. An unsigned or wrongly-signed request must be refused.
//  2. **Reject a stale timestamp.** X-Octo-Timestamp is inside the signed string,
//     so it cannot be tampered with, but nothing stops a captured request from
//     being REPLAYED — and this operation is idempotent, so a replay would
//     resurrect a container the subsystem had already reclaimed. A bounded skew
//     window (a few minutes) is what closes that; octosign.Verify
//     deliberately does not check freshness, so the receiver owns it.
//  3. **Treat container_id as the idempotency key**: get-first, create,
//     duplicate-key downgrade. Delivery is at-least-once by construction, so an
//     ensure that creates a second container on the second call is a defect on
//     the receiving side.
//
// And one thing the receiver should NOT expect: `name` is a fixed, low-information
// label, not the project's name (see the field comment on EnsureRequest). A
// receiver that wants a human-readable label should build one from `project_id`,
// which is in every request — octo-server deliberately does not egress
// user-supplied text here.
package projectprovision

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Mininglamp-OSS/octo-server/pkg/octosign"
)

// Header names. Same three headers, same canonical string and same v1 HMAC as
// pkg/octosign, because a receiver that already verifies card
// callbacks can verify these with the code it has.
const (
	HeaderSignature = octosign.HeaderSignature
	HeaderTimestamp = octosign.HeaderTimestamp
	HeaderEventID   = octosign.HeaderEventID
)

const (
	// defaultTimeout bounds one ensure call. Deliberately short: the worker is
	// batched and leased, so a slow target costs a retry rather than a stuck
	// lease, and a long timeout here is what turns one unhealthy target into a
	// worker that makes no progress on the other one.
	defaultTimeout = 10 * time.Second
	// maxResponseBytes bounds what we will read back. The response carries a
	// container id and (for fleet) a slug; anything larger is a misdirected
	// route or a captive-portal style interception, and reading it is pure risk.
	maxResponseBytes = 8 << 10
	// minSecretBytes matches the repository-wide bar for a new internal-route
	// credential (modules/internal_resolve/config.go, modules/notify).
	minSecretBytes = 32
)

// Target is one fully resolved destination. Built by the caller from
// configuration at process start; ValidateTarget is what makes "resolved" mean
// something.
type Target struct {
	// Name is the low-cardinality target label ("fleet" / "drive"). It reaches
	// metrics and log lines, so it must stay an enum and never a URL.
	Name string
	// EnsureURL is the absolute POST endpoint, e.g.
	// https://fleet.internal/api/internal/workspaces/ensure.
	EnsureURL string
	// Secret is the per-target HMAC secret. One secret per target: a single
	// leaked value must not authorize provisioning into both subsystems.
	Secret string
	// Timeout bounds one call; zero means defaultTimeout.
	Timeout time.Duration
}

// EnsureRequest is the wire body. Field set is the union of the two targets'
// contracts (brief D2); IssuePrefix is fleet-only and omitted when empty.
//
// project_id travels alongside container_id and is NOT the id: the receiver needs
// it to answer "which project is this container for" when it later reclaims
// (D9), and must not derive an id from it.
//
// Name is a fixed, low-information label — NOT the project's name. octo-server holds
// the name authoritatively and never syncs it outbound (brief D3), and a project name
// is user-supplied free text, so egressing it would make provisioning a content path
// with its own escaping and disclosure questions. The consequence for the receiver is
// real and worth stating rather than discovering: every container arrives with the
// SAME name, so a receiver that shows this string in its own UI will show one label
// for every project. Build a display label from project_id instead, or read the real
// name from GET /v1/projects/:project_id, which is what D3 already tells fleet to do
// for `context`.
type EnsureRequest struct {
	ContainerID string `json:"container_id"`
	ProjectID   string `json:"project_id"`
	OctoSpaceID string `json:"octo_space_id"`
	Name        string `json:"name"`
	IssuePrefix string `json:"issue_prefix,omitempty"`
}

// EnsureResponse is what a target returns. Slug is fleet-only.
type EnsureResponse struct {
	ContainerID string `json:"container_id"`
	Slug        string `json:"slug,omitempty"`
}

// EnsureError carries a low-cardinality category so the worker can label a
// metric and write a bounded last_error without ever touching the request body.
type EnsureError struct {
	Category  string
	Status    int
	retryable bool
	cause     error
}

// Error is deliberately built from the category and the status only. It must stay
// that way: this string lands in octo_project_provisioning.last_error, and the
// container id is a capability (see the package comment).
func (e *EnsureError) Error() string {
	if e.Status > 0 {
		return fmt.Sprintf("projectprovision: %s (status %d)", e.Category, e.Status)
	}
	return "projectprovision: " + e.Category
}

func (e *EnsureError) Unwrap() error { return e.cause }

// Retryable reports whether another attempt could plausibly succeed. A 4xx other
// than 408 / 429 is NOT retryable in this sense — but the worker still retries it
// under its own bounded budget, because "the target has not deployed its ensure
// endpoint yet" presents as 404 and is expected to become 200 without anything on
// this side changing. What this flag buys is a distinct metric label, so a
// permanent contract break is visible on the first attempt instead of at
// attempt-exhaustion an hour later.
func Retryable(err error) bool {
	var ensureErr *EnsureError
	return errors.As(err, &ensureErr) && ensureErr.retryable
}

// Category extracts the low-cardinality failure label, or "" for a non-ensure
// error. Metric label values come from here so they can never be a free-form
// message.
func Category(err error) string {
	var ensureErr *EnsureError
	if errors.As(err, &ensureErr) {
		return ensureErr.Category
	}
	return ""
}

// ValidateTarget rejects a destination that must not be used, at process start.
//
// Scheme is restricted to http/https and userinfo is refused: an ensure URL is
// operator-supplied configuration, and a `https://user:pass@host/` form would put
// a credential into every log line that prints the URL. A path is required
// because a bare origin almost always means a truncated env value.
//
// What it deliberately does NOT do is reject private, loopback or link-local hosts
// (169.254.169.254 and friends). Recorded as an ACCEPTED posture rather than left as
// an omission a reader has to guess about: the destination is a deploy-time operator
// value, not user input, so there is no untrusted party choosing it, and both real
// targets are in-cluster services on private addresses — a private-range denylist
// would reject every legitimate configuration. This is the same posture as
// pkg/octosign, whose route URLs are operator-registered for the same
// reason. It would have to change if an ensure URL ever became something a tenant
// could influence.
func ValidateTarget(t Target) error {
	if strings.TrimSpace(t.Name) == "" {
		return errors.New("projectprovision: target name required")
	}
	if len(t.Secret) < minSecretBytes {
		// No secret value in the message, and no length either — an error string
		// that reports the observed length is a (small) oracle in a log.
		return fmt.Errorf("projectprovision: %s secret must be at least %d bytes", t.Name, minSecretBytes)
	}
	parsed, err := url.Parse(t.EnsureURL)
	if err != nil {
		return fmt.Errorf("projectprovision: %s ensure url is not parseable", t.Name)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("projectprovision: %s ensure url must be http or https", t.Name)
	}
	if parsed.Host == "" {
		return fmt.Errorf("projectprovision: %s ensure url has no host", t.Name)
	}
	if parsed.User != nil {
		return fmt.Errorf("projectprovision: %s ensure url must not carry userinfo", t.Name)
	}
	if parsed.EscapedPath() == "" || parsed.EscapedPath() == "/" {
		return fmt.Errorf("projectprovision: %s ensure url must include the ensure path", t.Name)
	}
	return nil
}

// Client is the outbound HTTP client. One per process.
type Client struct {
	client *http.Client
	clock  func() time.Time
}

// NewClient builds the client. transport and clock are injectable so tests drive
// it without a network, which is the only reason they are parameters.
//
// Proxy is cleared and redirects are refused, for the same two reasons
// pkg/octosign/http.go clears them: the destinations are exact,
// operator-registered URLs, so honouring HTTP(S)_PROXY would let a
// deployment-level setting redirect provisioning traffic, and following a
// redirect would re-send the signed body to a host the signature was not
// computed for.
func NewClient(transport http.RoundTripper, clock func() time.Time) *Client {
	if transport == nil {
		base := http.DefaultTransport.(*http.Transport).Clone()
		base.Proxy = nil
		transport = base
	}
	if clock == nil {
		clock = time.Now
	}
	return &Client{
		client: &http.Client{
			Transport: transport,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		clock: clock,
	}
}

// Ensure calls one target's ensure endpoint exactly once.
//
// The receiver's contract is get-first / create / duplicate-key downgrade, keyed
// on the supplied container_id (brief P-1), so this is safe to replay: at-least-once
// delivery converges instead of manufacturing a second container. The receiver owes
// two more things that this side cannot enforce — see "What the receiver must do".
//
// The signature's event-id slot carries containerEventID(container_id), i.e. the
// hash, not the id. See the package comment for why a capability does not go in a
// header.
func (c *Client) Ensure(ctx context.Context, target Target, req EnsureRequest) (EnsureResponse, error) {
	if ctx == nil {
		return EnsureResponse{}, &EnsureError{Category: "invalid_request"}
	}
	if req.ContainerID == "" || req.ProjectID == "" || req.OctoSpaceID == "" {
		return EnsureResponse{}, &EnsureError{Category: "invalid_request"}
	}
	if err := ValidateTarget(target); err != nil {
		return EnsureResponse{}, &EnsureError{Category: "invalid_target", cause: err}
	}
	body, err := json.Marshal(req)
	if err != nil {
		return EnsureResponse{}, &EnsureError{Category: "encode_failed", cause: err}
	}
	parsed, err := url.Parse(target.EnsureURL)
	if err != nil {
		return EnsureResponse{}, &EnsureError{Category: "invalid_target", cause: err}
	}
	path := parsed.EscapedPath()
	timeout := target.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	timestamp := strconv.FormatInt(c.clock().Unix(), 10)
	eventID := containerEventID(req.ContainerID)

	requestCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	httpReq, err := http.NewRequestWithContext(requestCtx, http.MethodPost, target.EnsureURL, bytes.NewReader(body))
	if err != nil {
		return EnsureResponse{}, &EnsureError{Category: "request_failed", cause: err}
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("User-Agent", "octo-server/project-provisioning-v1")
	httpReq.Header.Set(HeaderTimestamp, timestamp)
	httpReq.Header.Set(HeaderEventID, eventID)
	httpReq.Header.Set(HeaderSignature, octosign.Sign(
		target.Secret, http.MethodPost, path, timestamp, eventID, body))

	response, err := c.client.Do(httpReq)
	if err != nil {
		// Retryable only when the deadline we imposed (or the caller's) has not
		// already fired: a cancelled parent context means the worker is shutting
		// down, and reporting that as retryable would burn an attempt on it.
		return EnsureResponse{}, &EnsureError{
			Category:  "transport_failed",
			retryable: ctx.Err() == nil,
			cause:     err,
		}
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		// Drain a bounded prefix so the connection can be reused, and discard it:
		// an error body from an unnarrowed target is not something we want in a log.
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxResponseBytes))
		return EnsureResponse{}, &EnsureError{
			Category:  statusCategory(response.StatusCode),
			Status:    response.StatusCode,
			retryable: statusRetryable(response.StatusCode),
		}
	}
	var out EnsureResponse
	decoder := json.NewDecoder(io.LimitReader(response.Body, maxResponseBytes))
	if err := decoder.Decode(&out); err != nil {
		return EnsureResponse{}, &EnsureError{Category: "invalid_response", retryable: true, cause: err}
	}
	if out.ContainerID != req.ContainerID {
		// Fail loudly and permanently. A target that answers with a different id
		// means our mapping row now points at a container nobody owns, and
		// retrying cannot repair that — it needs a human to look at which side
		// generated the id.
		return EnsureResponse{}, &EnsureError{Category: "container_id_mismatch", Status: response.StatusCode}
	}
	return out, nil
}

// containerEventID derives the wire event id from a container id.
//
// sha256 hex, and exported behaviour rather than an implementation detail: the
// receiver has to compute the same value to verify the signature, so this is part
// of the contract. It is deterministic, so a replay of the same job produces the
// same canonical string; and it is one-way, so the value sitting in a proxy log
// is not the capability.
func containerEventID(containerID string) string {
	sum := sha256.Sum256([]byte(containerID))
	return hex.EncodeToString(sum[:])
}

// statusCategory maps a status to a fixed metric label.
func statusCategory(status int) string {
	switch {
	case status >= 500:
		return "target_5xx"
	case status == http.StatusTooManyRequests:
		return "target_rate_limited"
	case status == http.StatusRequestTimeout:
		return "target_timeout"
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return "target_rejected_credential"
	case status == http.StatusNotFound:
		return "target_no_ensure_endpoint"
	case status >= 300 && status < 400:
		return "target_redirect_refused"
	default:
		return "target_4xx"
	}
}

// statusRetryable reports whether the status alone justifies another attempt.
// See Retryable for why the worker retries some non-retryable statuses anyway.
func statusRetryable(status int) bool {
	return status >= 500 ||
		status == http.StatusRequestTimeout ||
		status == http.StatusTooManyRequests
}
