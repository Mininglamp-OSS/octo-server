package project

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

// Provisioning job status. Mirrors octo_project_provisioning.status.
const (
	// provisionStatusPending — enqueued, not yet confirmed by the target.
	provisionStatusPending uint8 = 0
	// provisionStatusReady — the target answered its synchronous success status
	// at least once (200 for Fleet, 201 for Drive).
	//
	// It does NOT mean "the container exists now": the remote resource can be
	// deleted, archived or migrated on the subsystem side without informing
	// this service. That is exactly why D12 forbids read paths from gating on
	// this table.
	provisionStatusReady uint8 = 1
	// provisionStatusAbandoned — the retry budget is exhausted. Nothing re-drives
	// it; a non-zero count needs a human.
	provisionStatusAbandoned uint8 = 2
	// provisionStatusDisbandPending — the project was disbanded, so the container
	// (if it was ever created) is now reclaimable. Teardown is PULL-based (D9):
	// this state is accounting, and the subsystem learns by asking
	// POST /v1/internal/projects/status. Nothing is pushed outbound.
	provisionStatusDisbandPending uint8 = 3
)

// containerIDRandomBytes is the entropy behind Fleet's opaque container id.
// Drive does not send this field; its remote mapping is keyed by project_id.
const containerIDRandomBytes = 16

// containerIDPrefix returns the per-target human-readable prefix.
//
// A CONSTANT prefix does not weaken opacity — it carries no project information
// — and earns its keep for Fleet operators inspecting a workspace slug. Drive
// rows retain the same local opaque task key for schema compatibility, but it
// never becomes a Drive id or URL.
//
// The prefix lives on provisionTargetSpec, so a new subsystem declares it in
// the registry rather than here.
func containerIDPrefix(target string) (string, bool) {
	spec, ok := lookupProvisionTarget(target)
	if !ok {
		return "", false
	}
	return spec.containerPrefix, true
}

// newContainerID mints an opaque local outbox key for one target.
//
// Fleet currently sends this key as its container_id, so it must not be a
// function of project_id and must remain unguessable. Drive keeps the same
// local column for compatibility and uniqueness, but its worker branch does
// not send, look up or authorize with this value; Drive uses project_id.
//
// crypto/rand failure is fatal to the create: no fallback may reintroduce
// derivability at the moment the entropy source is unavailable.
func newContainerID(target string) (string, error) {
	prefix, ok := containerIDPrefix(target)
	if !ok {
		return "", fmt.Errorf("project: unknown provisioning target %q", target)
	}
	buf := make([]byte, containerIDRandomBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("project: generate container id: %w", err)
	}
	return prefix + hex.EncodeToString(buf), nil
}

// provisioningJob is one claimed outbox row.
//
// ContainerID is a Fleet-only wire key. Drive retains the local field for
// compatibility and uniqueness, but never sends it, looks it up remotely or
// uses it for authorization. It is never logged, put in last_error, or sent
// in a client response.
type provisioningJob struct {
	ID          uint64 `db:"id"`
	ProjectID   string `db:"project_id"`
	SpaceID     string `db:"space_id"`
	Target      string `db:"target"`
	ContainerID string `db:"container_id"`
	Attempts    uint32 `db:"attempts"`
}

// provisioningRetryDelay is the capped exponential backoff between attempts.
//
// Compute then clamp, never clamp the exponent first: clamping `attempt` to 8
// would make 2^8 = 256s the real ceiling and quietly shrink the whole budget below
// the documented one (the same arithmetic slip recorded on
// modules/space.memberRemovalRetryDelay).
func provisioningRetryDelay(attempt uint32) time.Duration {
	const maxDelay = 5 * time.Minute
	if attempt > 12 { // 2^12 s already far exceeds the cap; shifting further overflows
		return maxDelay
	}
	delay := time.Second * time.Duration(1<<attempt)
	if delay > maxDelay {
		return maxDelay
	}
	return delay
}

// provisioningLease is how long one claim is held.
//
// Must comfortably exceed one job's real duration. A job is one bounded
// outbound call (defaultProvisionTimeout, 10s) plus two short statements, so
// two minutes is ~10x headroom. A lease that expires mid-job lets a second
// replica re-claim and issue a duplicate request; Fleet converges on its
// container_id key, while Drive converges on its project_id conflict rule.
const provisioningLease = 2 * time.Minute

// errProvisioningEnqueueFailed marks a create failure that came from the outbox write
// rather than from the project write.
//
// It exists for the metric, not for the wire: both render as the same 500
// store_failed, because neither is anything a client can act on. What the label buys
// is that "project creation started failing" and "project creation started failing
// BECAUSE the provisioning slice was turned on" stop looking identical on a dashboard
// — which matters precisely because this slice made create fail-closed on a write it
// did not previously do.
var errProvisioningEnqueueFailed = errors.New("project: provisioning enqueue failed")

// errProvisioningLeaseLost is returned when a terminal write finds the lease has
// changed hands. Not a failure of the job: another replica took over, and
// duplicate execution is safe by the ensure contract.
var errProvisioningLeaseLost = errors.New("project: provisioning lease ownership lost")

// Drive delivery is skipped when the current Project/Owner cannot satisfy the
// internal create contract. These are retryable local states unless the worker
// detects an outbox/Project Space mismatch, which it records as invalid_request.
var (
	errProvisioningProjectNotReady        = errors.New("project: provisioning project owner is not ready")
	errProvisioningProjectMismatch        = errors.New("project: provisioning project Space changed")
	errProvisioningDriveClientUnavailable = errors.New("project: Drive provisioning client unavailable")
)
