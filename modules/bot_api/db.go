package bot_api

import (
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/db"
	"github.com/gocraft/dbr/v2"
	"strings"
)

type botAPIDB struct {
	session *dbr.Session
	ctx     *config.Context
}

func newBotAPIDB(ctx *config.Context) *botAPIDB {
	return &botAPIDB{
		ctx:     ctx,
		session: ctx.DB(),
	}
}

// ==================== Robot Model (User Bot) ====================

type robotModel struct {
	AppID         string
	RobotID       string
	Username      string
	InlineOn      int
	Placeholder   string
	Token         string
	Version       int64
	Status        int
	CreatorUID    string
	Description   string
	BotToken      string
	IMTokenCache  string
	BotCommands   string
	AutoApprove   int
	AccessMode    int
	AgentPlatform string
	AgentVersion  string
	PluginVersion string
	db.BaseModel
}

// queryRobotByBotToken queries robot by bot token.
func (d *botAPIDB) queryRobotByBotToken(botToken string) (*robotModel, error) {
	if botToken == "" {
		return nil, nil
	}
	var m *robotModel
	_, err := d.session.Select("*").From("robot").Where("bot_token=? and bot_token!='' and status=1", botToken).Load(&m)
	return m, err
}

// queryRobotByRobotID queries robot by robot ID.
func (d *botAPIDB) queryRobotByRobotID(robotID string) (*robotModel, error) {
	var m *robotModel
	_, err := d.session.Select("*").From("robot").Where("robot_id=?", robotID).Load(&m)
	return m, err
}

// updateRobotIMTokenCache updates the IM token cache for a robot.
func (d *botAPIDB) updateRobotIMTokenCache(robotID string, imToken string) error {
	_, err := d.session.Update("robot").SetMap(map[string]interface{}{
		"im_token_cache": imToken,
	}).Where("robot_id=?", robotID).Exec()
	return err
}

// updateRobotAgentInfo updates agent runtime info for a robot.
func (d *botAPIDB) updateRobotAgentInfo(robotID, agentPlatform, agentVersion, pluginVersion string) error {
	_, err := d.session.Update("robot").SetMap(map[string]interface{}{
		"agent_platform": agentPlatform,
		"agent_version":  agentVersion,
		"plugin_version": pluginVersion,
	}).Where("robot_id=?", robotID).Exec()
	return err
}

// updateBotCommands updates bot commands JSON.
func (d *botAPIDB) updateBotCommands(robotID string, botCommands string) error {
	_, err := d.session.Update("robot").SetMap(map[string]interface{}{
		"bot_commands": botCommands,
	}).Where("robot_id=?", robotID).Exec()
	return err
}

// queryAllActiveRobots queries all active robots with non-empty bot_token.
func (d *botAPIDB) queryAllActiveRobots() ([]*robotModel, error) {
	var models []*robotModel
	_, err := d.session.Select("*").From("robot").Where("status=1 AND bot_token != ''").Load(&models)
	return models, err
}

// querySpaceIDByRobotID returns the active Space ID for the given bot.
//
// Mininglamp-OSS/octo-server#36 (multi-Space ambiguity, PR#35 deep-review
// High-2): when a User Bot is a member of multiple active Spaces, the prior
// SQL had no `ORDER BY` and used `LoadOne`, leaving the result up to the DB
// engine. This function now:
//
//  1. Loads all matching rows (not just one) so the count is observable.
//  2. Orders by `sm.created_at ASC, sm.space_id ASC` so ties resolve to the
//     earliest joined Space, with `space_id` as a deterministic tie-breaker.
//  3. Returns `dbr.ErrNotFound` for the empty case to preserve the existing
//     caller contract (callers branch on `errors.Is(err, dbr.ErrNotFound)`).
//
// The full row list is exposed via `querySpaceIDsByRobotID` for callers that
// want to observe ambiguity (`len(spaceIDs) > 1`) without issuing a second
// query — see `resolveBotActiveSpaceID` for the structured warn it emits.
func (d *botAPIDB) querySpaceIDByRobotID(robotID string) (string, error) {
	spaceID, _, err := d.querySpaceIDsByRobotID(robotID)
	return spaceID, err
}

