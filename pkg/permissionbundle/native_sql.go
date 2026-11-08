package permissionbundle

import (
	"context"
	"database/sql"
	"errors"
	"hash/crc32"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/gocraft/dbr/v2"
)

// SQLNative uses native unique equality lookups, then compares returned bytes.
// It never uses BINARY expressions that could lose the native absence range.
// These are configuration-time facts, not original-message/read/send authority.
type SQLNative struct{ testNoLocks bool }

func (n SQLNative) suffix(absence bool) string {
	if n.testNoLocks {
		return ""
	}
	if absence {
		return " FOR SHARE"
	}
	return " FOR SHARE NOWAIT"
}
func queryOne(ctx context.Context, tx *dbr.Tx, q string, args []any, dest ...any) error {
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	return tx.QueryRowContext(ctx, q, args...).Scan(dest...)
}

type nativeUser struct {
	id, uid                string
	status, destroy, robot int
}

func (n SQLNative) user(ctx context.Context, tx *dbr.Tx, uid string, kind int) (nativeUser, error) {
	var u nativeUser
	e := queryOne(ctx, tx, `SELECT id,uid,status,COALESCE(is_destroy,0),robot FROM user WHERE uid=?`+n.suffix(false), []any{uid}, &u.id, &u.uid, &u.status, &u.destroy, &u.robot)
	if e != nil {
		return u, ErrUnavailable
	}
	if u.uid != uid || u.status != 1 || u.destroy == 2 || u.robot != kind {
		return u, ErrForbidden
	}
	return u, nil
}
func (n SQLNative) noApp(ctx context.Context, tx *dbr.Tx, uid string) error {
	var id, canonical string
	e := queryOne(ctx, tx, `SELECT id,uid FROM app_bot WHERE uid=?`+n.suffix(true), []any{uid}, &id, &canonical)
	if errors.Is(e, sql.ErrNoRows) {
		return nil
	}
	if e != nil {
		return ErrUnavailable
	}
	return ErrForbidden
}
func (n SQLNative) member(ctx context.Context, tx *dbr.Tx, key Key, uid string) error {
	var sid, canonical string
	var status int
	e := queryOne(ctx, tx, `SELECT space_id,uid,status FROM space_member WHERE space_id=? AND uid=?`+n.suffix(false), []any{key.SpaceID, uid}, &sid, &canonical, &status)
	if e != nil {
		return ErrUnavailable
	}
	if sid != key.SpaceID || canonical != uid || status != 1 {
		return ErrForbidden
	}
	return nil
}
func (n SQLNative) human(ctx context.Context, tx *dbr.Tx, key Key, uid string) (nativeUser, error) {
	u, e := n.user(ctx, tx, uid, 0)
	if e != nil {
		return u, e
	}
	var id, canonical string
	var status int
	e = queryOne(ctx, tx, `SELECT id,robot_id,status FROM robot WHERE robot_id=?`+n.suffix(true), []any{uid}, &id, &canonical, &status)
	if e != nil && !errors.Is(e, sql.ErrNoRows) {
		return u, ErrUnavailable
	}
	if e == nil && (canonical != uid || status == 1) {
		return u, ErrForbidden
	}
	if e = n.noApp(ctx, tx, uid); e != nil {
		return u, e
	}
	if e = n.member(ctx, tx, key, uid); e != nil {
		return u, e
	}
	return u, nil
}
func (n SQLNative) Scope(ctx context.Context, tx *dbr.Tx, key Key, actor, incarnation string) (NativeAnchor, error) {
	var robotID, uid, creator string
	var status int
	e := queryOne(ctx, tx, `SELECT id,robot_id,creator_uid,status FROM robot WHERE robot_id=?`+n.suffix(false), []any{key.BotUID}, &robotID, &uid, &creator, &status)
	if e != nil {
		return NativeAnchor{}, ErrUnavailable
	}
	if uid != key.BotUID || status != 1 || !ValidNativeID(creator) || actor != "" && actor != creator {
		return NativeAnchor{}, ErrForbidden
	}
	bot, e := n.user(ctx, tx, key.BotUID, 1)
	if e != nil {
		return NativeAnchor{}, e
	}
	if e = n.noApp(ctx, tx, key.BotUID); e != nil {
		return NativeAnchor{}, e
	}
	var sid, spaceID string
	e = queryOne(ctx, tx, `SELECT id,space_id,status FROM space WHERE space_id=?`+n.suffix(false), []any{key.SpaceID}, &spaceID, &sid, &status)
	if e != nil {
		return NativeAnchor{}, ErrUnavailable
	}
	if sid != key.SpaceID || status != 1 {
		return NativeAnchor{}, ErrForbidden
	}
	if e = n.member(ctx, tx, key, key.BotUID); e != nil {
		return NativeAnchor{}, e
	}
	owner, e := n.human(ctx, tx, key, creator)
	if e != nil {
		return NativeAnchor{}, e
	}
	return NativeAnchor{incarnation, robotID, bot.id, creator, owner.id, spaceID}, nil
}

