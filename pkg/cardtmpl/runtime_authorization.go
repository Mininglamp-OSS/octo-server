package cardtmpl

// PR-C D4 — the narrow authorization resolver contract.
//
// Before PR-C the runtime read the activation pointer, then the artifact/block
// row, then (had there been one) a grant row, each in its own statement. Three
// independent reads can interleave with an activate/block/revoke commit and
// produce a decision assembled from three different points in time — a card
// authorized against an activation that no longer exists, or against a grant
// that was revoked between the two reads.
//
// This file defines the replacement: a single store call that returns every
// input of one authorization decision, read in one primary-DB snapshot. The
// catalog cannot pull the call apart, so there is no way to reintroduce the
// torn read by accident.
//
// The returned value is a *receipt*, not a bearer capability. It describes the
// snapshot that produced it and is valid only for the request that asked for
// it: it is never cached, never handed to another request, and never used to
// skip a later re-resolve. Bot profile and Bot send are two separate decisions
// precisely because of this — send re-resolves rather than trusting the
// profile's receipt.

import (
	"context"
	"fmt"
	"strings"
	"sync"
)

// RuntimeGrantScope names which stored row won the exact-over-global
// precedence rule. It exists for logs, metrics and same-request assertions.
type RuntimeGrantScope string

const (
	// RuntimeGrantScopeExact is a row scoped to the requesting principal's own
	// Space. It is authoritative: an exact row overrides the global row
	// entirely, including an exact tombstone shadowing an active global grant.
	RuntimeGrantScopeExact RuntimeGrantScope = "exact"
	// RuntimeGrantScopeGlobal is the Space-independent fallback row, consulted
	// only when the principal has no row scoped to this Space.
	RuntimeGrantScopeGlobal RuntimeGrantScope = "global"
)

// RuntimeGrant is the effective permission set for one principal against one
// template ID, already reduced by the store's precedence rule. Permissions are
// never a union across scopes (invariant 11), so this carries the winning
// row's permissions verbatim — a revoked winner yields all-false, which is a
// denial and not an invitation to look at the loser.
type RuntimeGrant struct {
	// Found reports that some row existed for this principal, active or
	// revoked. A false Found and a revoked-tombstone Found are both denials;
	// they differ only in what gets logged.
	Found    bool
	Scope    RuntimeGrantScope
	Revision uint64

	Discover bool
	Send     bool
	Edit     bool
}

// Allows reports whether this grant carries the permission the purpose needs.
func (g RuntimeGrant) Allows(purpose CatalogPurpose) bool {
	switch purpose {
	case CatalogPurposeNewSend:
		return g.Send
	case CatalogPurposeDiscover:
		return g.Discover
	case CatalogPurposeHistoricalEdit, CatalogPurposeActionContext:
		// action_context is gated on edit rather than on discover: an action
		// callback is the precondition of the edit that answers it, so a
		// producer whose edit grant was revoked must stop being able to drive
		// the card at all, not merely stop being able to write the result.
		return g.Edit
	default:
		return false
	}
}

// RuntimeAuthorizationQuery is the input of one snapshot.
type RuntimeAuthorizationQuery struct {
	ID ID
	// Version pins the stored exact version for historical_edit and
	// action_context, which must never follow the activation pointer. Empty
	// asks the store to resolve the pointer inside the same snapshot, which is
	// the only correct way to serve a new send's default version.
	Version string
	// Principal is the authenticated or stored server-authored identity. A
	// principal the grant model cannot express (system, or a blank ID) yields
	// an empty RuntimeGrant rather than an error: it is simply ungranted.
	Principal CatalogPrincipal
}

// RuntimeAuthorization is the bounded receipt of one primary-DB snapshot.
type RuntimeAuthorization struct {
	// Activation is the pointer as it existed in the snapshot. It is zero when
	// the query pinned an explicit version, because a pinned read must not
	// consult the pointer at all.
	Activation RuntimeActivation
	// Version is the effective version the snapshot resolved to. Empty means
	// the template has no activation row and the caller falls back to whatever
	// the frozen static Registry provides.
	Version string
	// Artifact is the claim/artifact/block row for Version, loaded in the same
	// snapshot. It is zero when Version is empty.
	Artifact RuntimeArtifactMeta
	// Grant is the reduced permission set for Principal against ID.
	Grant RuntimeGrant
}

