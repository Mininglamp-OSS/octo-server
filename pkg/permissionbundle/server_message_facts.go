package permissionbundle

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gocraft/dbr/v2"
)

// Server-only message facts (v45).
//
// ServerMessageFactProvider implements NativeIMFactProvider over the message
// data octo-server itself persists (sharded message tables, message_extra for
// edit/revoke/delete, message_user_extra and channel_offset for per-user
// deletion/clear-history, channel_setting for channel-level clearing) plus the
// native group/thread membership tables. It performs no IM/WuKongIM access.
//
// Metadata-only: no message, quote or media body is ever placed in any value
// this provider returns. The only payload-derived values are the payload
// SHA-256 digest mandated by the header contract and the bounded fields the
// shared server helpers need (visibles / space tag), which stay internal.
//
// Uncertainty is never fabricated into "true". Values that octo-server storage
// cannot prove for a specific reference are flagged on the resolved delivery
// (MessageSpaceUnknown, MessageVersionUnknown, *OriginalVisibleUnknown) and the
// SQL adapter turns the affected reference into an explicit unavailable /
// non-serving result.
//
// The cut returned by this provider is a read-time marker over ordinary SQL
// state. It is not an acknowledged-state / restore-safe fence and the provider
// makes no failover or backup/restore claim.

const (
	serverMessageStreamID = "octo-server-message-store"

	serverRefDelivery = "s1"
	serverRefHeader   = "h1"
	serverRefSource   = "c1"

	channelTypePerson = 1
	channelTypeGroup  = 2
	channelTypeThread = 5

	serverGroupStatusNormal  = 1
	serverGroupStatusDisband = 2
	serverThreadDeleted      = 3

	// ServerDisclosurePermitTable names the optional SQL ledger table that
	// persists issued/consumed single-use history disclosure permits across
	// process restarts when DurablePermitLedger is enabled.
	ServerDisclosurePermitTable = "bot_permission_disclosure_permit"

	// ServerDisclosurePermitDDL is the portable schema definition for the
	// durable single-use disclosure permit ledger.
	ServerDisclosurePermitDDL = `CREATE TABLE IF NOT EXISTS bot_permission_disclosure_permit (
		permit_sha256 VARCHAR(64) NOT NULL PRIMARY KEY,
		bot_uid VARCHAR(128) NOT NULL,
		space_id VARCHAR(128) NOT NULL,
		context_id VARCHAR(128) NOT NULL,
		context_epoch VARCHAR(64) NOT NULL,
		header_reference VARCHAR(255) NOT NULL,
		payload_sha256 CHAR(64) NOT NULL,
		issued_at BIGINT NOT NULL,
		consumed_at BIGINT NULL
	)`
)

// ServerMessageHooks injects the octo-server owned helpers so this package does
// not carry a divergent copy of shard naming or visibility rules. The concrete
// wiring lives in modules/message.
type ServerMessageHooks struct {
	// MessageTable returns the message shard table for a physical channel id.
	MessageTable func(channelID string) string
	// MessageUserExtraTable returns the per-user message extra shard table.
	MessageUserExtraTable func(uid string) string
	// ChannelOffsetTable returns the per-user channel offset shard table.
	ChannelOffsetTable func(uid string) string
	// PayloadVisibleTo applies the server visibles whitelist to raw payload bytes.
	PayloadVisibleTo func(payload []byte, uid string) bool
	// PayloadSpaceID returns the payload space_id tag ("" when untagged).
	PayloadSpaceID func(payload []byte) string
	// IsSystemUID reports server system principals.
	IsSystemUID func(uid string) bool
}

func (h ServerMessageHooks) valid() bool {
	return h.MessageTable != nil && h.MessageUserExtraTable != nil && h.ChannelOffsetTable != nil &&
		h.PayloadVisibleTo != nil && h.PayloadSpaceID != nil && h.IsSystemUID != nil
}

type ServerMessageFactConfig struct {
	Session *dbr.Session
	Hooks   ServerMessageHooks
	// StoreAuthorityID names the message store in cuts.
	StoreAuthorityID string
	// StoreIncarnation is an operator-provided 32-hex lineage label for the
	// message store. It is required; it is never invented.
	StoreIncarnation string
	// PermitKey (>=32 bytes) authenticates history disclosure permits.
	PermitKey []byte
	// DurablePermitLedger persists issued/consumed disclosure permits in SQL
	// (bot_permission_disclosure_permit) so single-use consumption survives
	// process restarts.
	DurablePermitLedger bool
	Now                 func() time.Time
}

type ServerMessageFactProvider struct {
	db             *dbr.Session
	hooks          ServerMessageHooks
	authorityID    string
	incarnation    string
	permitKey      []byte
	durablePermits bool
	now            func() time.Time

	mu      sync.Mutex
	usedPmt map[string]struct{}
}

