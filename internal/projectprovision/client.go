// Package projectprovision is octo-server's ONLY outbound path to the two
// subsystems that hold a Project's per-project container: octo-fleet (a
// `workspace`) and octo-drive (a shared `drive_space`).
//
// It is a separate package on purpose, and the separation is the enforcement
// mechanism rather than tidiness. The brief's "outbound confinement" rule says no
// request handler may reach fleet or drive, because a handler that does makes a
// user request depend on another service being up. A package boundary makes that
// checkable: no REQUEST HANDLER in this repository may import this package, and
// modules/project's provisioning worker is the only file that calls Ensure. A source
// guard in that module asserts both (TestProvisioningClientIsConfinedToTheWorker).
//
// Not "only one file may import it" — modules/project/config_provisioning.go imports it too,
// for Target and ValidateTarget at boot, which is intended. The earlier wording claimed a
// property neither the design nor the guard has.
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
//     (pkg/octosign.CanonicalRequest), using the shared per-target
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
	"unicode"
	"unicode/utf8"

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
	// Surrounding whitespace is REFUSED, not trimmed, and the difference is the
	// whole point. A secret mounted from a file carries a trailing newline; trimming
	// it would let a subtly wrong mount work, so nobody learns the mount is wrong
	// until the day something stops trimming. Refusing surfaces it at boot, in the
	// one place an operator is already reading — while trimming silently would have
	// been indistinguishable from a correct deployment.
	//
	// The alternative failure, if this check is absent, is not a clean error either:
	// every request signs with a value the peer rejects, so the whole retry budget
	// burns as 401s that look exactly like a rotated secret, and the row lands in
	// abandoned, which has no automatic re-drive.
	//
	// What refusing COSTS, stated because this comment is what the next person
	// consults: the target is dropped from cfg.Targets, and if it was the only one
	// then Enabled() goes false and the claim, sweep and purge timers are never
	// mounted — so rows already `pending` stop being claimed until a valid target
	// is configured again. The census keeps the backlog visible throughout. That is
	// still the better trade than 23 minutes of ambiguous 401s, but it is a
	// different one from "the process refuses to start", which this does not do.
	//
	// No value in the message, on the same principle as the length check below.
	if strings.TrimSpace(t.Secret) != t.Secret {
		return fmt.Errorf("projectprovision: %s secret has leading or trailing whitespace; "+
			"a file-mounted secret usually needs its trailing newline removed", t.Name)
	}
	if len(t.Secret) < minSecretBytes {
		// No secret value in the message, and no length either — an error string
		// that reports the observed length is a (small) oracle in a log.
		return fmt.Errorf("projectprovision: %s secret must be at least %d bytes", t.Name, minSecretBytes)
	}
	if isPublishedConformanceSecret(t.Secret) {
		// The conformance vectors ship real, working secrets in a source file of a public
		// repository, and both are long enough to clear the floor above — so an operator who
		// copies one into OCTO_PROJECT_PROVISION_*_SECRET to "try it out" boots clean holding
		// a published HMAC key. conformanceTamperedBody is literally the forgery that key
		// authorises: a valid signature over an attacker-chosen project_id. The MAC is the
		// load-bearing layer here precisely because ValidateTarget declines to require TLS.
		//
		// Compared with subtle.ConstantTimeCompare for consistency with how the rest of this
		// package treats secret material, not because a timing signal would matter: the
		// values being compared against are already public.
		return fmt.Errorf("projectprovision: %s secret is a published conformance vector secret; generate a real one", t.Name)
	}
	// The same rule as the secret above, and it belongs here for the same reason
	// the sibling validator applies it (internal/cardactiondispatch/registry.go
	// refuses an untrimmed raw value before parsing): "reject, do not trim" applied
	// to one of the two configured values is a principle with a hole in it. The
	// loader happens to trim this one today, so the gap is reachable only through a
	// hand-built Target — and there a trailing space is not a control byte,
	// url.Parse accepts it, it survives into EscapedPath() as %20, the signature
	// and the wire path agree, and the peer answers 404, which retries to abandoned.
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