// PairReference uses the pinned native helper only after guarding its known
// ambiguous domains. It is server-side configuration preparation, not proof of
// native delivery, conversation existence or original visibility.
func PairReference(bot, peer string) (map[string]any, error) {
	if !ValidNativeID(bot) || !ValidNativeID(peer) || bot == peer || strings.Contains(bot, "@") || strings.Contains(peer, "@") || crc32.ChecksumIEEE([]byte(bot)) == crc32.ChecksumIEEE([]byte(peer)) {
		return nil, ErrReference
	}
	id := common.GetFakeChannelIDWith(bot, peer)
	if !validText(id, 100, 400) {
		return nil, ErrReference
	}
	return map[string]any{"kind": "dm", "id": id}, nil
}
func dmPeer(key Key, id string) (string, error) {
	p := strings.Split(id, "@")
	if len(p) != 2 {
		return "", ErrReference
	}
	peer := ""
	if p[0] == key.BotUID {
		peer = p[1]
	} else if p[1] == key.BotUID {
		peer = p[0]
	} else {
		return "", ErrReference
	}
	ref, e := PairReference(key.BotUID, peer)
	if e != nil || ref["id"] != id {
		return "", ErrReference
	}
	return peer, nil
}
func (n SQLNative) Prepare(ctx context.Context, tx *dbr.Tx, key Key, peers []string) ([]map[string]any, error) {
	// Lock in byte order but preserve request order in the entire successful map.
	ordered := append([]string(nil), peers...)
	sort.Strings(ordered)
	for _, peer := range ordered {
		if _, e := n.human(ctx, tx, key, peer); e != nil {
			return nil, ErrReference
		}
	}
	out := make([]map[string]any, 0, len(peers))
	for _, peer := range peers {
		ref, e := PairReference(key.BotUID, peer)
		if e != nil {
			return nil, e
		}
		out = append(out, ref)
	}
	return out, nil
}

var threadID = regexp.MustCompile(`^([0-9a-fA-F]{32})____([0-9]{15,20})$`)

func (n SQLNative) group(ctx context.Context, tx *dbr.Tx, key Key, id string) error {
	var canonical, sid string
	var status int
	e := queryOne(ctx, tx, "SELECT group_no,space_id,status FROM `group` WHERE group_no=?"+n.suffix(false), []any{id}, &canonical, &sid, &status)
	if e != nil || canonical != id || sid != key.SpaceID || status != 1 {
		return ErrReference
	}
	return nil
}
func (n SQLNative) reference(ctx context.Context, tx *dbr.Tx, key Key, ref map[string]any) error {
	id := ref["id"].(string)
	switch ref["kind"] {
	case "dm":
		peer, e := dmPeer(key, id)
		if e != nil {
			return e
		}
		if _, e = n.human(ctx, tx, key, peer); e != nil {
			return ErrReference
		}
	case "group":
		return n.group(ctx, tx, key, id)
	case "thread":
		parts := threadID.FindStringSubmatch(id)
		if parts == nil {
			return ErrReference
		}
		if e := n.group(ctx, tx, key, parts[1]); e != nil {
			return e
		}
		var parent, short string
		var status int
		e := queryOne(ctx, tx, `SELECT group_no,short_id,status FROM thread WHERE group_no=? AND short_id=?`+n.suffix(false), []any{parts[1], parts[2]}, &parent, &short, &status)
		if e != nil || parent != parts[1] || short != parts[2] || status == 3 || status < 1 || status > 3 {
			return ErrReference
		}
	default:
		return ErrReference
	}
	return nil
}
func (n SQLNative) Rules(ctx context.Context, tx *dbr.Tx, key Key, rules []Rule) error {
	humans := map[string]bool{}
	refs := map[string]map[string]any{}
	for _, rule := range rules {
		caller := rule["caller"].(map[string]any)
		if ids, ok := caller["uids"]; ok {
			for _, id := range ids.([]any) {
				humans[id.(string)] = true
			}
		}
		source := rule["source"].(map[string]any)
		if a, ok := source["refs"]; ok {
			for _, v := range a.([]any) {
				ref := v.(map[string]any)
				refs[string(canonical(ref))] = ref
			}
		}
	}
	uids := make([]string, 0, len(humans))
	for uid := range humans {
		uids = append(uids, uid)
	}
	sort.Strings(uids)
	for _, uid := range uids {
		if _, e := n.human(ctx, tx, key, uid); e != nil {
			return ErrReference
		}
	}
	keys := make([]string, 0, len(refs))
	for k := range refs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	// Batches retain all locks in the caller's SAME transaction/deadline; 32
	// never becomes a policy cap and there is no intermediate publication.
	for offset := 0; offset < len(keys); offset += 32 {
		end := min(offset+32, len(keys))
		for _, k := range keys[offset:end] {
			if e := n.reference(ctx, tx, key, refs[k]); e != nil {
				return e
			}
		}
	}
	return nil
}
