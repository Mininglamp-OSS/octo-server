package permissionbundle

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gocraft/dbr/v2"
)

const NativeFactProviderModeIMV1 = "native_im_fact_provider_v1"

// NativeFactProviderModeServerStoreV1 selects the server-only provider that
// reads octo-server's own message storage (ServerMessageFactProvider). It is
// accepted on evidence from that storage, not by name.
const NativeFactProviderModeServerStoreV1 = "octo_server_message_store_v1"

// RejectPlainMessageSyncFactSource rejects content-first message sync and
// webhook mirror substitutes that cannot prove header-only inspection,
// delivery-stream incarnation, or historical/current visibility cuts.
func RejectPlainMessageSyncFactSource(mode string) error {
	normalized := strings.ToLower(strings.TrimSpace(mode))
	switch normalized {
	case NativeFactProviderModeIMV1, NativeFactProviderModeServerStoreV1:
		return nil
	case "", "none", "disabled":
		return fmt.Errorf("%w: native fact provider mode not configured", ErrUnavailable)
	case "im_sync_channel_message", "plain_message_sync", "bot_api_sync", "messages_sync":
		return fmt.Errorf("%w: plain message sync (%s) returns content-first uint32 payloads and does not prove header-only inspection or original visibility", ErrUnavailable, mode)
	case "webhook_mirror", "message_mirror", "sql_message_mirror":
		return fmt.Errorf("%w: webhook or separate SQL message mirror (%s) is a copy that cannot establish authoritative delivery headers or original visibility; use octo_server_message_store_v1 to read the server message storage itself", ErrUnavailable, mode)
	default:
		return fmt.Errorf("%w: unsupported native fact provider mode %q", ErrUnavailable, mode)
	}
}

type IMAuthorityCut struct {
	AuthorityID string
	Incarnation string
	Position    string
	ReadNonce   string
	StartedAt   time.Time
	CompletedAt time.Time
}

type IMDeliveryMetadata struct {
	StreamID          string `json:"stream_id"`
	StreamIncarnation string `json:"stream_incarnation"`
	Sequence          string `json:"sequence"`
}

type IMHeaderSource struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

type IMHeaderRecord struct {
	HeaderReference      string             `json:"header_reference"`
	MessageID            string             `json:"message_id"`
	Source               IMHeaderSource     `json:"source"`
	ParentGroup          *string            `json:"parent_group"`
	SpaceID              string             `json:"space_id"`
	AuthorUID            string             `json:"author_uid"`
	AuthorKind           string             `json:"author_kind"`
	RecipientBotUID      string             `json:"recipient_bot_uid"`
	NativeMessageVersion string             `json:"native_message_version"`
	PayloadSHA256        string             `json:"payload_sha256"`
	Delivery             IMDeliveryMetadata `json:"delivery"`
}

func (h IMHeaderRecord) toMap() map[string]any {
	var parent any
	if h.ParentGroup != nil {
		parent = *h.ParentGroup
	}
	return map[string]any{
		"header_version":         json.Number("1"),
		"header_reference":       h.HeaderReference,
		"message_id":             h.MessageID,
		"source":                 map[string]any{"kind": h.Source.Kind, "id": h.Source.ID},
		"parent_group":           parent,
		"space_id":               h.SpaceID,
		"author_uid":             h.AuthorUID,
		"author_kind":            h.AuthorKind,
		"recipient_bot_uid":      h.RecipientBotUID,
		"native_message_version": h.NativeMessageVersion,
		"payload_sha256":         h.PayloadSHA256,
		"delivery": map[string]any{
			"stream_id":          h.Delivery.StreamID,
			"stream_incarnation": h.Delivery.StreamIncarnation,
			"sequence":           h.Delivery.Sequence,
		},
	}
}

type IMResolvedDelivery struct {
	Reference               string
	Status                  string
	Reason                  string
	Header                  IMHeaderRecord
	MessageSourceExact      bool
	MessageSpaceExact       bool
	MessageAuthorExact      bool
	MessageRecipientExact   bool
	MessageVersionExact     bool
	CallerCurrentRead       bool
	BotCurrentRead          bool
	CallerOriginalVisible   bool
	BotOriginalVisible      bool
	CallerParentCurrentRead bool
	BotParentCurrentRead    bool
	BotCurrentSend          bool

	// Explicit unknowns: set by providers whose storage cannot prove the value
	// for this reference. A reference with an applicable unknown is returned as
	// unavailable (non-serving); the value is never treated as true.
	MessageSpaceUnknown          bool
	MessageVersionUnknown        bool
	CallerOriginalVisibleUnknown bool
	BotOriginalVisibleUnknown    bool
}

// unknownReason reports whether an unproven value applies to the purpose.
// Original visibility is only consulted by non-reply purposes.
func (d IMResolvedDelivery) unknownReason(purpose string) string {
	if d.MessageSpaceUnknown || d.MessageVersionUnknown {
		return "predicate_unsupported"
	}
	if purpose != "reply" && (d.CallerOriginalVisibleUnknown || d.BotOriginalVisibleUnknown) {
		return "predicate_unsupported"
	}
	return ""
}