// querySpaceIDsByRobotID is the multi-row variant. Returns the deterministic
// primary SpaceID, the full ordered list of matching SpaceIDs, and any DB
// error. Empty result → `dbr.ErrNotFound` (preserves caller contract).
func (d *botAPIDB) querySpaceIDsByRobotID(robotID string) (string, []string, error) {
	var spaceIDs []string
	_, err := d.session.SelectBySql(
		// s.space_id, not sm.space_id: the membership row carries its own copy of
		// the identity, and a copy that differs in case still joins (the space
		// tables are case-insensitive) while the grant table's utf8mb4_bin
		// lookup would then miss. Every Space identity that leaves this file is
		// the canonical stored one — see the header note on canonicalisation.
		"SELECT s.space_id FROM space_member sm INNER JOIN space s ON s.space_id = sm.space_id WHERE sm.uid=? AND sm.status=1 AND s.status=1 ORDER BY sm.created_at ASC, s.space_id ASC",
		robotID,
	).Load(&spaceIDs)
	if err != nil {
		return "", nil, err
	}
	if len(spaceIDs) == 0 {
		return "", nil, dbr.ErrNotFound
	}
	return spaceIDs[0], spaceIDs, nil
}

// isBotSpaceAuthorized reports whether `robotID` is allowed to dispatch into
// the given `spaceID`. Used by `/v1/bot/sendMessage` to validate an
// `X-Space-ID` header hint before honoring it (Option B from issue#36).
// Without this check, the header would be a trivial cross-Space bypass.
//
// Authorization is the OR of three production conditions — all gated on the
// target Space being active (`space.status=1`):
//
//  1. **User Bot / manually-added bot membership** — the bot has an active
//     `space_member` row for the target Space (status=1).
//  2. **Platform App Bot** — the bot is a published `app_bot` row with
//     `scope='platform'` (status=1). Platform App Bots are visible in every
//     active Space (mirrors `pkg/space/query.go:CheckBotsInSpace`) and never
//     get a `space_member` insert (see `modules/app_bot/db.go:insertAppBot`).
//     Without this branch the validator rejects every legitimate platform App
//     Bot dispatch and the caller's `enrichBotPayloadWithSpaceID` strips the
//     payload.space_id, downgrading the request to PERSONAL DM (Mininglamp-OSS/
//     octo-server PR#43 R1 critical from Jerry-Xin + lml2468).
//  3. **Scope=space App Bot** — the bot is a published `app_bot` row with
//     `scope='space'` AND its own `space_id` matches the requested SpaceID.
//     This branch is mostly defensive; production traffic for scope=space App
//     Bots reaches `resolveBotActiveSpaceID` via `CtxKeyAppBotSpaceID` (the
//     ctx fast path) and never falls through to the header validator. The
//     branch is included so a future refactor (or test regression) cannot
//     turn the header path into a cross-Space bypass for scope=space bots.
//
// Implementation note: two short queries instead of one OR-joined statement.
// Both run on indexed columns (`space_member(uid, space_id)`, `app_bot.uid`)
// and the second is skipped when the first hits, so the common case is a
// single round trip. A single combined query was rejected because OR-of-
// EXISTS in MySQL with parameter reuse forces the planner to materialize
// both branches even when the first short-circuits.
// It returns the **canonical** `space.space_id` rather than the spelling it was
// asked about, and callers must propagate that value rather than their input.
// The two are not interchangeable: this authorization joins case-insensitively
// (the space tables declare no collation and inherit the database default),
// while `card_template_grant.scope_space_id` is `utf8mb4_bin` and is matched
// byte-for-byte. Handing the caller's spelling to a grant lookup therefore
// authorizes under one comparison and looks up under another — and where the
// two disagree, an exact-scope revoke tombstone is not found and the still-
// active global grant answers in its place. That is a revoke bypass reachable
// by changing the case of one request header (review P0-1, yujiawei).
func (d *botAPIDB) isBotSpaceAuthorized(robotID, spaceID string) (string, bool, error) {
	if robotID == "" || spaceID == "" {
		return "", false, nil
	}
	// (1) space_member path: active member row in the target active Space.
	var canonical []string
	_, err := d.session.SelectBySql(
		"SELECT s.space_id FROM space_member sm INNER JOIN space s ON s.space_id = sm.space_id WHERE sm.uid=? AND sm.space_id=? AND sm.status=1 AND s.status=1 LIMIT 1",
		robotID, spaceID,
	).Load(&canonical)
	if err != nil {
		return "", false, err
	}
	if len(canonical) > 0 {
		return canonical[0], true, nil
	}
	// (2)+(3) app_bot path: published platform Bot in any active Space, OR
	// scope=space Bot whose own SpaceID matches the requested target Space.
	// Both branches require the target Space to be active.
	_, err = d.session.SelectBySql(
		"SELECT s.space_id FROM app_bot ab INNER JOIN space s ON s.space_id=? "+
			"WHERE ab.uid=? AND ab.status=1 AND s.status=1 "+
			"AND (ab.scope='platform' OR (ab.scope='space' AND ab.space_id=?)) LIMIT 1",
		spaceID, robotID, spaceID,
	).Load(&canonical)
	if err != nil {
		return "", false, err
	}
	if len(canonical) == 0 {
		return "", false, nil
	}
	return canonical[0], true, nil
}