func NewServerMessageFactProvider(cfg ServerMessageFactConfig) (*ServerMessageFactProvider, error) {
	if cfg.Session == nil || !cfg.Hooks.valid() || !validHex(cfg.StoreIncarnation, 32) || len(cfg.PermitKey) < 32 {
		return nil, fmt.Errorf("%w: server message facts require a session, shared server hooks, a 32-hex store incarnation and a permit key", ErrUnavailable)
	}
	id := cfg.StoreAuthorityID
	if id == "" {
		id = "octo-server-message-store"
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &ServerMessageFactProvider{
		db: cfg.Session, hooks: cfg.Hooks, authorityID: id, incarnation: cfg.StoreIncarnation,
		permitKey: append([]byte(nil), cfg.PermitKey...), durablePermits: cfg.DurablePermitLedger,
		now: now, usedPmt: map[string]struct{}{},
	}, nil
}

type serverRef struct {
	kind        string
	channelType uint8
	channelID   string
	messageID   string
	version     string
}

func encodeServerRef(prefix string, r serverRef) string {
	s := prefix + "-" + strconv.Itoa(int(r.channelType)) + "-" + hex.EncodeToString([]byte(r.channelID)) + "-" + r.messageID
	if r.version != "" {
		s += "-" + r.version
	}
	return s
}

func decodeServerRef(prefix, ref string) (serverRef, bool) {
	parts := strings.SplitN(ref, "-", 5)
	if len(parts) < 4 || parts[0] != prefix {
		return serverRef{}, false
	}
	t, err := strconv.Atoi(parts[1])
	if err != nil || (t != channelTypePerson && t != channelTypeGroup && t != channelTypeThread) {
		return serverRef{}, false
	}
	ch, err := hex.DecodeString(parts[2])
	if err != nil || len(ch) == 0 {
		return serverRef{}, false
	}
	if _, err := strconv.ParseInt(parts[3], 10, 64); err != nil || parts[3] == "" || parts[3][0] == '-' {
		return serverRef{}, false
	}
	r := serverRef{kind: prefix, channelType: uint8(t), channelID: string(ch), messageID: parts[3]}
	if len(parts) == 5 {
		if parts[4] == "" || strings.ContainsAny(parts[4], "-") {
			return serverRef{}, false
		}
		r.version = parts[4]
	}
	return r, true
}

// ServerDeliveryReference builds the reference an endpoint presents for a
// stored message. version is the native message version observed by the
// endpoint ("" means unknown and makes resolve non-serving for that reference).
func ServerDeliveryReference(channelType uint8, channelID, messageID, version string) string {
	return encodeServerRef(serverRefDelivery, serverRef{channelType: channelType, channelID: channelID, messageID: messageID, version: version})
}

func (p *ServerMessageFactProvider) cut(started time.Time) (IMAuthorityCut, error) {
	nonce, err := randomHex32()
	if err != nil {
		return IMAuthorityCut{}, err
	}
	done := p.now().UTC().Truncate(time.Microsecond)
	return IMAuthorityCut{
		AuthorityID: p.authorityID,
		Incarnation: p.incarnation,
		Position:    strconv.FormatInt(done.UnixMicro(), 10),
		ReadNonce:   nonce,
		StartedAt:   started,
		CompletedAt: done,
	}, nil
}

func (p *ServerMessageFactProvider) lockSuffix(ctx context.Context) string {
	if tc, ok := factReadTxFromContext(ctx); ok && !tc.testNoLocks {
		return " FOR SHARE NOWAIT"
	}
	return ""
}

func (p *ServerMessageFactProvider) one(ctx context.Context, q string, args []any, dest ...any) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if tc, ok := factReadTxFromContext(ctx); ok {
		return tc.tx.QueryRowContext(ctx, q, args...).Scan(dest...)
	}
	return p.db.QueryRowContext(ctx, q, args...).Scan(dest...)
}

func (p *ServerMessageFactProvider) query(ctx context.Context, q string, args ...any) (*sql.Rows, error) {
	if tc, ok := factReadTxFromContext(ctx); ok {
		return tc.tx.QueryContext(ctx, q, args...)
	}
	return p.db.QueryContext(ctx, q, args...)
}

func (p *ServerMessageFactProvider) exec(ctx context.Context, q string, args ...any) (sql.Result, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if tc, ok := factReadTxFromContext(ctx); ok {
		return tc.tx.ExecContext(ctx, q, args...)
	}
	return p.db.ExecContext(ctx, q, args...)
}

type serverMsgRow struct {
	seq       uint32
	fromUID   string
	timestamp int64
	payload   []byte
	deleted   int
}

type serverExtraRow struct {
	version int64
	revoke  int
	deleted int
	edit    sql.NullString
	hash    string
	exists  bool
}

func (p *ServerMessageFactProvider) loadMessage(ctx context.Context, r serverRef) (*serverMsgRow, error) {
	var m serverMsgRow
	err := p.one(ctx, "SELECT message_seq,from_uid,COALESCE(`timestamp`,0),payload,COALESCE(is_deleted,0) FROM "+p.hooks.MessageTable(r.channelID)+" WHERE channel_id=? AND channel_type=? AND message_id=?"+p.lockSuffix(ctx),
		[]any{r.channelID, int(r.channelType), r.messageID}, &m.seq, &m.fromUID, &m.timestamp, &m.payload, &m.deleted)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, ErrUnavailable
	}
	return &m, nil
}

