package permissionbundle

// OPTIONAL / NON-PRODUCTION (v45). The external-authority types in this file
// (RecoveryFenceV1, ElectedAuthoritySession, ExternalAuthorityDriver,
// FencedAuthorityAdapter) are retained as historical interfaces and synthetic
// test seams. They are not a delivery prerequisite and no operator is required
// to provision an external fence, election or recovery service. They are not
// wired into live startup with a no-op, and nothing here claims restore-safe
// acknowledged-state recovery for ordinary SQL.

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/gocraft/dbr/v2"
)

const AuthorityModeExternalFencedV1 = "external_fenced_v1"

// RecoveryFenceV1 is the surviving external recovery and election fence record.
// Incarnation is the 32-hex continuity-verified authority lineage stored in
// NativeAnchor.AuthorityIncarnation and reported in NativeFactSource SQL cuts.
// ElectionGeneration is the strictly positive HA writer term/epoch; a normal
// failover bumps ElectionGeneration while preserving Incarnation after
// RecoverAtLeast proves RecoverableScopeCheckpoint >= MinAckPosition.
type RecoveryFenceV1 struct {
	AuthorityID                string `json:"authority_id"`
	Incarnation                string `json:"incarnation"`
	ElectionGeneration         uint64 `json:"election_generation"`
	MinAckPosition             uint64 `json:"min_ack_position"`
	RecoverableScopeCheckpoint uint64 `json:"recoverable_scope_checkpoint"`
	CatalogDigest              string `json:"catalog_digest"`
}

func (f RecoveryFenceV1) Validate(expectedCatalogDigest string) error {
	if !validText(f.AuthorityID, 128, 128) || !validHex(f.Incarnation, 32) || f.ElectionGeneration == 0 || !validHex(f.CatalogDigest, 64) {
		return fmt.Errorf("%w: invalid recovery fence fields", ErrUnavailable)
	}
	if expectedCatalogDigest != "" && f.CatalogDigest != expectedCatalogDigest {
		return fmt.Errorf("%w: recovery fence catalog digest mismatch", ErrUnavailable)
	}
	if f.RecoverableScopeCheckpoint < f.MinAckPosition {
		return fmt.Errorf("%w: recoverable scope checkpoint %d behind min_ack_position %d", ErrUnavailable, f.RecoverableScopeCheckpoint, f.MinAckPosition)
	}
	return nil
}

// ElectedAuthoritySession binds one RepeatableRead SQL transaction to the
// currently elected writer/session under a verified authority lineage.
type ElectedAuthoritySession struct {
	AuthorityID        string `json:"authority_id"`
	Incarnation        string `json:"incarnation"`
	ElectionGeneration uint64 `json:"election_generation"`
	Position           uint64 `json:"position"`
}

func (s ElectedAuthoritySession) Validate() error {
	if !validText(s.AuthorityID, 128, 128) || !validHex(s.Incarnation, 32) || s.ElectionGeneration == 0 {
		return fmt.Errorf("%w: invalid elected authority session", ErrUnavailable)
	}
	return nil
}

// ExternalAuthorityDriver is the concrete seam for external SQL writer election,
// session fencing, monotonic commit positions, and surviving recovery coverage.
// Neither local SQL writable flags (@@read_only / @@super_read_only) nor a
// counter stored in the same MySQL instance can implement this seam.
type ExternalAuthorityDriver interface {
	RecoverAtLeast(ctx context.Context, expectedCatalogDigest string) (RecoveryFenceV1, error)
	AcquireCurrent(ctx context.Context, tx *dbr.Tx) (ElectedAuthoritySession, error)
	AssertCurrent(ctx context.Context, tx *dbr.Tx, session ElectedAuthoritySession) error
	RecordAcknowledgedPosition(ctx context.Context, session ElectedAuthoritySession) error
}