// ==================== App Bot Model ====================

type appBotModel struct {
	ID          string `db:"id"`
	UID         string `db:"uid"`
	DisplayName string `db:"display_name"`
	Description string `db:"description"`
	Avatar      string `db:"avatar"`
	Scope       string `db:"scope"`
	SpaceID     string `db:"space_id"`
	Status      int    `db:"status"`
	Token       string `db:"token"`
	CreatedBy   string `db:"created_by"`
	db.BaseModel
}

// queryAppBotByToken queries app_bot by token.
func (d *botAPIDB) queryAppBotByToken(token string) (*appBotModel, error) {
	if token == "" {
		return nil, nil
	}
	var m *appBotModel
	_, err := d.session.Select("*").From("app_bot").Where("token=?", token).Load(&m)
	return m, err
}

// queryAppBotByUID queries app_bot by UID.
func (d *botAPIDB) queryAppBotByUID(uid string) (*appBotModel, error) {
	var m *appBotModel
	_, err := d.session.Select("*").From("app_bot").Where("uid=?", uid).Load(&m)
	return m, err
}

// queryGroupSpaceID returns the authoritative Space of a group row, and whether
// that Space is currently active. It is the Space a card sent into that group is
// authorized against — the Bot's own membership Space is not, since a Bot can
// belong to one Space and be a member of a group that lives in another. An empty
// result is dbr.ErrNotFound so the caller fails closed rather than treating "no
// row" as "no Space needed".
//
// The active flag exists because every other Space resolver in this file treats
// `space.status = 1` as load-bearing — querySpaceIDsByRobotID, both branches of
// isBotSpaceAuthorized, and isUserSpaceMember all require it — and this one did
// not. A grant scoped to a Space an operator had disabled therefore still
// authorized sends into that Space's groups, while the same bot's membership,
// an X-Space-ID header naming it, and a DM peer inside it were all refused. The
// join is LEFT rather than INNER so that "this group belongs to no Space" stays
// distinguishable from "this group's Space is disabled": the first is a
// determination the caller can act on, the second is a Space it must not use.
func (d *botAPIDB) queryGroupSpaceID(groupNo string) (spaceID string, spaceActive bool, err error) {
	var rows []struct {
		SpaceID string `db:"space_id"`
		Active  bool   `db:"active"`
	}
	_, err = d.session.SelectBySql(
		// COALESCE picks the canonical s.space_id whenever the Space row joined
		// (i.e. whenever active is true, which is the only case a caller may
		// use the value for); the group's own copy survives only for the
		// inactive/absent branch, which every caller refuses.
		"SELECT COALESCE(s.space_id, `group`.space_id) AS space_id, (s.space_id IS NOT NULL) AS active "+
			"FROM `group` LEFT JOIN space s ON s.space_id = `group`.space_id AND s.status = 1 "+
			"WHERE `group`.group_no=? LIMIT 1", groupNo,
	).Load(&rows)
	if err != nil {
		return "", false, err
	}
	if len(rows) == 0 {
		return "", false, dbr.ErrNotFound
	}
	return rows[0].SpaceID, rows[0].Active, nil
}

// isUserSpaceMember reports whether a plain user holds an active membership in
// an active Space.
//
// It is deliberately narrower than isBotSpaceAuthorized: that rule also honours
// platform App Bots, which are visible in every Space by construction. A human
// recipient has no such blanket visibility, so a DM's peer is checked against
// space_member alone.
func (d *botAPIDB) isUserSpaceMember(uid, spaceID string) (bool, error) {
	if strings.TrimSpace(uid) == "" || strings.TrimSpace(spaceID) == "" {
		return false, nil
	}
	var count int
	err := d.session.SelectBySql(
		"SELECT COUNT(*) FROM space_member sm INNER JOIN space s ON s.space_id = sm.space_id "+
			"WHERE sm.uid=? AND sm.space_id=? AND sm.status=1 AND s.status=1",
		uid, spaceID,
	).LoadOne(&count)
	if err != nil {
		return false, err
	}
	return count > 0, nil
}