type IMHistoryHeaderItem struct {
	Status    string
	Reference string
	Reason    string
	Header    IMHeaderRecord
}

type IMHistoryHeadersPage struct {
	SourceReference string
	PageID          string
	Items           []IMHistoryHeaderItem
	NextCursor      *string
	End             bool
	Cut             IMAuthorityCut
}

type IMDisclosureItem struct {
	Reference       string
	Status          string
	Reason          string
	Permit          string
	HeaderReference string
	PayloadSHA256   string
}

// NativeIMFactProvider is the concrete seam for authoritative IM header-only
// inspection, original/current visibility facts, immutable-cut history header
// pagination, and one-use history disclosure permits.
type NativeIMFactProvider interface {
	InspectDeliveredHeadersV1(ctx context.Context, key Key, purpose string, deliveryRefs []string) ([]IMResolvedDelivery, IMAuthorityCut, error)
	ListHistoryHeadersV1(ctx context.Context, key Key, sourceRef string, cursor *string, pageSize uint64) (IMHistoryHeadersPage, error)
	IssueHistoryDisclosureV1(ctx context.Context, key Key, contextID, contextEpoch string, headerRefs []string) ([]IMDisclosureItem, IMAuthorityCut, error)
	ValidateIMCut(ctx context.Context, key Key, authorityID, incarnation, position, readNonce string) error
}

type sqlCutVerifier interface {
	VerifySQLAuthorityCut(authorityID, incarnation, position string) error
}

type sqlSessionMetadata interface {
	AuthorityID() string
	Position() uint64
}

// SQLNativeFactAdapter implements NativeFactSource by combining locking SQL
// principal/space/channel/relation predicates under the caller's RepeatableRead
// transaction with authoritative IM header/visibility/disclosure facts.
type SQLNativeFactAdapter struct {
	Authority   FencedAuthority
	IMProvider  NativeIMFactProvider
	SQLID       string
	testNoLocks bool
}

func NewSQLNativeFactAdapter(authority FencedAuthority, imProvider NativeIMFactProvider) (*SQLNativeFactAdapter, error) {
	if authority == nil || imProvider == nil {
		return nil, ErrUnavailable
	}
	return &SQLNativeFactAdapter{
		Authority:  authority,
		IMProvider: imProvider,
		SQLID:      "sql-primary",
	}, nil
}

func (a *SQLNativeFactAdapter) suffix(absence bool) string {
	if a.testNoLocks {
		return ""
	}
	if absence {
		return " FOR SHARE"
	}
	return " FOR SHARE NOWAIT"
}

func randomHex32() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", ErrUnavailable
	}
	return hex.EncodeToString(b[:]), nil
}

func formatAuthorityDescriptor(authorityID, kind, incarnation, position, nonce string, started, completed time.Time) map[string]any {
	if completed.Before(started) {
		completed = started
	}
	return map[string]any{
		"authority_id": authorityID,
		"kind":         kind,
		"incarnation":  incarnation,
		"position":     position,
		"read_nonce":   nonce,
		"consistency":  "ack_before_read_start",
		"started_at":   started.UTC().Format(time.RFC3339Nano),
		"completed_at": completed.UTC().Format(time.RFC3339Nano),
	}
}

func sortAuthoritiesAndObserved(sqlAuth, imAuth map[string]any, sqlCompleted, imCompleted time.Time) ([]any, string) {
	observed := sqlCompleted
	if imCompleted.After(observed) {
		observed = imCompleted
	}
	items := []map[string]any{sqlAuth, imAuth}
	sort.Slice(items, func(i, j int) bool {
		return bytes.Compare(canonical(items[i]), canonical(items[j])) < 0
	})
	return []any{items[0], items[1]}, observed.UTC().Format(time.RFC3339Nano)
}