// RejectUnfencedAuthorityMode rejects local SQL writable variables, same-DB
// counters, and unverified pool/lease substitutes that cannot establish
// FencedAuthority or RecoverAtLeast across failover and point-in-time restore.
func RejectUnfencedAuthorityMode(mode string) error {
	normalized := strings.ToLower(strings.TrimSpace(mode))
	switch normalized {
	case AuthorityModeExternalFencedV1:
		return nil
	case "", "none", "disabled":
		return fmt.Errorf("%w: fenced authority mode not configured", ErrUnavailable)
	case "sql_read_only_variables", "mysql_read_only", "super_read_only", "@@read_only", "@@super_read_only":
		return fmt.Errorf("%w: local SQL writable flags (%s) cannot substitute for FencedAuthority or RecoverAtLeast", ErrUnavailable, mode)
	case "same_db_counter", "local_sql_table", "same_mysql_counter":
		return fmt.Errorf("%w: same-DB counter (%s) rolls back with the restored database and cannot substitute for FencedAuthority or RecoverAtLeast", ErrUnavailable, mode)
	case "unverified_pool", "server_uuid", "timestamp", "cutover_floor", "credential_writer_lease", "session_rollout_floor":
		return fmt.Errorf("%w: unfenced authority substitute (%s) is not a valid FencedAuthority", ErrUnavailable, mode)
	default:
		return fmt.Errorf("%w: unsupported authority mode %q", ErrUnavailable, mode)
	}
}

// FencedAuthorityAdapter implements FencedAuthority over an ExternalAuthorityDriver.
type FencedAuthorityAdapter struct {
	driver                ExternalAuthorityDriver
	expectedCatalogDigest string

	mu        sync.RWMutex
	recovered bool
	fence     RecoveryFenceV1
}

func NewFencedAuthorityAdapter(ctx context.Context, driver ExternalAuthorityDriver, expectedCatalogDigest string) (*FencedAuthorityAdapter, error) {
	if driver == nil || !validHex(expectedCatalogDigest, 64) {
		return nil, fmt.Errorf("%w: missing external authority driver or catalog digest", ErrUnavailable)
	}
	adapter := &FencedAuthorityAdapter{
		driver:                driver,
		expectedCatalogDigest: expectedCatalogDigest,
	}
	if _, err := adapter.RecoverAtLeast(ctx); err != nil {
		return nil, err
	}
	return adapter, nil
}