func (p *ServerMessageFactProvider) loadExtra(ctx context.Context, messageID string) (serverExtraRow, error) {
	var e serverExtraRow
	err := p.one(ctx, "SELECT COALESCE(version,0),COALESCE(revoke,0),COALESCE(is_deleted,0),content_edit,COALESCE(content_edit_hash,'') FROM message_extra WHERE message_id=?"+p.lockSuffix(ctx),
		[]any{messageID}, &e.version, &e.revoke, &e.deleted, &e.edit, &e.hash)
	if errors.Is(err, sql.ErrNoRows) {
		return serverExtraRow{}, nil
	}
	if err != nil {
		return serverExtraRow{}, ErrUnavailable
	}
	e.exists = true
	return e, nil
}

func (e serverExtraRow) nativeVersion() string {
	h := e.hash
	if h == "" || strings.ContainsAny(h, "-") {
		h = "0"
	}
	for _, c := range h {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '_') {
			h = "0"
			break
		}
	}
	return "x" + strconv.FormatInt(e.version, 10) + "_" + h
}

func (p *ServerMessageFactProvider) groupFacts(ctx context.Context, groupNo string) (exists bool, status int, spaceID string, err error) {
	e := p.one(ctx, "SELECT COALESCE(space_id,''),status FROM `group` WHERE group_no=?"+p.lockSuffix(ctx), []any{groupNo}, &spaceID, &status)
	if errors.Is(e, sql.ErrNoRows) {
		return false, 0, "", nil
	}
	if e != nil {
		return false, 0, "", ErrUnavailable
	}
	return true, status, spaceID, nil
}

func (p *ServerMessageFactProvider) activeMember(ctx context.Context, groupNo, uid string) (bool, error) {
	var n int
	if err := p.one(ctx, "SELECT COUNT(*) FROM group_member WHERE group_no=? AND uid=? AND COALESCE(is_deleted,0)=0 AND COALESCE(status,1)=1"+p.lockSuffix(ctx), []any{groupNo, uid}, &n); err != nil {
		return false, ErrUnavailable
	}
	return n > 0, nil
}

func (p *ServerMessageFactProvider) userHidden(ctx context.Context, r serverRef, m *serverMsgRow, uid, dmPeerUID string) (bool, error) {
	var deleted int
	err := p.one(ctx, "SELECT COALESCE(message_is_deleted,0) FROM "+p.hooks.MessageUserExtraTable(uid)+" WHERE uid=? AND message_id=?"+p.lockSuffix(ctx), []any{uid, r.messageID}, &deleted)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, ErrUnavailable
	}
	if err == nil && deleted == 1 {
		return true, nil
	}
	offKey := r.channelID
	if r.channelType == channelTypePerson {
		offKey = dmPeerUID
	}
	var off sql.NullInt64
	err = p.one(ctx, "SELECT MAX(message_seq) FROM "+p.hooks.ChannelOffsetTable(uid)+" WHERE uid=? AND channel_id=? AND channel_type=?"+p.lockSuffix(ctx), []any{uid, offKey, int(r.channelType)}, &off)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, ErrUnavailable
	}
	if off.Valid && int64(m.seq) <= off.Int64 {
		return true, nil
	}
	var chOff sql.NullInt64
	err = p.one(ctx, "SELECT offset_message_seq FROM channel_setting WHERE channel_id=? AND channel_type=?"+p.lockSuffix(ctx), []any{r.channelID, int(r.channelType)}, &chOff)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, ErrUnavailable
	}
	if chOff.Valid && chOff.Int64 > 0 && int64(m.seq) <= chOff.Int64 {
		return true, nil
	}
	return false, nil
}

// resolved is the per-reference outcome: either an unavailable reason or a
// delivery with explicit unknown flags.
type serverResolution struct {
	unavailable string
	delivery    IMResolvedDelivery
	payloadHash string
}

