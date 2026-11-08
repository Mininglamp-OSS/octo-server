package permissionbundle

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"time"

	"github.com/gocraft/dbr/v2"
)

// FencedAuthority must bind the actual elected writer/session, fence old reads
// AND writes and reconcile every acknowledged durable position after recovery.
// A pool, server UUID or timestamp is not an implementation. There is no
// permissive fallback when this integration is absent.
type FencedAuthority interface {
	Begin(context.Context, *dbr.Tx) (AuthorityRead, error)
}
type AuthorityRead interface {
	Incarnation() string
	Validate(context.Context) error
	// Acknowledge runs after COMMIT. Failure means reconciliation is required;
	// callers must not replay a mutation or claim rollback.
	Acknowledge(context.Context) error
}
type NativeAnchor struct {
	AuthorityIncarnation string `json:"authority_incarnation"`
	RobotRecordID        string `json:"robot_record_id"`
	BotUserRecordID      string `json:"bot_user_record_id"`
	CreatorUID           string `json:"creator_uid"`
	CreatorUserRecordID  string `json:"creator_user_record_id"`
	SpaceRecordID        string `json:"space_record_id"`
}

// NativeGuard owns current locking native SQL predicates. Every native row and
// missing-key range is acquired before the final header suffix, under one edit
// deadline. Implementations must never treat a failed query as absent/eligible.
type NativeGuard interface {
	Scope(context.Context, *dbr.Tx, Key, string, string) (NativeAnchor, error)
	Rules(context.Context, *dbr.Tx, Key, []Rule) error
	Prepare(context.Context, *dbr.Tx, Key, []string) ([]map[string]any, error)
}
type Store struct {
	Session   *dbr.Session
	Origin    string
	Authority FencedAuthority
	Native    NativeGuard
	Releases  Releases
	// Empty only in in-package disposable SQLite tests. Production always uses
	// the current InnoDB locking suffix; this is not a second runtime store.
	testNoLocks bool
}
type Attribution struct {
	UpdatedBy *string `json:"updated_by"`
	UpdatedAt string  `json:"updated_at"`
}
type OwnerResponse struct {
	Bundle      Bundle          `json:"bundle"`
	Attribution Attribution     `json:"attribution"`
	Contract    json.RawMessage `json:"contract"`
}
type header struct {
	p            Bundle
	anchor       NativeAnchor
	anchorDigest string
	attribution  Attribution
}
type scopeTx struct {
	tx        *dbr.Tx
	authority AuthorityRead
	anchor    NativeAnchor
	key       Key
	actor     string
}