func (a *SQLNativeFactAdapter) Read(ctx context.Context, tx *dbr.Tx, key Key, requestBytes []byte) ([]byte, error) {
	if a == nil || a.Authority == nil || a.IMProvider == nil || tx == nil || !key.Valid() {
		return nil, ErrUnavailable
	}
	req, _, err := nativeRequest(bytes.NewReader(requestBytes))
	if err != nil {
		return nil, err
	}
	sqlStarted := time.Now().UTC().Truncate(time.Microsecond)
	authRead, err := a.Authority.Begin(ctx, tx)
	if err != nil || authRead == nil || !validHex(authRead.Incarnation(), 32) {
		return nil, ErrUnavailable
	}
	sqlID := a.SQLID
	if sqlID == "" {
		sqlID = "sql-primary"
	}
	sqlPos := "1"
	if meta, ok := authRead.(sqlSessionMetadata); ok {
		if id := meta.AuthorityID(); id != "" {
			sqlID = id
		}
		sqlPos = strconv.FormatUint(meta.Position(), 10)
	}
	sqlNonce, err := randomHex32()
	if err != nil {
		return nil, err
	}
	readID, err := randomHex32()
	if err != nil {
		return nil, err
	}
	keyObj, err := asObject(key)
	if err != nil {
		return nil, ErrUnavailable
	}

	op, _ := req["operation"].(string)
	switch op {
	case "resolve":
		return a.readResolve(ctx, tx, key, keyObj, req, readID, sqlID, authRead.Incarnation(), sqlPos, sqlNonce, sqlStarted)
	case "history_headers":
		return a.readHistoryHeaders(ctx, tx, key, keyObj, req, readID, sqlID, authRead.Incarnation(), sqlPos, sqlNonce, sqlStarted)
	case "history_disclosure":
		return a.readHistoryDisclosure(ctx, tx, key, keyObj, req, readID, sqlID, authRead.Incarnation(), sqlPos, sqlNonce, sqlStarted)
	default:
		return nil, ErrInvalid
	}
}

type sqlScopeBaseFacts struct {
	botUserActive      bool
	botRobotActive     bool
	botCreatorEligible bool
	creatorUID         string
	spaceActive        bool
	botSpaceMember     bool
}

func (a *SQLNativeFactAdapter) evalScopeBaseFacts(ctx context.Context, tx *dbr.Tx, key Key) (sqlScopeBaseFacts, error) {
	var out sqlScopeBaseFacts
	var botUser nativeUser
	if err := queryOne(ctx, tx, `SELECT id,uid,status,COALESCE(is_destroy,0),robot FROM user WHERE uid=?`+a.suffix(false), []any{key.BotUID}, &botUser.id, &botUser.uid, &botUser.status, &botUser.destroy, &botUser.robot); err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return out, ErrUnavailable
		}
	} else if botUser.uid == key.BotUID && botUser.status == 1 && botUser.destroy != 2 && botUser.robot == 1 {
		out.botUserActive = true
	}

	var appID, appUID string
	appErr := queryOne(ctx, tx, `SELECT id,uid FROM app_bot WHERE uid=?`+a.suffix(true), []any{key.BotUID}, &appID, &appUID)
	if appErr != nil && !errors.Is(appErr, sql.ErrNoRows) {
		return out, ErrUnavailable
	}
	noBotApp := errors.Is(appErr, sql.ErrNoRows)

	var robotID, robotUID, creator string
	var robotStatus int
	if err := queryOne(ctx, tx, `SELECT id,robot_id,creator_uid,status FROM robot WHERE robot_id=?`+a.suffix(false), []any{key.BotUID}, &robotID, &robotUID, &creator, &robotStatus); err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return out, ErrUnavailable
		}
	} else if robotUID == key.BotUID && robotStatus == 1 && noBotApp {
		out.botRobotActive = true
		out.creatorUID = creator
	}

	var spaceRowID, spaceID string
	var spaceStatus int
	if err := queryOne(ctx, tx, `SELECT id,space_id,status FROM space WHERE space_id=?`+a.suffix(false), []any{key.SpaceID}, &spaceRowID, &spaceID, &spaceStatus); err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return out, ErrUnavailable
		}
	} else if spaceID == key.SpaceID && spaceStatus == 1 {
		out.spaceActive = true
	}

	okMember, err := a.isSpaceMember(ctx, tx, key.SpaceID, key.BotUID)
	if err != nil {
		return out, err
	}
	out.botSpaceMember = okMember

	if ValidNativeID(creator) {
		eligible, err := a.isEligibleHumanInSpace(ctx, tx, key.SpaceID, creator)
		if err != nil {
			return out, err
		}
		out.botCreatorEligible = eligible
	}
	return out, nil
}

func (a *SQLNativeFactAdapter) isSpaceMember(ctx context.Context, tx *dbr.Tx, spaceID, uid string) (bool, error) {
	var sid, canonical string
	var status int
	err := queryOne(ctx, tx, `SELECT space_id,uid,status FROM space_member WHERE space_id=? AND uid=?`+a.suffix(false), []any{spaceID, uid}, &sid, &canonical, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, ErrUnavailable
	}
	return sid == spaceID && canonical == uid && status == 1, nil
}

func (a *SQLNativeFactAdapter) isEligibleHumanInSpace(ctx context.Context, tx *dbr.Tx, spaceID, uid string) (bool, error) {
	var u nativeUser
	err := queryOne(ctx, tx, `SELECT id,uid,status,COALESCE(is_destroy,0),robot FROM user WHERE uid=?`+a.suffix(false), []any{uid}, &u.id, &u.uid, &u.status, &u.destroy, &u.robot)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, ErrUnavailable
	}
	if u.uid != uid || u.status != 1 || u.destroy == 2 || u.robot != 0 {
		return false, nil
	}
	var rID, rUID string
	var rStatus int
	err = queryOne(ctx, tx, `SELECT id,robot_id,status FROM robot WHERE robot_id=?`+a.suffix(true), []any{uid}, &rID, &rUID, &rStatus)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, ErrUnavailable
	}
	if err == nil && (rUID != uid || rStatus == 1) {
		return false, nil
	}
	var appID, appUID string
	err = queryOne(ctx, tx, `SELECT id,uid FROM app_bot WHERE uid=?`+a.suffix(true), []any{uid}, &appID, &appUID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, ErrUnavailable
	}
	if err == nil {
		return false, nil
	}
	return a.isSpaceMember(ctx, tx, spaceID, uid)
}