func (p *ServerMessageFactProvider) resolveOne(ctx context.Context, key Key, ref serverRef) (serverResolution, error) {
	un := func(reason string) (serverResolution, error) { return serverResolution{unavailable: reason}, nil }

	var sourceKind, sourceID, parentGroup, dmPeerUID string
	var parentPtr *string
	spaceFromSource := ""
	sourceSpaceKnown := false
	switch ref.channelType {
	case channelTypePerson:
		peer, err := dmPeer(key, ref.channelID)
		if err != nil {
			return un("predicate_unsupported")
		}
		sourceKind, sourceID, dmPeerUID = "dm", ref.channelID, peer
	case channelTypeGroup:
		exists, status, sid, err := p.groupFacts(ctx, ref.channelID)
		if err != nil {
			return serverResolution{}, err
		}
		if !exists || (status != serverGroupStatusNormal && status != serverGroupStatusDisband) {
			return un("expired_reference")
		}
		sourceKind, sourceID, spaceFromSource, sourceSpaceKnown = "group", ref.channelID, sid, true
		parentGroup = ref.channelID
	case channelTypeThread:
		parts := threadID.FindStringSubmatch(ref.channelID)
		if parts == nil {
			return un("predicate_unsupported")
		}
		exists, status, sid, err := p.groupFacts(ctx, parts[1])
		if err != nil {
			return serverResolution{}, err
		}
		if !exists || (status != serverGroupStatusNormal && status != serverGroupStatusDisband) {
			return un("parent_unavailable")
		}
		var tstatus int
		e := p.one(ctx, "SELECT status FROM thread WHERE group_no=? AND short_id=?"+p.lockSuffix(ctx), []any{parts[1], parts[2]}, &tstatus)
		if errors.Is(e, sql.ErrNoRows) || (e == nil && tstatus == serverThreadDeleted) {
			return un("expired_reference")
		}
		if e != nil {
			return serverResolution{}, ErrUnavailable
		}
		parentGroup = parts[1]
		g := parts[1]
		parentPtr = &g
		sourceKind, sourceID, spaceFromSource, sourceSpaceKnown = "thread", ref.channelID, sid, true
	default:
		return un("predicate_unsupported")
	}

	msg, err := p.loadMessage(ctx, ref)
	if err != nil {
		return serverResolution{}, err
	}
	if msg == nil {
		// Row absent: never persisted (no_persist) or not yet written. Do not
		// guess which; the reference is non-serving.
		return un("mirror_lag")
	}
	extra, err := p.loadExtra(ctx, ref.messageID)
	if err != nil {
		return serverResolution{}, err
	}
	if msg.deleted != 0 || extra.revoke != 0 || extra.deleted != 0 {
		return un("expired_reference")
	}

	var robot int
	e := p.one(ctx, "SELECT COALESCE(robot,0) FROM user WHERE uid=?"+p.lockSuffix(ctx), []any{msg.fromUID}, &robot)
	if errors.Is(e, sql.ErrNoRows) {
		return un("predicate_unsupported")
	}
	if e != nil {
		return serverResolution{}, ErrUnavailable
	}
	authorKind := "human"
	switch {
	case p.hooks.IsSystemUID(msg.fromUID):
		authorKind = "system"
	case robot != 0:
		authorKind = "user_bot"
	}

	curVersion := extra.nativeVersion()
	body := msg.payload
	if extra.edit.Valid {
		body = []byte(extra.edit.String)
	}
	sum := sha256.Sum256(body)
	payloadHash := hex.EncodeToString(sum[:])

	d := IMResolvedDelivery{Status: "resolved"}
	d.Header = IMHeaderRecord{
		MessageID:            ref.messageID,
		Source:               IMHeaderSource{Kind: sourceKind, ID: sourceID},
		ParentGroup:          parentPtr,
		SpaceID:              key.SpaceID,
		AuthorUID:            msg.fromUID,
		AuthorKind:           authorKind,
		RecipientBotUID:      key.BotUID,
		NativeMessageVersion: curVersion,
		PayloadSHA256:        payloadHash,
		Delivery: IMDeliveryMetadata{
			StreamID:          serverMessageStreamID,
			StreamIncarnation: p.incarnation,
			Sequence:          strconv.FormatUint(uint64(msg.seq), 10),
		},
	}
	hr := ref
	hr.version = curVersion
	d.Header.HeaderReference = encodeServerRef(serverRefHeader, hr)
	d.MessageSourceExact = true
	d.MessageAuthorExact = msg.fromUID != ""

	// Version: only exact when the reference carries the observed version.
	if ref.version == "" {
		d.MessageVersionUnknown = true
	} else {
		d.MessageVersionExact = ref.version == curVersion
	}

	// Space.
	if sourceSpaceKnown {
		d.MessageSpaceExact = spaceFromSource == key.SpaceID
		d.Header.SpaceID = spaceFromSource
	} else {
		switch tag := p.hooks.PayloadSpaceID(msg.payload); {
		case tag == "":
			d.MessageSpaceUnknown = true
		case tag == key.SpaceID:
			d.MessageSpaceExact = true
		default:
			d.Header.SpaceID = tag
		}
	}

	authorUID, botUID := msg.fromUID, key.BotUID
	switch ref.channelType {
	case channelTypePerson:
		d.MessageRecipientExact = true
		authorIsParticipant := authorUID == dmPeerUID || authorUID == botUID
		authorHidden, err1 := p.userHidden(ctx, ref, msg, authorUID, otherOf(ref.channelID, authorUID))
		botHidden, err2 := p.userHidden(ctx, ref, msg, botUID, otherOf(ref.channelID, botUID))
		if err1 != nil || err2 != nil {
			return serverResolution{}, ErrUnavailable
		}
		callerVisible := p.hooks.PayloadVisibleTo(msg.payload, authorUID)
		botVisible := p.hooks.PayloadVisibleTo(msg.payload, botUID)
		d.MessageAuthorExact = d.MessageAuthorExact && authorIsParticipant
		d.CallerCurrentRead = authorIsParticipant && callerVisible && !authorHidden
		d.BotCurrentRead = botVisible && !botHidden
		d.CallerOriginalVisible = authorIsParticipant && callerVisible
		d.BotOriginalVisible = botVisible
		d.BotCurrentSend = true // relation checks are evaluated by the SQL adapter
	default:
		botMember, err := p.activeMember(ctx, parentGroup, botUID)
		if err != nil {
			return serverResolution{}, err
		}
		authorMember, err := p.activeMember(ctx, parentGroup, authorUID)
		if err != nil {
			return serverResolution{}, err
		}
		authorHidden, err1 := p.userHidden(ctx, ref, msg, authorUID, "")
		botHidden, err2 := p.userHidden(ctx, ref, msg, botUID, "")
		if err1 != nil || err2 != nil {
			return serverResolution{}, ErrUnavailable
		}
		callerVisible := p.hooks.PayloadVisibleTo(msg.payload, authorUID)
		botVisible := p.hooks.PayloadVisibleTo(msg.payload, botUID)
		d.MessageRecipientExact = botMember
		d.CallerCurrentRead = authorMember && callerVisible && !authorHidden
		d.BotCurrentRead = botMember && botVisible && !botHidden
		d.CallerParentCurrentRead = authorMember
		d.BotParentCurrentRead = botMember
		// Original visibility needs membership history that octo-server does
		// not keep. A visibles exclusion is a write-time fact and is provably
		// false; otherwise the value is unknown, never true.
		if !callerVisible {
			d.CallerOriginalVisible = false
		} else {
			d.CallerOriginalVisibleUnknown = true
		}
		if !botVisible {
			d.BotOriginalVisible = false
		} else {
			d.BotOriginalVisibleUnknown = true
		}
		_, st, _, gerr := p.groupFacts(ctx, parentGroup)
		if gerr != nil {
			return serverResolution{}, gerr
		}
		d.BotCurrentSend = botMember && st == serverGroupStatusNormal
	}
	return serverResolution{delivery: d, payloadHash: payloadHash}, nil
}

