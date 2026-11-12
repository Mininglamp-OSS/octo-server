package permissionbundle

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"

	"github.com/gocraft/dbr/v2"
)

// ManagedIdentityRequest is the opt-in identity arm only. The caller keeps its
// existing unmanaged parser when identity_version is omitted.
type ManagedIdentityRequest struct {
	BotToken      string
	IncludePolicy bool
}

func ParseManagedIdentity(data []byte) (ManagedIdentityRequest, error) {
	v, e := decodeStrict(bytes.NewReader(data), 16384)
	if e != nil {
		return ManagedIdentityRequest{}, e
	}
	m, ok := v.(map[string]any)
	if ok && m["identity_version"] != json.Number("2") {
		if n, yes := m["identity_version"].(json.Number); yes {
			if _, e := U64(string(n)); e == nil {
				return ManagedIdentityRequest{}, ErrVersion
			}
		}
	}
	if !ok || m["identity_version"] != json.Number("2") || len(m) < 2 || len(m) > 3 {
		return ManagedIdentityRequest{}, ErrInvalid
	}
	for k := range m {
		if k != "identity_version" && k != "bot_token" && k != "include_mcp_policy" {
			return ManagedIdentityRequest{}, ErrInvalid
		}
	}
	token, ok := m["bot_token"].(string)
	if !ok || !strings.HasPrefix(token, "bf_") {
		return ManagedIdentityRequest{}, ErrInvalid
	}
	include := false
	if v, ok := m["include_mcp_policy"]; ok {
		var valid bool
		include, valid = v.(bool)
		if !valid {
			return ManagedIdentityRequest{}, ErrInvalid
		}
	}
	return ManagedIdentityRequest{token, include}, nil
}

// Identity validates the native identity rows in the caller's token-locking
// transaction. It supplies no Space, policy grant, message or recovery proof.
func (n SQLNative) Identity(ctx context.Context, tx *dbr.Tx, uid, creator string) error {
	if !ValidNativeID(uid) || !ValidNativeID(creator) {
		return ErrForbidden
	}
	if _, e := n.user(ctx, tx, uid, 1); e != nil {
		return e
	}
	if e := n.noApp(ctx, tx, uid); e != nil {
		return e
	}
	if _, e := n.user(ctx, tx, creator, 0); e != nil {
		return e
	}
	var robotID, canonical string
	var status int
	e := queryOne(ctx, tx, `SELECT id,robot_id,status FROM robot WHERE robot_id=?`+n.suffix(true), []any{creator}, &robotID, &canonical, &status)
	if e != nil && !errors.Is(e, sql.ErrNoRows) {
		return ErrUnavailable
	}
	if e == nil && (canonical != creator || status == 1) {
		return ErrForbidden
	}
	if e := n.noApp(ctx, tx, creator); e != nil {
		return e
	}
	return nil
}