type sqlAuthorFacts struct {
	identitySupported  bool
	authorIsHuman      bool
	authorActive       bool
	authorNotDestroyed bool
	callerSpaceMember  bool
}

func (a *SQLNativeFactAdapter) evalAuthorFacts(ctx context.Context, tx *dbr.Tx, spaceID, authorUID string) (sqlAuthorFacts, error) {
	var out sqlAuthorFacts
	if !ValidNativeID(authorUID) {
		return out, nil
	}
	var u nativeUser
	err := queryOne(ctx, tx, `SELECT id,uid,status,COALESCE(is_destroy,0),robot FROM user WHERE uid=?`+a.suffix(false), []any{authorUID}, &u.id, &u.uid, &u.status, &u.destroy, &u.robot)
	if errors.Is(err, sql.ErrNoRows) {
		return out, nil
	}
	if err != nil {
		return out, ErrUnavailable
	}
	if u.uid != authorUID {
		return out, nil
	}
	var rID, rUID string
	var rStatus int
	rErr := queryOne(ctx, tx, `SELECT id,robot_id,status FROM robot WHERE robot_id=?`+a.suffix(true), []any{authorUID}, &rID, &rUID, &rStatus)
	if rErr != nil && !errors.Is(rErr, sql.ErrNoRows) {
		return out, ErrUnavailable
	}
	activeRobot := rErr == nil && (rUID != authorUID || rStatus == 1)

	var appID, appUID string
	appErr := queryOne(ctx, tx, `SELECT id,uid FROM app_bot WHERE uid=?`+a.suffix(true), []any{authorUID}, &appID, &appUID)
	if appErr != nil && !errors.Is(appErr, sql.ErrNoRows) {
		return out, ErrUnavailable
	}
	hasApp := appErr == nil

	out.authorIsHuman = u.robot == 0 && !activeRobot && !hasApp
	out.identitySupported = out.authorIsHuman
	out.authorActive = u.status == 1
	out.authorNotDestroyed = u.destroy != 2
	member, err := a.isSpaceMember(ctx, tx, spaceID, authorUID)
	if err != nil {
		return out, err
	}
	out.callerSpaceMember = member
	return out, nil
}

func (a *SQLNativeFactAdapter) evalFriendDirected(ctx context.Context, tx *dbr.Tx, uid, toUID string) (active bool, notAlone bool, err error) {
	var fUID, fToUID string
	var isDeleted, isAlone int
	e := queryOne(ctx, tx, `SELECT uid,to_uid,COALESCE(is_deleted,0),COALESCE(is_alone,0) FROM friend WHERE uid=? AND to_uid=?`+a.suffix(false), []any{uid, toUID}, &fUID, &fToUID, &isDeleted, &isAlone)
	if errors.Is(e, sql.ErrNoRows) {
		return false, false, nil
	}
	if e != nil {
		return false, false, ErrUnavailable
	}
	if fUID != uid || fToUID != toUID || isDeleted != 0 {
		return false, false, nil
	}
	return true, isAlone == 0, nil
}

func (a *SQLNativeFactAdapter) evalBlacklistClear(ctx context.Context, tx *dbr.Tx, uid, toUID string) (bool, error) {
	var sUID, sToUID string
	var blacklist int
	e := queryOne(ctx, tx, `SELECT uid,to_uid,COALESCE(blacklist,0) FROM user_setting WHERE uid=? AND to_uid=?`+a.suffix(false), []any{uid, toUID}, &sUID, &sToUID, &blacklist)
	if errors.Is(e, sql.ErrNoRows) {
		return true, nil
	}
	if e != nil {
		return false, ErrUnavailable
	}
	if sUID != uid || sToUID != toUID {
		return false, ErrUnavailable
	}
	return blacklist == 0, nil
}

func (a *SQLNativeFactAdapter) evalGroupMember(ctx context.Context, tx *dbr.Tx, groupNo, uid string) (active bool, internal bool, err error) {
	var gno, canonicalUID string
	var status, isDeleted, isExternal int
	e := queryOne(ctx, tx, `SELECT group_no,uid,COALESCE(status,1),COALESCE(is_deleted,0),COALESCE(is_external,0) FROM group_member WHERE group_no=? AND uid=?`+a.suffix(false), []any{groupNo, uid}, &gno, &canonicalUID, &status, &isDeleted, &isExternal)
	if errors.Is(e, sql.ErrNoRows) {
		return false, false, nil
	}
	if e != nil {
		return false, false, ErrUnavailable
	}
	if gno != groupNo || canonicalUID != uid || status != 1 || isDeleted != 0 {
		return false, false, nil
	}
	return true, isExternal == 0, nil
}