func otherOf(fakeChannelID, uid string) string {
	p := strings.Split(fakeChannelID, "@")
	if len(p) != 2 {
		return ""
	}
	if p[0] == uid {
		return p[1]
	}
	if p[1] == uid {
		return p[0]
	}
	return ""
}

func (p *ServerMessageFactProvider) InspectDeliveredHeadersV1(ctx context.Context, key Key, purpose string, deliveryRefs []string) ([]IMResolvedDelivery, IMAuthorityCut, error) {
	if p == nil || !key.Valid() || (purpose != "admit" && purpose != "contributor" && purpose != "reply") {
		return nil, IMAuthorityCut{}, ErrUnavailable
	}
	started := p.now().UTC().Truncate(time.Microsecond)
	out := make([]IMResolvedDelivery, 0, len(deliveryRefs))
	for _, ref := range deliveryRefs {
		r, ok := decodeServerRef(serverRefDelivery, ref)
		if !ok {
			out = append(out, IMResolvedDelivery{Reference: ref, Status: "unavailable", Reason: "predicate_unsupported"})
			continue
		}
		res, err := p.resolveOne(ctx, key, r)
		if err != nil {
			return nil, IMAuthorityCut{}, err
		}
		if res.unavailable != "" {
			out = append(out, IMResolvedDelivery{Reference: ref, Status: "unavailable", Reason: res.unavailable})
			continue
		}
		res.delivery.Reference = ref
		out = append(out, res.delivery)
	}
	cut, err := p.cut(started)
	return out, cut, err
}

func (p *ServerMessageFactProvider) ValidateIMCut(_ context.Context, _ Key, authorityID, incarnation, position, readNonce string) error {
	if p == nil || authorityID != p.authorityID || incarnation != p.incarnation || !validHex(readNonce, 32) {
		return ErrUnavailable
	}
	if _, err := strconv.ParseUint(position, 10, 64); err != nil {
		return ErrUnavailable
	}
	return nil
}

