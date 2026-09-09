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
	// provisionStatusReady — the target answered 200 at least once.
	//
	// It does NOT mean "the container exists now": the container can be deleted,
	// archived or migrated on the subsystem side and nothing informs this service.
	// That is exactly why D12 forbids any read path from gating on this table.
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

// containerIDRandomBytes is the entropy behind an opaque container id. 16 bytes /
// 128 bits: this id is a capability until fleet's R2 and drive's R3 narrow their
// gates by Project (brief R1), so it has to be unguessable rather than merely
// unique.
const containerIDRandomBytes = 16

// containerIDPrefix returns the per-target human-readable prefix.
//
// A CONSTANT prefix does not weaken opacity — it carries no information about the
// project — and it earns its keep on the subsystem side, where an operator looking
// at a `workspace.slug` or a `drive_space.id` can tell at a glance that octo-server
// created it and for which integration.
//
// The prefix lives on provisionTargetSpec, so a new subsystem declares it in the
// registry rather than here.
func containerIDPrefix(target string) (string, bool) {
	spec, ok := lookupProvisionTarget(target)
	if !ok {
		return "", false
	}
	return spec.containerPrefix, true
}

// newContainerID mints an opaque container id for one target.
//
// **Not a function of project_id, and this is the load-bearing property of the
// whole slice.** A derived id (`project:<project_id>`, `p-<project_id>`) was the
// first design and is rejected under eager provisioning, because
// listVisibleInSpace lets any active Space member read the project_id of every
// space_listed project in that Space while fleet's workspace gate admits on octo
// Space membership alone and then materializes the caller as a workspace member
// with a row nothing ever deletes. Derivability is what completes that chain from
// "can list projects" to "is permanently a member of every project's workspace"
// (brief P-3). A source guard asserts nothing in this package builds a container id
// out of a project id.
//
// crypto/rand, and an error is fatal to the create: a container id we could not
// randomize must never be written. Returning a fallback (a counter, a timestamp,
// the project id) is the one outcome that silently reintroduces derivability at
// the exact moment the entropy source is broken.
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
// ContainerID is present because the worker has to send it, and absent from
// everything else: it is never logged, never put in last_error, and never reaches
// a client response.
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
// Must comfortably exceed one job's real duration. A job is a single bounded
// outbound call (defaultProvisionTimeout, 10s) plus two short statements, so two
// minutes is ~10x headroom. A lease that expires mid-job lets a second replica
// legitimately re-claim and run the SAME row concurrently — the per-claim owner
// only makes the late writer's UPDATE land on nothing, it does not prevent the
// duplicate call. The target's ensure is idempotent on container_id, so a
// duplicate converges; the lease is the first line of defence, not the only one.
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
