// Package projectprovision is octo-server's only outbound path to the two
// optional Project provisioning subsystems: octo-fleet and octo-drive.
//
// No request handler may reach either subsystem. The Project worker is the
// caller of Ensure and CreateDriveSpace; config_provisioning.go imports this
// package only to resolve and validate targets at boot. Keeping that boundary
// in one package makes the no-egress-from-request-path rule checkable.
//
// This package deliberately does not retry or log. Retry, backoff and the
// give-up decision belong to the durable outbox row, and callers receive
// targets that have already been resolved from configuration.
//
// Fleet's Ensure protocol uses an opaque container_id as its idempotency key
// and authenticates with the per-target HMAC secret. That id is never included
// in errors or logs.
//
// Drive's internal create protocol is intentionally separate: it authenticates
// with X-Internal-Token and sends the current Project name, octo_space_id,
// super_admin_uid and project_id. It does not send Fleet's container_id and
// does not read or persist Drive's remote space id. A same-project 409 is
// accepted only when Drive's conflict envelope names the requested project_id
// exactly; all other statuses remain failures for the worker to retry or
// abandon according to its category.
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
	"unicode"
	"unicode/utf8"

	"github.com/Mininglamp-OSS/octo-server/pkg/octosign"
)

// Fleet HMAC header names. They use the same canonical string and v1 HMAC as
// pkg/octosign, because a receiver that already verifies card
// callbacks can verify these with the code it has.
const (
	HeaderSignature = octosign.HeaderSignature
	HeaderTimestamp = octosign.HeaderTimestamp
	HeaderEventID   = octosign.HeaderEventID
)

// AuthMode selects the wire authentication contract for a target.
//
// AuthHMAC is the zero value for backwards compatibility with the fleet
// ensure client. AuthInternalToken is used by Drive's internal create route;
// the two credential fields are mutually exclusive and ValidateTarget enforces
// that separation.
type AuthMode uint8

const (
	AuthHMAC AuthMode = iota
	AuthInternalToken
)

// HeaderInternalToken carries an internal service token to a target that uses
// token authentication instead of the Fleet HMAC contract.
const HeaderInternalToken = "X-Internal-Token"

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
	// metrics and log lines, so it must stay an enum and never be a URL.
	Name string
	// EnsureURL is the absolute POST endpoint. Fleet uses its HMAC ensure route;
	// Drive uses /v1/internal/drive/spaces with AuthInternalToken.
	EnsureURL string
	// Auth selects the target's wire authentication. The zero value is HMAC.
	Auth AuthMode
	// Secret is the per-target Fleet HMAC secret.
	Secret string
	// InternalToken is the Drive internal-route token. It must not be used as
	// an HMAC secret or sent to the Fleet route.
	InternalToken string
	// Timeout bounds one call; zero means defaultTimeout.
	Timeout time.Duration
}

// EnsureRequest is the Fleet wire body. Drive has a separate body type because
// its project mapping is keyed by project_id and does not use container_id.
type EnsureRequest struct {
	ContainerID string `json:"container_id"`
	ProjectID   string `json:"project_id"`
	OctoSpaceID string `json:"octo_space_id"`
	Name        string `json:"name"`
	IssuePrefix string `json:"issue_prefix,omitempty"`
}

// DriveRequest is the body accepted by Drive's internal create route.
//
// ProjectID is the stable project-to-space mapping key. ContainerID is
// deliberately absent: Drive owns its space id and this client never reads or
// persists it.
type DriveRequest struct {
	Name          string `json:"name"`
	OctoSpaceID   string `json:"octo_space_id"`
	SuperAdminUID string `json:"super_admin_uid"`
	ProjectID     string `json:"project_id"`
}

// EnsureResponse is what Fleet returns. Drive's remote space id is intentionally
// not represented because the Project outbox is keyed by project_id.
type EnsureResponse struct {
	ContainerID string `json:"container_id"`
	Slug        string `json:"slug,omitempty"`
}

// DriveSpaceResponse reports the only Drive result the caller needs. A 201 is
// a new remote space; an exact same-project 409 is an idempotent duplicate.
// The remote Drive id is intentionally ignored.
type DriveSpaceResponse struct {
	Duplicate bool
}