// RuntimeAuthorizationStore is the indivisible resolver. Implementations MUST
// answer the whole query from one consistent read; returning fields gathered
// from separate statements defeats the entire point of this interface.
type RuntimeAuthorizationStore interface {
	LoadAuthorization(context.Context, RuntimeAuthorizationQuery) (RuntimeAuthorization, error)
}

// RuntimeAdvertisedTemplate is one template a principal holds a grant on,
// paired with the activation snapshot that decides whether it is advertisable
// right now.
type RuntimeAdvertisedTemplate struct {
	ID            ID
	Authorization RuntimeAuthorization
}

// RuntimeAdvertiser enumerates a principal's granted templates. It exists for
// capability manifests, which describe what a producer *could* ask for. It is
// explicitly not an authorization decision: a manifest is assembled from
// several snapshots and can be stale by the time the producer acts on it,
// which is why every send re-resolves through RuntimeAuthorizationStore rather
// than trusting what the manifest said.
type RuntimeAdvertiser interface {
	ListAuthorizedTemplates(context.Context, CatalogPrincipal, int) ([]RuntimeAdvertisedTemplate, error)
}

// RuntimeAuthorizationSource is the process-wide seam consumers bind to. It
// carries the new-send gate alongside the readers so that a caller can honour
// "gate off means do not touch the runtime DB at all" without having to import
// the module that owns the environment variables.
type RuntimeAuthorizationSource interface {
	RuntimeAuthorizationStore
	RuntimeAdvertiser
	// NewSendEnabled reports the deployment's dynamic new-send gate. When it is
	// false the dynamic catalog is dark: callers keep their existing static
	// behaviour and must not acquire a runtime DB dependency for it.
	NewSendEnabled() bool
}

var (
	defaultAuthorizationMu     sync.RWMutex
	defaultAuthorizationSource RuntimeAuthorizationSource
)

// SetDefaultAuthorizationSource installs the process resolver. Production
// installs it once in the composition root; passing nil (as tests do) leaves
// consumers with no dynamic overlay, which is the pre-PR-C behaviour.
func SetDefaultAuthorizationSource(source RuntimeAuthorizationSource) {
	defaultAuthorizationMu.Lock()
	defer defaultAuthorizationMu.Unlock()
	defaultAuthorizationSource = source
}

// DefaultAuthorizationSource returns the installed resolver, or nil when none
// is installed. A nil return means "no dynamic catalog", never "allow".
func DefaultAuthorizationSource() RuntimeAuthorizationSource {
	defaultAuthorizationMu.RLock()
	defer defaultAuthorizationMu.RUnlock()
	return defaultAuthorizationSource
}

// ActivationVersion maps an activation row to the version new sends resolve
// to. Both the catalog and the store call this — the store to decide which
// artifact row to read inside the snapshot, the catalog to re-derive the same
// answer and reject a store that disagreed. Two callers, one implementation,
// so the rule cannot drift into two subtly different truths.
func ActivationVersion(id ID, activation RuntimeActivation) (string, error) {
	if !activation.Exists {
		return "", nil
	}
	switch activation.Status {
	case RuntimeActivationDisabled:
		return "", fmt.Errorf("%w: %s revision %d", ErrRuntimeCatalogDisabled, id, activation.Revision)
	case RuntimeActivationActive:
		if strings.TrimSpace(activation.Version) == "" {
			return "", fmt.Errorf("%w: active row has no version", ErrRuntimeCatalogIntegrity)
		}
		return activation.Version, nil
	default:
		return "", fmt.Errorf("%w: unknown activation status %q", ErrRuntimeCatalogIntegrity, activation.Status)
	}
}