func (a *SQLNativeFactAdapter) evalSourceExists(ctx context.Context, tx *dbr.Tx, key Key, src IMHeaderSource) (exists bool, spaceExact bool, threadNotDeleted bool, err error) {
	switch src.Kind {
	case "dm":
		_, e := dmPeer(key, src.ID)
		return e == nil, e == nil, false, nil
	case "group":
		var canonical, sid string
		var status int
		e := queryOne(ctx, tx, "SELECT group_no,space_id,status FROM `group` WHERE group_no=?"+a.suffix(false), []any{src.ID}, &canonical, &sid, &status)
		if errors.Is(e, sql.ErrNoRows) {
			return false, false, false, nil
		}
		if e != nil {
			return false, false, false, ErrUnavailable
		}
		if canonical != src.ID || status != 1 {
			return false, false, false, nil
		}
		return true, sid == key.SpaceID, false, nil
	case "thread":
		parts := threadID.FindStringSubmatch(src.ID)
		if parts == nil {
			return false, false, false, nil
		}
		parentExists, parentSpaceExact, _, e := a.evalSourceExists(ctx, tx, key, IMHeaderSource{Kind: "group", ID: parts[1]})
		if e != nil || !parentExists {
			return false, parentSpaceExact, false, e
		}
		var parent, short string
		var status int
		e = queryOne(ctx, tx, `SELECT group_no,short_id,status FROM thread WHERE group_no=? AND short_id=?`+a.suffix(false), []any{parts[1], parts[2]}, &parent, &short, &status)
		if errors.Is(e, sql.ErrNoRows) {
			return false, parentSpaceExact, false, nil
		}
		if e != nil {
			return false, false, false, ErrUnavailable
		}
		if parent != parts[1] || short != parts[2] || status < 1 || status > 3 {
			return false, parentSpaceExact, false, nil
		}
		return true, parentSpaceExact, status != 3, nil
	default:
		return false, false, false, nil
	}
}