// EnsureError carries a low-cardinality category so the worker can label a
// metric and write a bounded last_error without ever touching the request body.
type EnsureError struct {
	Category string
	Status   int
	// Detail is a bounded, container-id-free reason. It is either derived from a
	// transport inner error or a fixed response-classification constant; it is
	// never derived from a request or a response body.
	//
	// It exists because category-plus-status is empty for the failure an operator hits
	// first. A target that is down produced `last_error = "transport_failed:
	// projectprovision: transport_failed"` — the outcome label twice — while the actual
	// reason (connection refused vs DNS vs TLS vs deadline) sat in `cause`, reachable
	// only through Unwrap, which nothing calls. That field is what the runbook sends a
	// human to read, the sweep was deliberately changed to APPEND to it because it is the
	// only durable per-row evidence, and this package has no logger by design — so for an
	// OOM-killed pod there is no log line to fall back on either.
	//
	// Why a separate field rather than folding `cause` into Error(): `cause` is whatever
	// the standard library produced, and at two of the construction sites that is a
	// function of OUR request — encode_failed wraps a json.Marshal error over an
	// EnsureRequest, which carries the container id. json.Marshal of an all-string struct
	// cannot realistically fail, but "cannot realistically" is not the bar for a value the
	// package comment calls a capability. Transport-derived Detail comes only from the
	// transport error's INNER error, which describes the network and structurally cannot
	// contain the request. Response classifications use fixed constants rather than parsing
	// errors, because a parser error can quote response-body bytes. The container-id-freedom
	// of last_error stays a property of construction, not an argument about stdlib formatting.
	Detail string
	cause  error
}

// Error is built from the category, the status and the bounded transport Detail — never
// from the request. It must stay that way: this string lands in
// octo_project_provisioning.last_error, and the container id is a capability (see the
// package comment and the Detail field).
func (e *EnsureError) Error() string {
	msg := "projectprovision: " + e.Category
	if e.Status > 0 {
		msg = fmt.Sprintf("projectprovision: %s (status %d)", e.Category, e.Status)
	}
	if e.Detail != "" {
		msg += ": " + e.Detail
	}
	return msg
}

func (e *EnsureError) Unwrap() error { return e.cause }

// There is deliberately NO Retryable predicate here, and the absence is a decision.
//
// An earlier version exported one, defined as "the status alone justifies another
// attempt", and nothing consumed it — the worker classified on Category and the flag was
// computed and discarded. The abstraction was wrong rather than merely unused: the worker
// deliberately RETRIES two statuses that predicate calls non-retryable, because 404 is
// what "the target has not deployed its ensure endpoint yet" looks like and 401 is what a
// rotated secret looks like, and both become 200 after a deployment on the other side
// with nothing changing here. Meanwhile the failures that genuinely must not be retried
// are identified by their CATEGORY, not by an HTTP status (container_id_mismatch,
// invalid_request, encode_failed).
//
// So the retry decision lives entirely in the worker, keyed on Category — see
// isPermanentProvisioningOutcome in modules/project/provisioning_worker.go. Keeping a
// predicate no caller should use is worse than not having one.