func (s Store) begin(ctx context.Context, key Key, actor string) (*scopeTx, error) {
	if s.Session == nil || s.Native == nil || s.Authority == nil || s.Releases == nil || key.Origin != s.Origin || !key.Valid() || actor != "" && !ValidNativeID(actor) {
		return nil, ErrUnavailable
	}
	session := *s.Session
	session.EventReceiver = &dbr.NullEventReceiver{}
	tx, e := session.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead})
	if e != nil {
		return nil, ErrUnavailable
	}
	a, e := s.Authority.Begin(ctx, tx)
	if e != nil || a == nil || a.Incarnation() == "" {
		tx.RollbackUnlessCommitted()
		return nil, ErrUnavailable
	}
	anchor, e := s.Native.Scope(ctx, tx, key, actor, a.Incarnation())
	if e != nil {
		tx.RollbackUnlessCommitted()
		return nil, e
	}
	if !validAnchor(anchor) || anchor.AuthorityIncarnation != a.Incarnation() {
		tx.RollbackUnlessCommitted()
		return nil, ErrUnavailable
	}
	return &scopeTx{tx, a, anchor, key, actor}, nil
}
func validAnchor(a NativeAnchor) bool {
	if !validHex(a.AuthorityIncarnation, 32) || !ValidNativeID(a.CreatorUID) {
		return false
	}
	for _, s := range []string{a.RobotRecordID, a.BotUserRecordID, a.CreatorUserRecordID, a.SpaceRecordID} {
		n, e := U64(s)
		if e != nil || n == 0 || n > 9223372036854775807 {
			return false
		}
	}
	return true
}
func anchorBytes(a NativeAnchor) []byte { m, _ := asObject(a); return canonical(m) }
func (s Store) read(ctx context.Context, t *scopeTx, lock string) (header, error) {
	q := `SELECT scope_incarnation,revision,context_epoch,schema_version,catalog_version,catalog_digest,bundle_json,bundle_digest,native_anchor_json,native_anchor_digest,retired,updated_by,updated_at FROM bot_permission_scope WHERE bot_uid=? AND space_id=?`
	if lock != "" && !s.testNoLocks {
		q += " FOR " + lock + " NOWAIT"
	}
	var nonce, catDigest, bundle, bd, anchor, ad []byte
	var rev, epoch string
	var version, retired int
	var cat string
	var actor sql.NullString
	var at time.Time
	e := t.tx.QueryRowContext(ctx, q, t.key.BotUID, t.key.SpaceID).Scan(&nonce, &rev, &epoch, &version, &cat, &catDigest, &bundle, &bd, &anchor, &ad, &retired, &actor, &at)
	if e != nil || len(nonce) != 16 || len(catDigest) != 32 || len(bd) != 32 || len(ad) != 32 || retired != 0 {
		return header{}, ErrUnavailable
	}
	p, e := ParseBundle(bytes.NewReader(bundle))
	if e != nil || p.Key != t.key || p.Incarnation != hex.EncodeToString(nonce) || p.Revision != rev || p.ContextEpoch != epoch || p.SchemaVersion != version || p.CatalogVersion != cat || p.CatalogDigest != hex.EncodeToString(catDigest) || p.Digest != hex.EncodeToString(bd) {
		return header{}, ErrUnavailable
	}
	v, e := decodeStrict(bytes.NewReader(anchor), 4096)
	if e != nil {
		return header{}, ErrUnavailable
	}
	canonicalAnchor := canonical(v)
	if hash(canonicalAnchor) != hex.EncodeToString(ad) || !bytes.Equal(canonicalAnchor, anchorBytes(t.anchor)) {
		return header{}, ErrUnavailable
	}
	var by *string
	if actor.Valid {
		if !ValidNativeID(actor.String) {
			return header{}, ErrUnavailable
		}
		by = &actor.String
	}
	return header{p, t.anchor, hex.EncodeToString(ad), Attribution{by, at.UTC().Format(time.RFC3339Nano)}}, nil
}
func complete(ctx context.Context, t *scopeTx) error {
	if t.authority.Validate(ctx) != nil {
		return ErrUnavailable
	}
	if t.tx.Commit() != nil {
		return ErrUnavailable
	}
	if t.authority.Acknowledge(ctx) != nil {
		return ErrUnavailable
	}
	return nil
}
func owner(h header) OwnerResponse { return OwnerResponse{h.p, h.attribution, Descriptor()} }
func (s Store) Get(ctx context.Context, key Key, actor string) (OwnerResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	t, e := s.begin(ctx, key, actor)
	if e != nil {
		return OwnerResponse{}, e
	}
	defer t.tx.RollbackUnlessCommitted()
	h, e := s.read(ctx, t, "SHARE")
	if e != nil {
		return OwnerResponse{}, e
	}
	release, e := loadRelease(ctx, s.Releases, h.p.Pin())
	if e != nil || !catalogMembers(h.p, release) {
		return OwnerResponse{}, ErrUnavailable
	}
	if e = complete(ctx, t); e != nil {
		return OwnerResponse{}, e
	}
	return owner(h), nil
}
func (s Store) Self(ctx context.Context, key Key) (Bundle, error) {
	p, e := s.Get(ctx, key, "")
	return p.Bundle, e
}