func (a *SQLNativeFactAdapter) readResolve(ctx context.Context, tx *dbr.Tx, key Key, keyObj map[string]any, req map[string]any, readID, sqlID, sqlIncarnation, sqlPos, sqlNonce string, sqlStarted time.Time) ([]byte, error) {
	purpose, _ := req["purpose"].(string)
	rawRefs := req["references"].([]any)
	deliveryRefs := make([]string, len(rawRefs))
	for i, v := range rawRefs {
		deliveryRefs[i] = v.(map[string]any)["delivery_reference"].(string)
	}
	imItems, imCut, err := a.IMProvider.InspectDeliveredHeadersV1(ctx, key, purpose, deliveryRefs)
	if err != nil || len(imItems) != len(deliveryRefs) || !validHex(imCut.Incarnation, 32) || !validHex(imCut.ReadNonce, 32) || imCut.ReadNonce == sqlNonce {
		return nil, ErrUnavailable
	}
	base, err := a.evalScopeBaseFacts(ctx, tx, key)
	if err != nil {
		return nil, err
	}

	results := make([]any, 0, len(imItems))
	authorityRefs := []any{imCut.ReadNonce, sqlNonce}
	for i, item := range imItems {
		ref := deliveryRefs[i]
		if item.Reference != "" && item.Reference != ref {
			return nil, ErrUnavailable
		}
		if item.Status == "unavailable" {
			results = append(results, map[string]any{
				"reference": ref,
				"status":    "unavailable",
				"reason":    item.Reason,
			})
			continue
		}
		if item.Status != "resolved" {
			return nil, ErrUnavailable
		}
		if reason := item.unknownReason(purpose); reason != "" {
			results = append(results, map[string]any{
				"reference": ref,
				"status":    "unavailable",
				"reason":    reason,
			})
			continue
		}
		hMap := item.Header.toMap()
		srcExists, spaceExact, threadNotDel, err := a.evalSourceExists(ctx, tx, key, item.Header.Source)
		if err != nil {
			return nil, err
		}
		if item.Header.AuthorKind != "human" {
			results = append(results, map[string]any{
				"reference":      ref,
				"status":         "resolved",
				"header":         hMap,
				"profile":        "unsupported_principal_v1",
				"authority_refs": authorityRefs,
				"predicates": map[string]any{
					"identity_supported":      false,
					"author_is_human":         false,
					"message_source_exact":    item.MessageSourceExact,
					"message_space_exact":     item.MessageSpaceExact,
					"message_author_exact":    item.MessageAuthorExact,
					"message_recipient_exact": item.MessageRecipientExact,
					"message_version_exact":   item.MessageVersionExact,
					"source_exists":           srcExists,
				},
			})
			continue
		}

		author, err := a.evalAuthorFacts(ctx, tx, key.SpaceID, item.Header.AuthorUID)
		if err != nil {
			return nil, err
		}
		profile := item.Header.Source.Kind + "_" + purpose + "_v1"
		preds := map[string]any{
			"identity_supported":      author.identitySupported,
			"author_is_human":         author.authorIsHuman,
			"author_active":           author.authorActive,
			"author_not_destroyed":    author.authorNotDestroyed,
			"bot_user_active":         base.botUserActive,
			"bot_robot_active":        base.botRobotActive,
			"bot_creator_eligible":    base.botCreatorEligible,
			"space_active":            base.spaceActive,
			"caller_space_member":     author.callerSpaceMember,
			"bot_space_member":        base.botSpaceMember,
			"message_source_exact":    item.MessageSourceExact,
			"message_space_exact":     item.MessageSpaceExact,
			"message_author_exact":    item.MessageAuthorExact,
			"message_recipient_exact": item.MessageRecipientExact,
			"message_version_exact":   item.MessageVersionExact,
			"source_exists":           srcExists,
		}

		switch item.Header.Source.Kind {
		case "dm":
			peer, dmErr := dmPeer(key, item.Header.Source.ID)
			dmPeerExact := dmErr == nil && peer == item.Header.AuthorUID
			isCreator := base.creatorUID != "" && base.creatorUID == item.Header.AuthorUID
			h2b, h2bNotAlone, err := a.evalFriendDirected(ctx, tx, item.Header.AuthorUID, key.BotUID)
			if err != nil {
				return nil, err
			}
			b2h, b2hNotAlone, err := a.evalFriendDirected(ctx, tx, key.BotUID, item.Header.AuthorUID)
			if err != nil {
				return nil, err
			}
			humanBlocksClear, err := a.evalBlacklistClear(ctx, tx, item.Header.AuthorUID, key.BotUID)
			if err != nil {
				return nil, err
			}
			botBlocksClear, err := a.evalBlacklistClear(ctx, tx, key.BotUID, item.Header.AuthorUID)
			if err != nil {
				return nil, err
			}
			preds["dm_peer_exact"] = dmPeerExact
			preds["dm_human_blocks_bot_clear"] = humanBlocksClear
			preds["dm_bot_blocks_human_clear"] = botBlocksClear
			if purpose == "reply" {
				preds["dm_bot_to_human_send_relation"] = isCreator || (h2b && h2bNotAlone && b2h && b2hNotAlone)
				preds["bot_current_send"] = item.BotCurrentSend
			} else {
				preds["dm_human_to_bot_relation"] = isCreator || h2b
				preds["dm_bot_to_human_read_relation"] = isCreator || b2h
				preds["caller_current_read"] = item.CallerCurrentRead
				preds["bot_current_read"] = item.BotCurrentRead
				preds["caller_original_visible"] = item.CallerOriginalVisible
				preds["bot_original_visible"] = item.BotOriginalVisible
			}
		case "group":
			callerMember, callerInternal, err := a.evalGroupMember(ctx, tx, item.Header.Source.ID, item.Header.AuthorUID)
			if err != nil {
				return nil, err
			}
			botMember, botInternal, err := a.evalGroupMember(ctx, tx, item.Header.Source.ID, key.BotUID)
			if err != nil {
				return nil, err
			}
			preds["group_space_exact"] = spaceExact
			preds["bot_group_active_member"] = botMember
			preds["bot_external_access"] = botMember && (botInternal || base.botSpaceMember)
			if purpose == "reply" {
				preds["bot_current_send"] = item.BotCurrentSend
			} else {
				preds["caller_group_active_member"] = callerMember
				preds["caller_external_access"] = callerMember && (callerInternal || author.callerSpaceMember)
				preds["caller_current_read"] = item.CallerCurrentRead
				preds["bot_current_read"] = item.BotCurrentRead
				preds["caller_original_visible"] = item.CallerOriginalVisible
				preds["bot_original_visible"] = item.BotOriginalVisible
			}
		case "thread":
			parts := threadID.FindStringSubmatch(item.Header.Source.ID)
			parentGroup := ""
			if parts != nil {
				parentGroup = parts[1]
			}
			parentVerified := parts != nil && item.Header.ParentGroup != nil && *item.Header.ParentGroup == parentGroup
			callerParent, callerInternal, err := a.evalGroupMember(ctx, tx, parentGroup, item.Header.AuthorUID)
			if err != nil {
				return nil, err
			}
			botParent, botInternal, err := a.evalGroupMember(ctx, tx, parentGroup, key.BotUID)
			if err != nil {
				return nil, err
			}
			preds["parent_verified"] = parentVerified
			preds["thread_not_deleted"] = threadNotDel
			preds["parent_space_exact"] = spaceExact
			preds["bot_parent_active_member"] = botParent
			preds["bot_external_access"] = botParent && (botInternal || base.botSpaceMember)
			if purpose == "reply" {
				preds["bot_current_send"] = item.BotCurrentSend
			} else {
				preds["caller_parent_active_member"] = callerParent
				preds["caller_parent_current_read"] = item.CallerParentCurrentRead
				preds["bot_parent_current_read"] = item.BotParentCurrentRead
				preds["caller_current_read"] = item.CallerCurrentRead
				preds["bot_current_read"] = item.BotCurrentRead
				preds["caller_original_visible"] = item.CallerOriginalVisible
				preds["bot_original_visible"] = item.BotOriginalVisible
				preds["caller_external_access"] = callerParent && (callerInternal || author.callerSpaceMember)
			}
		default:
			return nil, ErrUnavailable
		}

		results = append(results, map[string]any{
			"reference":      ref,
			"status":         "resolved",
			"header":         hMap,
			"profile":        profile,
			"authority_refs": authorityRefs,
			"predicates":     preds,
		})
	}

	sqlCompleted := time.Now().UTC().Truncate(time.Microsecond)
	sqlAuth := formatAuthorityDescriptor(sqlID, "sql", sqlIncarnation, sqlPos, sqlNonce, sqlStarted, sqlCompleted)
	imAuth := formatAuthorityDescriptor(imCut.AuthorityID, "im", imCut.Incarnation, imCut.Position, imCut.ReadNonce, imCut.StartedAt, imCut.CompletedAt)
	authorities, observedAt := sortAuthoritiesAndObserved(sqlAuth, imAuth, sqlCompleted, imCut.CompletedAt)

	resp := map[string]any{
		"schema_version":  json.Number("1"),
		"profile_version": json.Number("1"),
		"key":             keyObj,
		"request_id":      req["request_id"],
		"read_id":         readID,
		"observed_at":     observedAt,
		"authorities":     authorities,
		"results":         results,
	}
	return canonical(resp), nil
}