// Summary returns the container-id-free failure text WITHOUT the category.
//
// The caller already labels the row and the metric with its own outcome string, which is
// derived from Category — so including the category here produced `last_error =
// "transport_failed: projectprovision: transport_failed"`, the same label twice, in a
// 255-byte column that a human is sent to read and that the sweep appends to. This returns
// only the part the outcome does not already say:
//
//	transport_failed -> "dial tcp 10.0.0.4:8080: connect: connection refused"
//	target_5xx       -> "status 500"
//	invalid_request  -> ""            (nothing to add; the category IS the reason)
//
// A non-EnsureError falls back to its own message, which is how a panic or a DB error keeps
// its text. Same container-id-freedom guarantee as Error(): the only request-derived field
// on EnsureError is `cause`, and this never reads it.
func Summary(err error) string {
	var ensureErr *EnsureError
	if !errors.As(err, &ensureErr) {
		if err == nil {
			return ""
		}
		return err.Error()
	}
	if ensureErr.Detail != "" {
		return ensureErr.Detail
	}
	if ensureErr.Status > 0 {
		return fmt.Sprintf("status %d", ensureErr.Status)
	}
	return ""
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
// Two things it deliberately does NOT do, both ACCEPTED postures rather than omissions
// a reader has to guess about:
//
//   - It does not reject private, loopback or link-local hosts (169.254.169.254 and
//     friends). The destination is a deploy-time operator value, not user input, so
//     there is no untrusted party choosing it, and both real targets are in-cluster
//     services on private addresses — a private-range denylist would reject every
//     legitimate configuration. Same posture as internal/cardactiondispatch, whose
//     route URLs are operator-registered for the same reason. It would have to change
//     if an ensure URL ever became something a tenant could influence.
//   - It permits `http://`. Reviewed and accepted deliberately (2026-09-07): both
//     targets are in-cluster, and requiring TLS would block the ordinary in-cluster
//     deployment. The cost is stated rather than hidden: on a plaintext link, an
//     on-path observer inside the cluster sees the container id — which is a capability
//     until R2/R3 land — and captures a replayable signed request. That is precisely
//     why the receiver's timestamp-freshness clause below is a MUST and why the
//     conformance vectors exist: with TLS declined, the receiver-side check is the layer
//     that has to actually work.
func ValidateTarget(t Target) error {
	if strings.TrimSpace(t.Name) == "" {
		return errors.New("projectprovision: target name required")
	}
	switch t.Auth {
	case AuthHMAC:
		if t.InternalToken != "" {
			return fmt.Errorf("projectprovision: %s HMAC target must not set an internal token", t.Name)
		}
		if err := validateCredential(t.Name, "secret", t.Secret); err != nil {
			return err
		}
	case AuthInternalToken:
		if t.Secret != "" {
			return fmt.Errorf("projectprovision: %s internal-token target must not set an HMAC secret", t.Name)
		}
		if err := validateCredential(t.Name, "internal token", t.InternalToken); err != nil {
			return err
		}
	default:
		return fmt.Errorf("projectprovision: %s has an unsupported authentication mode", t.Name)
	}
	// The same rule as the secret above, and it belongs here for the same reason
	// the sibling validator applies it (internal/cardactiondispatch/registry.go
	// refuses an untrimmed raw value before parsing): "reject, do not trim" applied
	// to one of the two configured values is a principle with a hole in it.
	// loadProvisioningConfig deliberately passes the raw env value through, so a
	// whitespace-wrapped URL is rejected here, the target is dropped, and boot
	// reports it through Problems and Misconfigured. Direct callers get the same
	// refusal because Ensure revalidates the Target before constructing a request.
	// Before this check, a trailing space could survive as %20 in EscapedPath and
	// reach the peer; it can no longer take that request path.
	if strings.TrimSpace(t.EnsureURL) != t.EnsureURL || t.EnsureURL == "" {
		return fmt.Errorf("projectprovision: %s ensure url must not be empty or carry "+
			"leading or trailing whitespace", t.Name)
	}
	parsed, err := url.Parse(t.EnsureURL)
	if err != nil {
		return fmt.Errorf("projectprovision: %s ensure url is not parseable", t.Name)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("projectprovision: %s ensure url must be http or https", t.Name)
	}
	// Hostname(), not Host: Host keeps the ":port", so "http://:8080/ensure" would pass a
	// Host != "" check while having no host at all. Same reasoning as
	// cardactiondispatch.validateCallbackURL.
	if parsed.Hostname() == "" || parsed.Opaque != "" {
		return fmt.Errorf("projectprovision: %s ensure url has no host", t.Name)
	}
	if parsed.User != nil {
		return fmt.Errorf("projectprovision: %s ensure url must not carry userinfo", t.Name)
	}
	if parsed.EscapedPath() == "" || parsed.EscapedPath() == "/" {
		return fmt.Errorf("projectprovision: %s ensure url must include the ensure path", t.Name)
	}
	// A query string would travel OUTSIDE the MAC, so it is refused rather than
	// tolerated. CanonicalRequest signs the PATH only, while the request is issued
	// against the full URL — so `?tenant=A` is unauthenticated and an on-path rewrite to
	// `?tenant=B` still verifies. cardactiondispatch rejects the same thing for the same
	// signing scheme; reusing the format without reusing this restriction is what left
	// the gap. ForceQuery covers the bare "trailing ?" form, where RawQuery is empty but
	// the separator survives onto the wire.
	if parsed.RawQuery != "" || parsed.ForceQuery {
		return fmt.Errorf("projectprovision: %s ensure url must not contain a query", t.Name)
	}
	// A fragment is never sent, so one in configuration means the value was pasted from
	// somewhere it did not belong.
	//
	// Checked on the RAW string, not on parsed.Fragment. url.Parse leaves Fragment
	// empty for a bare trailing "#", so "https://host/ensure#" passes a
	// parsed.Fragment check while still carrying the separator. cardactiondispatch
	// documents this exact case and uses ContainsRune for it; this package claimed
	// alignment with that validator and had the weaker check — the same defect its
	// sibling had already found and fixed.
	if strings.ContainsRune(t.EnsureURL, '#') {
		return fmt.Errorf("projectprovision: %s ensure url must not contain a fragment", t.Name)
	}
	return nil
}

// validateCredential applies the common no-trimming, minimum-length and
// published-vector checks to either supported credential kind.
func validateCredential(name, label, value string) error {
	if strings.TrimSpace(value) != value {
		return fmt.Errorf("projectprovision: %s %s has leading or trailing whitespace; "+
			"a file-mounted credential usually needs its trailing newline removed", name, label)
	}
	if len(value) < minSecretBytes {
		return fmt.Errorf("projectprovision: %s %s must be at least %d bytes", name, label, minSecretBytes)
	}
	if isPublishedConformanceSecret(value) {
		return fmt.Errorf("projectprovision: %s %s is a published conformance vector secret; generate a real one",
			name, label)
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
// internal/cardactiondispatch/http.go clears them: the destinations are exact,
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
	if target.Auth != AuthHMAC {
		return EnsureResponse{}, &EnsureError{Category: "invalid_target"}
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
		return EnsureResponse{}, &EnsureError{
			Category: "request_failed", Detail: transportDetail(err), cause: err,
		}
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("User-Agent", "octo-server/project-provisioning-v1")
	httpReq.Header.Set(HeaderTimestamp, timestamp)
	httpReq.Header.Set(HeaderEventID, eventID)
	httpReq.Header.Set(HeaderSignature, octosign.Sign(
		target.Secret, http.MethodPost, path, timestamp, eventID, body))

	response, err := c.client.Do(httpReq)
	if err != nil {
		// One category for every way the call did not produce a response — connection
		// refused, DNS failure, TLS handshake, the per-call deadline — because the retry
		// decision is the same for all of them and the metric label must stay a closed
		// enum. Which one it was goes in Detail, where an operator can read it.
		//
		// The reachable cancellation is this client's own deadline, NOT a parent context:
		// the caller passes context.Background() and the module registers no Stop hook, so
		// SIGTERM does not interrupt an in-flight call at all. What actually happens on
		// shutdown is that the process is killed mid-call, the lease expires, and the
		// sweep recovers the row — attempts was already incremented at claim, and
		// container_id is the receiver's idempotency key, so the retry cannot double-create.
		return EnsureResponse{}, &EnsureError{
			Category: "transport_failed", Detail: transportDetail(err), cause: err,
		}
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		// STRICTLY 200, not any 2xx, because the published contract
		// (.octospec/tasks/project-p2-subsystem-integration/brief.md, "Target endpoint contracts")
		// specifies a SYNCHRONOUS 200 carrying container_id, and the difference
		// between 200 and 202 is exactly the thing this call has to know: 202 means
		// the peer accepted the request and will act on it later, so treating it as
		// success writes status=ready — "we successfully created the container" —
		// against a container that may not exist yet. Every later decision that
		// reads ready would be reading a promise as a fact.
		//
		// A 2xx that is not 200 is therefore a contract violation on the peer's
		// side, not a success, and it retries: the peer may be mid-rollout, and
		// unlike a 4xx there is nothing here that says it will answer the same way
		// forever. statusCategory maps 2xx to the generic bucket, so the category
		// is set explicitly for the one an operator needs to recognise.
		//
		// Drain a bounded prefix so the connection can be reused, and discard it:
		// an error body from an unnarrowed target is not something we want in a log.
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxResponseBytes))
		if response.StatusCode > http.StatusOK && response.StatusCode < http.StatusMultipleChoices {
			return EnsureResponse{}, &EnsureError{
				Category: "invalid_response",
				Status:   response.StatusCode,
				Detail:   "contract requires a synchronous 200",
			}
		}
		return EnsureResponse{}, &EnsureError{
			Category: statusCategory(response.StatusCode),
			Status:   response.StatusCode,
		}
	}
	var out EnsureResponse
	decoder := json.NewDecoder(io.LimitReader(response.Body, maxResponseBytes))
	if err := decoder.Decode(&out); err != nil {
		// Status is necessarily 200 here: the gate above returned every other status.
		// Keep the durable reason a fixed constant, not err.Error(), because a JSON
		// parsing error can quote response-body bytes including the container id.
		return EnsureResponse{}, &EnsureError{
			Category: "invalid_response",
			Detail:   "response body was not valid JSON",
			cause:    err,
		}
	}
	if strings.TrimSpace(out.ContainerID) == "" {
		// A MISSING id is a malformed response, not evidence the peer owns a
		// different container — the two must not share a category, because one is
		// retryable and the other is terminal on the first attempt.
		//
		// This is the shape a peer serves while its ensure endpoint is still being
		// rolled out. The REACHABLE set is `{}`, `{"container_id":""}`,
		// whitespace-only container_id values, and `null` — an empty or whitespace-only
		// BODY does NOT arrive here, because Decode returns io.EOF and the branch above
		// catches that different error shape.
		//
		// Classifying any of them as a mismatch abandons the row immediately, and
		// abandoned has no automatic re-drive — so a transient state on the other
		// side would need a human to requeue, at exactly the moment the first
		// target is being enabled.
		//
		// Detail rather than status alone: Summary() falls back to "status %d" when
		// Detail is empty, so this row used to write `invalid_response: status 200`
		// into last_error — a failure category paired with a success status, in the
		// 255-byte column the runbook sends a human to read. The constant carries no
		// request data, which is what that field's invariant requires.
		return EnsureResponse{}, &EnsureError{
			Category: "invalid_response",
			Status:   response.StatusCode,
			Detail:   "response carried no container_id",
		}
	}
	if out.ContainerID != req.ContainerID {
		// A DIFFERENT id is terminal. Our mapping row now points at a container
		// nobody owns, and retrying cannot repair that — it needs a human to look
		// at which side generated the id.
		return EnsureResponse{}, &EnsureError{Category: "container_id_mismatch", Status: response.StatusCode}
	}
	return out, nil
}

// CreateDriveSpace calls Drive's internal create route exactly once.
//
// Drive owns the remote space id, so this method sends only the project mapping
// fields and returns no remote identifier. A 409 is idempotent only when the
// response envelope names the same project_id that was requested; every other
// 409 remains a target failure.
func (c *Client) CreateDriveSpace(ctx context.Context, target Target, req DriveRequest) (DriveSpaceResponse, error) {
	if ctx == nil || !validDriveRequest(req) {
		return DriveSpaceResponse{}, &EnsureError{Category: "invalid_request"}
	}
	if target.Auth != AuthInternalToken {
		return DriveSpaceResponse{}, &EnsureError{Category: "invalid_target"}
	}
	if err := ValidateTarget(target); err != nil {
		return DriveSpaceResponse{}, &EnsureError{Category: "invalid_target", cause: err}
	}
	body, err := json.Marshal(req)
	if err != nil {
		return DriveSpaceResponse{}, &EnsureError{Category: "encode_failed", cause: err}
	}
	timeout := target.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	requestCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	httpReq, err := http.NewRequestWithContext(requestCtx, http.MethodPost, target.EnsureURL, bytes.NewReader(body))
	if err != nil {
		return DriveSpaceResponse{}, &EnsureError{
			Category: "request_failed", Detail: transportDetail(err), cause: err,
		}
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("User-Agent", "octo-server/project-provisioning-v1")
	httpReq.Header.Set(HeaderInternalToken, target.InternalToken)

	response, err := c.client.Do(httpReq)
	if err != nil {
		return DriveSpaceResponse{}, &EnsureError{
			Category: "transport_failed", Detail: transportDetail(err), cause: err,
		}
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusCreated {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxResponseBytes))
		return DriveSpaceResponse{}, nil
	}

	responseBody, readable := readBoundedResponse(response.Body)
	duplicate := false
	if response.StatusCode == http.StatusConflict && readable {
		duplicate = isExactDriveDuplicate(responseBody, req.ProjectID)
		if duplicate {
			return DriveSpaceResponse{Duplicate: true}, nil
		}
	}
	if response.StatusCode > http.StatusOK && response.StatusCode < http.StatusMultipleChoices {
		return DriveSpaceResponse{}, &EnsureError{
			Category: "invalid_response",
			Status:   response.StatusCode,
			Detail:   "contract requires a synchronous 201",
		}
	}
	return DriveSpaceResponse{}, &EnsureError{
		Category: statusCategory(response.StatusCode),
		Status:   response.StatusCode,
	}
}

func validDriveRequest(req DriveRequest) bool {
	if strings.TrimSpace(req.Name) == "" || len(req.Name) > 64 {
		return false
	}
	if strings.TrimSpace(req.OctoSpaceID) == "" || strings.TrimSpace(req.SuperAdminUID) == "" {
		return false
	}
	return strings.TrimSpace(req.ProjectID) != "" && len(req.ProjectID) <= 64
}

func readBoundedResponse(body io.Reader) ([]byte, bool) {
	data, err := io.ReadAll(io.LimitReader(body, maxResponseBytes+1))
	return data, err == nil && len(data) <= maxResponseBytes
}

func isExactDriveDuplicate(body []byte, projectID string) bool {
	var envelope struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	if err := decoder.Decode(&envelope); err != nil {
		return false
	}
	var extra interface{}
	if err := decoder.Decode(&extra); err != io.EOF {
		return false
	}
	return envelope.Error == "conflict" &&
		envelope.Message == fmt.Sprintf("workspace_id %q already bound to a space", projectID)
}

// maxDetailBytes bounds a transport Detail.
//
// last_error is VARCHAR(255) and it is SHARED: the worker prefixes an outcome label and
// the sweep appends its own marker, and the truncation on the way in keeps the OLDEST
// text. So an unbounded detail would not just be clipped — it would push the sweep's
// marker out of the column. Small enough that several attempts still fit.
const maxDetailBytes = 96

// transportDetail renders the reason a call produced no response, drawn ONLY from the
// transport error.
//
// Two properties, both load-bearing:
//
//   - It never reads the request. A *url.Error's own message embeds the method and the full
//     URL; this returns its INNER error instead, which describes the network. Nothing we
//     sent — least of all the container id — can reach the returned string. (The target's
//     identity is already its own column, so the URL adds nothing an operator needs here.)
//   - The two deadline/cancel shapes get FIXED strings rather than the stdlib's, so the
//     common case reads the same in every row and cannot vary with Go's wording.
//
// Control characters are stripped: this value lands in a log line and a DB column, and a
// newline from a hostile-ish transport error should not be able to forge a second log field.
func transportDetail(err error) string {
	if err == nil {
		return ""
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		// This is the client's own per-call timeout, bounded to a quarter of the lease at
		// config load. Named explicitly because it is the one an operator will see when a
		// target is slow rather than down, and the two need different actions.
		return "deadline exceeded (per-call timeout)"
	case errors.Is(err, context.Canceled):
		return "context canceled"
	}
	inner := err
	var urlErr *url.Error
	if errors.As(err, &urlErr) && urlErr.Err != nil {
		inner = urlErr.Err
	}
	return boundedSingleLine(inner.Error(), maxDetailBytes)
}

// boundedSingleLine collapses control characters and truncates on a rune boundary.
//
// Truncating a UTF-8 string by bytes can split a rune and leave an invalid sequence, which
// MySQL in strict mode rejects on a utf8mb4 column — that would make the UPDATE fail and
// leave the row pending and unsweepable, which is the zombie this module already closed
// once. Cutting at a rune boundary is what keeps that closed.
func boundedSingleLine(s string, max int) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r == '\n' || r == '\r' || r == '\t' {
			r = ' '
		}
		if unicode.IsControl(r) {
			continue
		}
		if b.Len()+utf8.RuneLen(r) > max {
			break
		}
		b.WriteRune(r)
	}
	return strings.TrimSpace(b.String())
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