// ListHistoryHeadersV1 lists metadata headers for one source, newest first,
// with a message_seq cursor. Pages are stable against appends; concurrent edit,
// revoke or clear-history can change later pages, so this is not an immutable
// cut. Items the bot cannot currently read are returned as unavailable.
func (p *ServerMessageFactProvider) ListHistoryHeadersV1(ctx context.Context, key Key, sourceRef string, cursor *string, pageSize uint64) (IMHistoryHeadersPage, error) {
	if p == nil || !key.Valid() || pageSize == 0 || pageSize > 32 {
		return IMHistoryHeadersPage{}, ErrUnavailable
	}
	src, ok := decodeServerRef(serverRefSource, sourceRef+"-0")
	if !ok {
		return IMHistoryHeadersPage{}, ErrUnavailable
	}
	started := p.now().UTC().Truncate(time.Microsecond)
	upper := int64(1<<62 - 1)
	if cursor != nil {
		v, err := strconv.ParseInt(strings.TrimPrefix(*cursor, "q"), 10, 64)
		if err != nil || !strings.HasPrefix(*cursor, "q") || v <= 0 {
			return IMHistoryHeadersPage{}, ErrUnavailable
		}
		upper = v
	}
	ctx2, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	rows, err := p.query(ctx2, "SELECT message_id,message_seq FROM "+p.hooks.MessageTable(src.channelID)+" WHERE channel_id=? AND channel_type=? AND message_seq<? AND COALESCE(is_deleted,0)=0 ORDER BY message_seq DESC LIMIT ?"+p.lockSuffix(ctx),
		src.channelID, int(src.channelType), upper, int(pageSize)+1)
	if err != nil {
		return IMHistoryHeadersPage{}, ErrUnavailable
	}
	type idSeq struct {
		id  string
		seq int64
	}
	var ids []idSeq
	for rows.Next() {
		var s idSeq
		if err := rows.Scan(&s.id, &s.seq); err != nil {
			rows.Close()
			return IMHistoryHeadersPage{}, ErrUnavailable
		}
		ids = append(ids, s)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return IMHistoryHeadersPage{}, ErrUnavailable
	}
	rows.Close()
	end := len(ids) <= int(pageSize)
	if !end {
		ids = ids[:pageSize]
	}
	page := IMHistoryHeadersPage{SourceReference: sourceRef, End: end}
	for _, s := range ids {
		r := src
		r.messageID = s.id
		res, err := p.resolveOne(ctx, key, r)
		if err != nil {
			return IMHistoryHeadersPage{}, err
		}
		itemRef := encodeServerRef(serverRefDelivery, r)
		if res.unavailable != "" || !res.delivery.BotCurrentRead {
			reason := res.unavailable
			if reason == "" {
				reason = "expired_reference"
			}
			page.Items = append(page.Items, IMHistoryHeaderItem{Status: "unavailable", Reference: itemRef, Reason: reason})
			continue
		}
		page.Items = append(page.Items, IMHistoryHeaderItem{Status: "resolved", Reference: itemRef, Header: res.delivery.Header})
	}
	if !end && len(ids) > 0 {
		next := "q" + strconv.FormatInt(ids[len(ids)-1].seq, 10)
		page.NextCursor = &next
	}
	cu, err := p.cut(started)
	if err != nil {
		return IMHistoryHeadersPage{}, err
	}
	page.Cut = cu
	idSum := sha256.Sum256([]byte(sourceRef + "|" + key.BotUID + "|" + strconv.FormatInt(upper, 10) + "|" + cu.Position))
	page.PageID = "p" + hex.EncodeToString(idSum[:16])
	return page, nil
}