func (a *SQLNativeFactAdapter) readHistoryHeaders(ctx context.Context, tx *dbr.Tx, key Key, keyObj map[string]any, req map[string]any, readID, sqlID, sqlIncarnation, sqlPos, sqlNonce string, sqlStarted time.Time) ([]byte, error) {
	sourceRef, _ := req["source_reference"].(string)
	var cursor *string
	if s, ok := req["cursor"].(string); ok {
		cursor = &s
	}
	pageSize, err := U64(string(req["page_size"].(json.Number)))
	if err != nil {
		return nil, err
	}
	base, err := a.evalScopeBaseFacts(ctx, tx, key)
	if err != nil || !base.botUserActive || !base.botRobotActive || !base.spaceActive || !base.botSpaceMember {
		return nil, ErrUnavailable
	}
	page, err := a.IMProvider.ListHistoryHeadersV1(ctx, key, sourceRef, cursor, pageSize)
	if err != nil || !validHex(page.Cut.Incarnation, 32) || !validHex(page.Cut.ReadNonce, 32) || page.Cut.ReadNonce == sqlNonce {
		return nil, ErrUnavailable
	}
	items := make([]any, 0, len(page.Items))
	for _, item := range page.Items {
		if item.Status == "unavailable" {
			items = append(items, map[string]any{
				"reference": item.Reference,
				"status":    "unavailable",
				"reason":    item.Reason,
			})
			continue
		}
		if item.Status != "resolved" {
			return nil, ErrUnavailable
		}
		items = append(items, item.Header.toMap())
	}
	var nextCursor any
	if page.NextCursor != nil {
		nextCursor = *page.NextCursor
	}
	sqlCompleted := time.Now().UTC().Truncate(time.Microsecond)
	sqlAuth := formatAuthorityDescriptor(sqlID, "sql", sqlIncarnation, sqlPos, sqlNonce, sqlStarted, sqlCompleted)
	imAuth := formatAuthorityDescriptor(page.Cut.AuthorityID, "im", page.Cut.Incarnation, page.Cut.Position, page.Cut.ReadNonce, page.Cut.StartedAt, page.Cut.CompletedAt)
	authorities, observedAt := sortAuthoritiesAndObserved(sqlAuth, imAuth, sqlCompleted, page.Cut.CompletedAt)

	resp := map[string]any{
		"schema_version":  json.Number("1"),
		"profile_version": json.Number("1"),
		"key":             keyObj,
		"request_id":      req["request_id"],
		"read_id":         readID,
		"observed_at":     observedAt,
		"authorities":     authorities,
		"page": map[string]any{
			"source_reference": sourceRef,
			"page_id":          page.PageID,
			"items":            items,
			"next_cursor":      nextCursor,
			"end":              page.End,
			"authority_refs":   []any{page.Cut.ReadNonce, sqlNonce},
		},
	}
	return canonical(resp), nil
}