func (a *FencedAuthorityAdapter) RecoverAtLeast(ctx context.Context) (RecoveryFenceV1, error) {
	if a == nil || a.driver == nil {
		return RecoveryFenceV1{}, ErrUnavailable
	}
	fence, err := a.driver.RecoverAtLeast(ctx, a.expectedCatalogDigest)
	if err != nil {
		return RecoveryFenceV1{}, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	if err := fence.Validate(a.expectedCatalogDigest); err != nil {
		return RecoveryFenceV1{}, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.recovered {
		if fence.AuthorityID != a.fence.AuthorityID || fence.Incarnation != a.fence.Incarnation {
			return RecoveryFenceV1{}, fmt.Errorf("%w: discontinuous authority incarnation change requires explicit store reprovisioning", ErrUnavailable)
		}
		if fence.ElectionGeneration < a.fence.ElectionGeneration || fence.MinAckPosition < a.fence.MinAckPosition {
			return RecoveryFenceV1{}, fmt.Errorf("%w: authority recovery fence regressed", ErrUnavailable)
		}
	}
	a.fence = fence
	a.recovered = true
	return fence, nil
}

func (a *FencedAuthorityAdapter) Fence() (RecoveryFenceV1, bool) {
	if a == nil {
		return RecoveryFenceV1{}, false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.fence, a.recovered
}

func (a *FencedAuthorityAdapter) VerifySQLAuthorityCut(authorityID, incarnation, position string) error {
	if a == nil {
		return ErrUnavailable
	}
	pos, err := U64(position)
	if err != nil {
		return ErrUnavailable
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	if !a.recovered || authorityID != a.fence.AuthorityID || incarnation != a.fence.Incarnation || pos < a.fence.MinAckPosition {
		return ErrUnavailable
	}
	return nil
}

func (a *FencedAuthorityAdapter) Begin(ctx context.Context, tx *dbr.Tx) (AuthorityRead, error) {
	if a == nil || a.driver == nil || tx == nil {
		return nil, ErrUnavailable
	}
	a.mu.RLock()
	recovered := a.recovered
	a.mu.RUnlock()
	if !recovered {
		return nil, ErrUnavailable
	}
	session, err := a.driver.AcquireCurrent(ctx, tx)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	if err := session.Validate(); err != nil {
		return nil, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.recovered || session.AuthorityID != a.fence.AuthorityID || session.Incarnation != a.fence.Incarnation {
		return nil, ErrUnavailable
	}
	if session.ElectionGeneration < a.fence.ElectionGeneration || session.Position < a.fence.MinAckPosition {
		return nil, ErrUnavailable
	}
	if session.ElectionGeneration > a.fence.ElectionGeneration {
		a.fence.ElectionGeneration = session.ElectionGeneration
	}
	return &FencedAuthorityRead{
		adapter: a,
		tx:      tx,
		session: session,
	}, nil
}

// FencedAuthorityRead binds one transaction's authority read/write lease.
// Incarnation() returns the 32-hex continuity-verified authority lineage and
// remains distinct from ElectionGeneration().
type FencedAuthorityRead struct {
	adapter *FencedAuthorityAdapter
	tx      *dbr.Tx
	session ElectedAuthoritySession
}

func (r *FencedAuthorityRead) Incarnation() string {
	if r == nil {
		return ""
	}
	return r.session.Incarnation
}

func (r *FencedAuthorityRead) ElectionGeneration() uint64 {
	if r == nil {
		return 0
	}
	return r.session.ElectionGeneration
}

func (r *FencedAuthorityRead) AuthorityID() string {
	if r == nil {
		return ""
	}
	return r.session.AuthorityID
}

func (r *FencedAuthorityRead) Position() uint64 {
	if r == nil {
		return 0
	}
	return r.session.Position
}

func (r *FencedAuthorityRead) Session() ElectedAuthoritySession {
	if r == nil {
		return ElectedAuthoritySession{}
	}
	return r.session
}

func (r *FencedAuthorityRead) Validate(ctx context.Context) error {
	if r == nil || r.adapter == nil || r.tx == nil {
		return ErrUnavailable
	}
	r.adapter.mu.RLock()
	recovered := r.adapter.recovered
	fence := r.adapter.fence
	r.adapter.mu.RUnlock()
	if !recovered || r.session.AuthorityID != fence.AuthorityID || r.session.Incarnation != fence.Incarnation || r.session.ElectionGeneration < fence.ElectionGeneration || r.session.Position < fence.MinAckPosition {
		return ErrUnavailable
	}
	if err := r.adapter.driver.AssertCurrent(ctx, r.tx, r.session); err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	return nil
}

func (r *FencedAuthorityRead) Acknowledge(ctx context.Context) error {
	if r == nil || r.adapter == nil {
		return ErrReconcile
	}
	if err := r.adapter.driver.RecordAcknowledgedPosition(ctx, r.session); err != nil {
		return fmt.Errorf("%w: %v", ErrReconcile, err)
	}
	r.adapter.mu.Lock()
	defer r.adapter.mu.Unlock()
	if r.session.Position > r.adapter.fence.MinAckPosition {
		r.adapter.fence.MinAckPosition = r.session.Position
	}
	if r.session.Position > r.adapter.fence.RecoverableScopeCheckpoint {
		r.adapter.fence.RecoverableScopeCheckpoint = r.session.Position
	}
	if r.session.ElectionGeneration > r.adapter.fence.ElectionGeneration {
		r.adapter.fence.ElectionGeneration = r.session.ElectionGeneration
	}
	return nil
}