func (p *ServerMessageFactProvider) permit(key Key, contextID, contextEpoch, headerRef, payloadSHA string) string {
	m := hmac.New(sha256.New, p.permitKey)
	for _, part := range []string{key.BotUID, key.SpaceID, contextID, contextEpoch, headerRef, payloadSHA} {
		m.Write([]byte(strconv.Itoa(len(part))))
		m.Write([]byte{':'})
		m.Write([]byte(part))
	}
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

func permitSHA256Hex(permit string) string {
	sum := sha256.Sum256([]byte(permit))
	return hex.EncodeToString(sum[:])
}

func (p *ServerMessageFactProvider) isPermitUsedInMemory(permit string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, used := p.usedPmt[permit]
	return used
}

func (p *ServerMessageFactProvider) ensurePermitIssued(ctx context.Context, key Key, contextID, contextEpoch, headerRef, payloadSHA, permit string) (bool, error) {
	if p.isPermitUsedInMemory(permit) {
		return false, nil
	}
	if !p.durablePermits {
		return true, nil
	}
	pmtHash := permitSHA256Hex(permit)
	var consumedAt sql.NullInt64
	err := p.one(ctx, "SELECT consumed_at FROM "+ServerDisclosurePermitTable+" WHERE permit_sha256=?"+p.lockSuffix(ctx), []any{pmtHash}, &consumedAt)
	if err == nil {
		if consumedAt.Valid {
			return false, nil
		}
		return true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, ErrUnavailable
	}
	issuedAt := p.now().UTC().UnixNano()
	if _, err := p.exec(ctx, "INSERT INTO "+ServerDisclosurePermitTable+" (permit_sha256, bot_uid, space_id, context_id, context_epoch, header_reference, payload_sha256, issued_at, consumed_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, NULL)",
		pmtHash, key.BotUID, key.SpaceID, contextID, contextEpoch, headerRef, payloadSHA, issuedAt); err != nil {
		return false, ErrUnavailable
	}
	return true, nil
}

// IssueHistoryDisclosureV1 is step two of the existing two-step disclosure: it
// re-resolves each header reference against current storage and issues a
// permit only for references that are current, version-exact and whose bot
// current/original visibility is proven. It never returns a body; the permit
// is only an authorization token for a later body retrieval step. This does
// not implement or activate any atomic header-plus-body substitute.
func (p *ServerMessageFactProvider) IssueHistoryDisclosureV1(ctx context.Context, key Key, contextID, contextEpoch string, headerRefs []string) ([]IMDisclosureItem, IMAuthorityCut, error) {
	if p == nil || !key.Valid() || contextID == "" || contextEpoch == "" {
		return nil, IMAuthorityCut{}, ErrUnavailable
	}
	started := p.now().UTC().Truncate(time.Microsecond)
	out := make([]IMDisclosureItem, 0, len(headerRefs))
	for _, hr := range headerRefs {
		r, ok := decodeServerRef(serverRefHeader, hr)
		if !ok || r.version == "" {
			out = append(out, IMDisclosureItem{Reference: hr, Status: "unavailable", Reason: "predicate_unsupported"})
			continue
		}
		res, err := p.resolveOne(ctx, key, r)
		if err != nil {
			return nil, IMAuthorityCut{}, err
		}
		d := res.delivery
		switch {
		case res.unavailable != "":
			out = append(out, IMDisclosureItem{Reference: hr, Status: "unavailable", Reason: res.unavailable})
		case !d.MessageVersionExact:
			out = append(out, IMDisclosureItem{Reference: hr, Status: "unavailable", Reason: "expired_reference"})
		case d.MessageSpaceUnknown || d.BotOriginalVisibleUnknown:
			out = append(out, IMDisclosureItem{Reference: hr, Status: "unavailable", Reason: "disclosure_unavailable"})
		case !d.MessageSpaceExact || !d.MessageSourceExact || !d.BotCurrentRead || !d.BotOriginalVisible:
			out = append(out, IMDisclosureItem{Reference: hr, Status: "unavailable", Reason: "disclosure_unavailable"})
		default:
			pmt := p.permit(key, contextID, contextEpoch, hr, res.payloadHash)
			avail, err := p.ensurePermitIssued(ctx, key, contextID, contextEpoch, hr, res.payloadHash, pmt)
			if err != nil {
				return nil, IMAuthorityCut{}, err
			}
			if !avail {
				out = append(out, IMDisclosureItem{Reference: hr, Status: "unavailable", Reason: "disclosure_unavailable"})
				continue
			}
			out = append(out, IMDisclosureItem{
				Reference: hr, Status: "resolved", HeaderReference: hr, PayloadSHA256: res.payloadHash,
				Permit: pmt,
			})
		}
	}
	cut, err := p.cut(started)
	return out, cut, err
}

// ConsumeDisclosurePermit verifies a permit and marks it used. When
// DurablePermitLedger is enabled, consumption is atomically persisted in
// bot_permission_disclosure_permit so a consumed permit cannot be replayed
// after a process restart; otherwise it falls back to the in-memory used-set.
func (p *ServerMessageFactProvider) ConsumeDisclosurePermit(key Key, contextID, contextEpoch, headerRef, payloadSHA, permit string) error {
	return p.ConsumeDisclosurePermitContext(context.Background(), key, contextID, contextEpoch, headerRef, payloadSHA, permit)
}

func (p *ServerMessageFactProvider) ConsumeDisclosurePermitContext(ctx context.Context, key Key, contextID, contextEpoch, headerRef, payloadSHA, permit string) error {
	if p == nil {
		return ErrUnavailable
	}
	want := p.permit(key, contextID, contextEpoch, headerRef, payloadSHA)
	if !hmac.Equal([]byte(want), []byte(permit)) {
		return ErrReference
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, used := p.usedPmt[permit]; used {
		return ErrReference
	}
	if p.durablePermits {
		pmtHash := permitSHA256Hex(permit)
		consumedAt := p.now().UTC().UnixNano()
		res, err := p.exec(ctx, "UPDATE "+ServerDisclosurePermitTable+" SET consumed_at=? WHERE permit_sha256=? AND bot_uid=? AND space_id=? AND context_id=? AND context_epoch=? AND header_reference=? AND payload_sha256=? AND consumed_at IS NULL",
			consumedAt, pmtHash, key.BotUID, key.SpaceID, contextID, contextEpoch, headerRef, payloadSHA)
		if err != nil {
			return ErrUnavailable
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return ErrUnavailable
		}
		if affected != 1 {
			return ErrReference
		}
	}
	p.usedPmt[permit] = struct{}{}
	return nil
}

// BindDeliveredMessageV1 binds an inbound delivery frame's trusted ingress
// evidence (channel_type, channel_id, message_id, message_seq, from_uid,
// observed_payload_sha256, and optional expected_native_version) against actual
// octo-server message storage.
//
// It never accepts a current-version lookup by message_id alone as proof of
// the delivered version: if the stored row is missing, deleted, revoked, has a
// mismatched sender or sequence, or has a different effective payload SHA-256
// (e.g. due to a concurrent edit in message_extra), it explicitly refuses with
// a deterministic reason and never releases any message, quote, or media body.
func (p *ServerMessageFactProvider) BindDeliveredMessageV1(ctx context.Context, key Key, req DeliveryBindingRequestV1) (DeliveryBindingResultV1, IMAuthorityCut, error) {
	if p == nil || !key.Valid() {
		return DeliveryBindingResultV1{}, IMAuthorityCut{}, ErrUnavailable
	}
	started := p.now().UTC().Truncate(time.Microsecond)
	ref := serverRef{
		kind:        serverRefDelivery,
		channelType: req.ChannelType,
		channelID:   req.ChannelID,
		messageID:   req.MessageID,
	}
	res, err := p.resolveOne(ctx, key, ref)
	if err != nil {
		return DeliveryBindingResultV1{}, IMAuthorityCut{}, err
	}
	cut, err := p.cut(started)
	if err != nil {
		return DeliveryBindingResultV1{}, IMAuthorityCut{}, err
	}
	if res.unavailable != "" {
		return DeliveryBindingResultV1{
			Status:  "refused",
			Reason:  res.unavailable,
			Current: false,
		}, cut, nil
	}
	d := res.delivery
	curVersion := d.Header.NativeMessageVersion
	if d.Header.AuthorUID != req.FromUID || d.Header.Delivery.Sequence != req.MessageSeq || !d.MessageAuthorExact {
		return DeliveryBindingResultV1{
			Status:               "refused",
			Reason:               "identity_mismatch",
			Current:              false,
			NativeMessageVersion: curVersion,
		}, cut, nil
	}
	if !strings.EqualFold(res.payloadHash, req.ObservedPayloadSHA256) {
		return DeliveryBindingResultV1{
			Status:               "refused",
			Reason:               "payload_digest_mismatch",
			Current:              false,
			NativeMessageVersion: curVersion,
		}, cut, nil
	}
	if req.ExpectedNativeVersion != "" && req.ExpectedNativeVersion != curVersion {
		return DeliveryBindingResultV1{
			Status:               "refused",
			Reason:               "version_mismatch",
			Current:              false,
			NativeMessageVersion: curVersion,
		}, cut, nil
	}
	if (!d.MessageSpaceUnknown && !d.MessageSpaceExact) || !d.BotCurrentRead || !d.MessageRecipientExact {
		return DeliveryBindingResultV1{
			Status:               "refused",
			Reason:               "visibility_denied",
			Current:              false,
			NativeMessageVersion: curVersion,
		}, cut, nil
	}

	proven := map[string]bool{
		"message_source_exact":    d.MessageSourceExact,
		"message_author_exact":    d.MessageAuthorExact,
		"message_recipient_exact": d.MessageRecipientExact,
		"message_version_exact":   true,
		"caller_current_read":     d.CallerCurrentRead,
		"bot_current_read":        d.BotCurrentRead,
		"bot_current_send":        d.BotCurrentSend,
	}
	if req.ChannelType != channelTypePerson {
		proven["caller_parent_current_read"] = d.CallerParentCurrentRead
		proven["bot_parent_current_read"] = d.BotParentCurrentRead
	}
	var unknowns []string
	if d.MessageSpaceUnknown {
		unknowns = append(unknowns, "message_space_exact")
	} else {
		proven["message_space_exact"] = d.MessageSpaceExact
	}
	if d.CallerOriginalVisibleUnknown {
		unknowns = append(unknowns, "caller_original_visible")
	} else {
		proven["caller_original_visible"] = d.CallerOriginalVisible
	}
	if d.BotOriginalVisibleUnknown {
		unknowns = append(unknowns, "bot_original_visible")
	} else {
		proven["bot_original_visible"] = d.BotOriginalVisible
	}
	sort.Strings(unknowns)

	var purposes []string
	if !d.MessageSpaceUnknown && d.MessageSpaceExact {
		if !d.CallerOriginalVisibleUnknown && !d.BotOriginalVisibleUnknown {
			purposes = []string{"admit", "contributor", "reply"}
		} else {
			purposes = []string{"reply"}
		}
	}

	deliveryRef := ServerDeliveryReference(req.ChannelType, req.ChannelID, req.MessageID, curVersion)
	return DeliveryBindingResultV1{
		Status:                "bound",
		Current:               true,
		DeliveryReference:     deliveryRef,
		HeaderReference:       d.Header.HeaderReference,
		NativeMessageVersion:  curVersion,
		ObservedPayloadSHA256: res.payloadHash,
		Header:                d.Header,
		ProvenPredicates:      proven,
		UnknownPredicates:     unknowns,
		ResolvablePurposes:    purposes,
	}, cut, nil
}

// ServerSourceReference builds the history source token for one channel.
func ServerSourceReference(channelType uint8, channelID string) string {
	return strings.TrimSuffix(encodeServerRef(serverRefSource, serverRef{channelType: channelType, channelID: channelID, messageID: "0"}), "-0")
}

var (
	_ NativeIMFactProvider          = (*ServerMessageFactProvider)(nil)
	_ NativeDeliveryReferenceBinder = (*ServerMessageFactProvider)(nil)
)