// Mutate parses after current scope authorization, resolves references before
// the header suffix and compares the final locked header against the preflight
// bytes. Lost/ambiguous commit is never retried.
func (s Store) Mutate(ctx context.Context, key Key, actor string, r io.Reader) (OwnerResponse, error) {
	if actor == "" {
		return OwnerResponse{}, ErrForbidden
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	t, e := s.begin(ctx, key, actor)
	if e != nil {
		return OwnerResponse{}, e
	}
	defer t.tx.RollbackUnlessCommitted()
	m, e := ParseMutation(r)
	if e != nil {
		return OwnerResponse{}, e
	}
	prior, e := s.read(ctx, t, "")
	if e != nil {
		return OwnerResponse{}, e
	}
	next, e := Apply(ctx, prior.p, m, s.Releases)
	if e != nil {
		return OwnerResponse{}, e
	}
	rules, e := ReferencesToValidate(ctx, prior.p, next, m, s.Releases)
	if e != nil {
		return OwnerResponse{}, e
	}
	if e = s.Native.Rules(ctx, t.tx, key, rules); e != nil {
		return OwnerResponse{}, e
	}
	current, e := s.read(ctx, t, "UPDATE")
	if e != nil {
		return OwnerResponse{}, e
	}
	if current.p.Expected() != m.Expected || current.p.Digest != prior.p.Digest {
		return OwnerResponse{}, ErrConflict
	}
	next, body, e := Seal(next)
	if e != nil {
		return OwnerResponse{}, e
	}
	nonce, _ := hex.DecodeString(next.Incarnation)
	cat, _ := hex.DecodeString(next.CatalogDigest)
	bd, _ := hex.DecodeString(next.Digest)
	ad, _ := hex.DecodeString(current.anchorDigest)
	oldCat, _ := hex.DecodeString(prior.p.CatalogDigest)
	at := time.Now().UTC().Truncate(time.Microsecond)
	result, e := t.tx.ExecContext(ctx, `UPDATE bot_permission_scope SET revision=?,context_epoch=?,schema_version=1,catalog_version=?,catalog_digest=?,bundle_json=?,bundle_digest=?,updated_by=?,updated_at=? WHERE bot_uid=? AND space_id=? AND scope_incarnation=? AND revision=? AND catalog_version=? AND catalog_digest=? AND native_anchor_digest=? AND retired=0`, next.Revision, next.ContextEpoch, next.CatalogVersion, cat, string(body), bd, actor, at, key.BotUID, key.SpaceID, nonce, prior.p.Revision, prior.p.CatalogVersion, oldCat, ad)
	if e != nil {
		return OwnerResponse{}, ErrUnavailable
	}
	n, e := result.RowsAffected()
	if e != nil {
		return OwnerResponse{}, ErrUnavailable
	}
	if n != 1 {
		return OwnerResponse{}, ErrConflict
	}
	if e = complete(ctx, t); e != nil {
		return OwnerResponse{}, e
	}
	return owner(header{next, current.anchor, current.anchorDigest, Attribution{&actor, at.Format(time.RFC3339Nano)}}), nil
}

type Preparation struct {
	Key      Key      `json:"key"`
	Expected Expected `json:"expected"`
	PeerUIDs []string `json:"peer_uids"`
}
type Prepared struct {
	Key        Key              `json:"key"`
	Expected   Expected         `json:"expected"`
	References []map[string]any `json:"references"`
}

func (s Store) Prepare(ctx context.Context, key Key, actor string, r io.Reader) (Prepared, error) {
	if actor == "" {
		return Prepared{}, ErrForbidden
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	t, e := s.begin(ctx, key, actor)
	if e != nil {
		return Prepared{}, e
	}
	defer t.tx.RollbackUnlessCommitted()
	req, e := ParsePreparation(r)
	if e != nil {
		return Prepared{}, e
	}
	if req.Key != key {
		return Prepared{}, ErrInvalid
	}
	prior, e := s.read(ctx, t, "")
	if e != nil {
		return Prepared{}, e
	}
	if prior.p.Expected() != req.Expected {
		return Prepared{}, ErrConflict
	}
	release, e := loadRelease(ctx, s.Releases, prior.p.Pin())
	if e != nil || !catalogMembers(prior.p, release) {
		return Prepared{}, ErrUnavailable
	}
	refs, e := s.Native.Prepare(ctx, t.tx, key, req.PeerUIDs)
	if e != nil {
		return Prepared{}, e
	}
	if len(refs) != len(req.PeerUIDs) {
		return Prepared{}, ErrUnavailable
	}
	h, e := s.read(ctx, t, "SHARE")
	if e != nil {
		return Prepared{}, e
	}
	if h.p.Expected() != req.Expected || h.p.Digest != prior.p.Digest {
		return Prepared{}, ErrConflict
	}
	if e = complete(ctx, t); e != nil {
		return Prepared{}, e
	}
	return Prepared{key, req.Expected, refs}, nil
}

// ProvisionSuffix is for an existing native association transaction only. All
// native work and authority guards must already be held. It never repairs on
// GET, upserts, deletes or changes an existing incarnation. The commit-owning
// native caller must validate/acknowledge its authority and append no native
// steps after this suffix. Multi-scope callers sort exact keys before calling.
func (s Store) ProvisionSuffix(ctx context.Context, tx *dbr.Tx, key Key, anchor NativeAnchor, pin CatalogPin) error {
	if tx == nil || key.Origin != s.Origin || !key.Valid() || !validAnchor(anchor) {
		return ErrUnavailable
	}
	if _, e := loadRelease(ctx, s.Releases, pin); e != nil {
		return e
	}
	t := &scopeTx{tx: tx, anchor: anchor, key: key}
	existing, e := s.read(ctx, t, "UPDATE")
	if e == nil {
		if existing.anchor != anchor {
			return ErrUnavailable
		}
		return nil
	}
	// Re-read absence explicitly: malformed/existing rows can never become a
	// fresh U bundle. The unique key also rejects competing provisioning.
	q := `SELECT bot_uid FROM bot_permission_scope WHERE bot_uid=? AND space_id=?`
	if !s.testNoLocks {
		q += " FOR UPDATE"
	}
	var uid string
	if e = tx.QueryRowContext(ctx, q, key.BotUID, key.SpaceID).Scan(&uid); !errors.Is(e, sql.ErrNoRows) {
		return ErrUnavailable
	}
	nonce := make([]byte, 16)
	if _, e = rand.Read(nonce); e != nil {
		return ErrUnavailable
	}
	p := Bundle{1, key, hex.EncodeToString(nonce), "0", "0", pin.Version, pin.Digest, "", Stage{"U", []Rule{}}, Stage{"U", []Rule{}}}
	p, b, e := Seal(p)
	if e != nil {
		return e
	}
	cat, _ := hex.DecodeString(pin.Digest)
	bd, _ := hex.DecodeString(p.Digest)
	ab := anchorBytes(anchor)
	ad, _ := hex.DecodeString(hash(ab))
	_, e = tx.ExecContext(ctx, `INSERT INTO bot_permission_scope (bot_uid,space_id,scope_incarnation,revision,context_epoch,schema_version,catalog_version,catalog_digest,bundle_json,bundle_digest,native_anchor_json,native_anchor_digest,retired,updated_by,updated_at) VALUES (?,?,?,0,0,1,?,?,?,?,?,?,0,NULL,?)`, key.BotUID, key.SpaceID, nonce, pin.Version, cat, string(b), bd, string(ab), ad, time.Now().UTC().Truncate(time.Microsecond))
	if e != nil {
		return ErrUnavailable
	}
	return nil
}