func (a *SQLNativeFactAdapter) readHistoryDisclosure(ctx context.Context, tx *dbr.Tx, key Key, keyObj map[string]any, req map[string]any, readID, sqlID, sqlIncarnation, sqlPos, sqlNonce string, sqlStarted time.Time) ([]byte, error) {
	contextID, _ := req["context_id"].(string)
	contextEpoch, _ := req["context_epoch"].(string)
	rawHeaders := req["headers"].([]any)
	headerRefs := make([]string, len(rawHeaders))
	for i, v := range rawHeaders {
		headerRefs[i] = v.(map[string]any)["header_reference"].(string)
	}
	var storedEpoch string
	var retired int
	if err := queryOne(ctx, tx, `SELECT context_epoch,retired FROM bot_permission_scope WHERE bot_uid=? AND space_id=?`+a.suffix(false), []any{key.BotUID, key.SpaceID}, &storedEpoch, &retired); err != nil || retired != 0 {
		return nil, ErrUnavailable
	}
	disclosures, imCut, err := a.IMProvider.IssueHistoryDisclosureV1(ctx, key, contextID, contextEpoch, headerRefs)
	if err != nil || len(disclosures) != len(headerRefs) || !validHex(imCut.Incarnation, 32) || !validHex(imCut.ReadNonce, 32) || imCut.ReadNonce == sqlNonce {
		return nil, ErrUnavailable
	}
	epochMatches := storedEpoch == contextEpoch
	results := make([]any, 0, len(disclosures))
	for i, item := range disclosures {
		ref := headerRefs[i]
		if item.Reference != "" && item.Reference != ref {
			return nil, ErrUnavailable
		}
		if !epochMatches {
			results = append(results, map[string]any{
				"reference": ref,
				"status":    "unavailable",
				"reason":    "expired_reference",
			})
			continue
		}
		if item.Status == "unavailable" {
			results = append(results, map[string]any{
				"reference": ref,
				"status":    "unavailable",
				"reason":    item.Reason,
			})
			continue
		}
		if item.Status != "resolved" {
			return nil, ErrUnavailable
		}
		results = append(results, map[string]any{
			"reference":        ref,
			"status":           "resolved",
			"permit":           item.Permit,
			"header_reference": item.HeaderReference,
			"context_id":       contextID,
			"context_epoch":    contextEpoch,
			"payload_sha256":   item.PayloadSHA256,
			"authority_refs":   []any{imCut.ReadNonce, sqlNonce},
		})
	}
	sqlCompleted := time.Now().UTC().Truncate(time.Microsecond)
	sqlAuth := formatAuthorityDescriptor(sqlID, "sql", sqlIncarnation, sqlPos, sqlNonce, sqlStarted, sqlCompleted)
	imAuth := formatAuthorityDescriptor(imCut.AuthorityID, "im", imCut.Incarnation, imCut.Position, imCut.ReadNonce, imCut.StartedAt, imCut.CompletedAt)
	authorities, observedAt := sortAuthoritiesAndObserved(sqlAuth, imAuth, sqlCompleted, imCut.CompletedAt)

	resp := map[string]any{
		"schema_version":  json.Number("1"),
		"profile_version": json.Number("1"),
		"key":             keyObj,
		"request_id":      req["request_id"],
		"read_id":         readID,
		"observed_at":     observedAt,
		"authorities":     authorities,
		"results":         results,
	}
	return canonical(resp), nil
}

func (a *SQLNativeFactAdapter) ValidateCuts(ctx context.Context, key Key, responseBytes []byte) error {
	if a == nil || a.Authority == nil || a.IMProvider == nil || !key.Valid() {
		return ErrUnavailable
	}
	var env struct {
		Authorities []struct {
			AuthorityID string `json:"authority_id"`
			Kind        string `json:"kind"`
			Incarnation string `json:"incarnation"`
			Position    string `json:"position"`
			ReadNonce   string `json:"read_nonce"`
			Consistency string `json:"consistency"`
		} `json:"authorities"`
	}
	if err := json.Unmarshal(responseBytes, &env); err != nil || len(env.Authorities) == 0 {
		return ErrUnavailable
	}
	hasSQL, hasIM := false, false
	for _, auth := range env.Authorities {
		if auth.Consistency != "ack_before_read_start" {
			return ErrUnavailable
		}
		switch auth.Kind {
		case "sql":
			hasSQL = true
			if verifier, ok := a.Authority.(sqlCutVerifier); ok {
				if err := verifier.VerifySQLAuthorityCut(auth.AuthorityID, auth.Incarnation, auth.Position); err != nil {
					return err
				}
			} else if _, err := U64(auth.Position); err != nil || !validHex(auth.Incarnation, 32) {
				return ErrUnavailable
			}
		case "im":
			hasIM = true
			if err := a.IMProvider.ValidateIMCut(ctx, key, auth.AuthorityID, auth.Incarnation, auth.Position, auth.ReadNonce); err != nil {
				return ErrUnavailable
			}
		default:
			return ErrUnavailable
		}
	}
	if !hasSQL || !hasIM {
		return ErrUnavailable
	}
	return nil
}
