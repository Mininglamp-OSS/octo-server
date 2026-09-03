package bot_api

import (
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/db"
	"github.com/gocraft/dbr/v2"
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
	// AgentHosting Agent 自报托管形态，小写 ASCII slug（self_hosted / octo_hosted /
	// <vendor>_hosted）。取值开放、只校验形状，见 register.go 的 agentHostingPattern。
	// 空串有两种含义，靠 AgentReportedHostingAt 区分：时间戳 NULL = 从未上报，
	// 时间戳非 NULL = 曾上报后被显式清空。自报值，仅供展示与排障，不可用于鉴权。
	AgentHosting string
	// AgentReportedHostingAt 最近一次收到 **agent_hosting** 上报的时间（SQL NOW()
	// 写入）；timestamp NULL，从未上报时无效。只在 hosting 被上报时前进 ——
	// 版本-only 的上报刷新它就等于替一份该次上报从未提及的数据背书新鲜度。
	// 必须用 NullTime 承接 NULL —— 否则 Select("*") 把 NULL 扫进 time.Time
	// 会报错，殃及所有 robot 查询（同 botfather 的 BoundAt）。
	AgentReportedHostingAt dbr.NullTime
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

// agentReport is a self-reported agent runtime update. A nil field means "the
// caller did not report this one", which is materially different from reporting
// an empty value — see updateRobotAgentInfo.
type agentReport struct {
	Platform *string
	Version  *string
	Plugin   *string
	Hosting  *string
}

// isEmpty reports whether no field was supplied at all. A cheap pre-check; the
// authoritative "is there anything to write" test is len(set)==0 inside
// updateRobotAgentInfo, because a supplied-but-empty legacy field is skipped
// there and so is not writable even though it was supplied.
func (r agentReport) isEmpty() bool {
	return r.Platform == nil && r.Version == nil && r.Plugin == nil && r.Hosting == nil
}

// updateRobotAgentInfo persists a self-reported agent runtime update.
//
// **A column is written only when the caller supplied a value for it.** An
// unsupplied field is absent from the statement, not resolved into the value the
// caller read a moment ago. The two look equivalent and are not:
// read-then-write-back turns two concurrent registers into a lost update —
// runtime A reads, runtime B writes a new agent_version, A writes the old one
// back. Nothing prevents two runtimes registering the same bot today (the
// occupancy lock is cooperative), and "report only agent_hosting" is a request
// shape this feature introduced, so the interleaving is reachable. Callers
// therefore must NOT substitute stored values for absent fields.
//
// **What counts as "supplied" differs between the legacy columns and the new
// one, and the asymmetry is deliberate:**
//
//   - agent_platform / agent_version / plugin_version: a non-empty value. These
//     predate this feature with the contract "empty means unchanged", where
//     omitting the field and sending "" were both no-ops. Honouring an explicit
//     "" as a clear would be a silent, repeating data loss: register is the
//     reconnect path, so any client whose serializer emits "" for a field it does
//     not populate would wipe that column on every reconnect, at HTTP 200 with
//     nothing in the log, leaving a row indistinguishable from one that never
//     reported. The first revision of this fix did exactly that while claiming in
//     a comment that it could not happen (PR #837 round 2, found independently by
//     both reviewers). Sparse writes and "empty means unchanged" are compatible:
//     the lost update came from substituting the value just read, not from
//     skipping the column.
//   - agent_hosting: any supplied value, including "". Reporting "" is the only
//     way a runtime can clear a stale hosting shape, and a stale shape is more
//     harmful than an absent one. Note this makes an explicit "" a genuine
//     contract change relative to the legacy columns — stated rather than
//     papered over.
//
// agent_reported_hosting_at is stamped **only when hosting was supplied**, and
// with SQL NOW() rather than Go's time.Now():
//
//   - Only on hosting reports, because its documented job is judging whether the
//     stored agent_hosting is still current. A version-only report refreshing it
//     would assert freshness for data that report never mentioned — and the
//     scenario where the two diverge (a new runtime that omits agent_hosting) is
//     exactly the one the pointer semantics exist to handle.
//   - With NOW(), because the value is rendered in an API response beside
//     bound_at, which modules/botfather/db.go writes with SQL NOW(). Go's
//     time.Now() goes through the driver's Config.Loc (UTC by default, and the
//     DSN never sets loc) while the app image pins TZ=Asia/Shanghai, so on a
//     MySQL session that is not UTC the two timestamps would sit in one response
//     eight hours apart with nothing to explain the gap. Production MySQL is UTC
//     today, so that was latent rather than live — reading the wall clock from
//     the same place as the column it is compared against removes the dependency
//     on the DB's session time zone altogether.
func (d *botAPIDB) updateRobotAgentInfo(robotID string, report agentReport) error {
	if report.isEmpty() {
		return nil
	}
	set := map[string]interface{}{}
	// The three legacy columns skip empty values as well as absent ones — see the
	// asymmetry note in the doc comment. Reporting "" for a version number has no
	// meaning, and treating it as a clear would destroy a stored value on every
	// reconnect for any client whose serializer emits "" for a field it does not
	// know about.
	if report.Platform != nil && *report.Platform != "" {
		set["agent_platform"] = *report.Platform
	}
	if report.Version != nil && *report.Version != "" {
		set["agent_version"] = *report.Version
	}
	if report.Plugin != nil && *report.Plugin != "" {
		set["plugin_version"] = *report.Plugin
	}
	if report.Hosting != nil {
		set["agent_hosting"] = *report.Hosting
		set["agent_reported_hosting_at"] = dbr.Expr("NOW()")
	}
	// Every field was present-but-empty on a legacy column: nothing to write, and
	// in particular no timestamp to advance. Checked after the map is built rather
	// than up front, because "supplied" and "writable" differ per column.
	if len(set) == 0 {
		return nil
	}
	_, err := d.session.Update("robot").SetMap(set).Where("robot_id=?", robotID).Exec()
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
		"SELECT sm.space_id FROM space_member sm INNER JOIN space s ON s.space_id = sm.space_id WHERE sm.uid=? AND sm.status=1 AND s.status=1 ORDER BY sm.created_at ASC, sm.space_id ASC",
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
func (d *botAPIDB) isBotSpaceAuthorized(robotID, spaceID string) (bool, error) {
	if robotID == "" || spaceID == "" {
		return false, nil
	}
	// (1) space_member path: active member row in the target active Space.
	var count int
	err := d.session.SelectBySql(
		"SELECT COUNT(*) FROM space_member sm INNER JOIN space s ON s.space_id = sm.space_id WHERE sm.uid=? AND sm.space_id=? AND sm.status=1 AND s.status=1",
		robotID, spaceID,
	).LoadOne(&count)
	if err != nil {
		return false, err
	}
	if count > 0 {
		return true, nil
	}
	// (2)+(3) app_bot path: published platform Bot in any active Space, OR
	// scope=space Bot whose own SpaceID matches the requested target Space.
	// Both branches require the target Space to be active.
	err = d.session.SelectBySql(
		"SELECT COUNT(*) FROM app_bot ab INNER JOIN space s ON s.space_id=? "+
			"WHERE ab.uid=? AND ab.status=1 AND s.status=1 "+
			"AND (ab.scope='platform' OR (ab.scope='space' AND ab.space_id=?))",
		spaceID, robotID, spaceID,
	).LoadOne(&count)
	if err != nil {
		return false, err
	}
	return count > 0, nil
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

// lookupEligibleSpacePrincipal performs the complete exact-Space authorization
// and target classification in one snapshot statement. Counts are intentional:
// duplicate/conflicting robot identities are treated as ineligible rather than
// selecting an arbitrary row.
type spacePrincipalEligibilityRow struct {
	SpaceActive int `db:"space_active"`

	CallerMember          int `db:"caller_member"`
	CallerUserBot         int `db:"caller_user_bot"`
	CallerAppBot          int `db:"caller_app_bot"`
	CallerCreatorEligible int `db:"caller_creator_eligible"`

	CanonicalUID          string `db:"canonical_uid"`
	TargetMember          int    `db:"target_member"`
	TargetHuman           int    `db:"target_human"`
	TargetRobotIdentity   int    `db:"target_robot_identity"`
	TargetUserBot         int    `db:"target_user_bot"`
	TargetAppBot          int    `db:"target_app_bot"`
	TargetCreatorEligible int    `db:"target_creator_eligible"`
}

// The user.robot predicates are an additional fail-closed consistency filter;
// robot remains the authoritative User Bot identity table.
const spacePrincipalEligibilityQuery = `SELECT
  (SELECT COUNT(*) FROM space s WHERE s.space_id=? AND s.status=1) AS space_active,
  (SELECT COUNT(*) FROM space_member sm WHERE sm.space_id=? AND sm.uid=? AND sm.status=1) AS caller_member,
  (SELECT COUNT(*) FROM robot r JOIN user u ON u.uid=r.robot_id AND u.status=1 AND COALESCE(u.is_destroy,0)<>2 AND u.robot=1 WHERE r.robot_id=? AND r.status=1) AS caller_user_bot,
  (SELECT COUNT(*) FROM app_bot ab WHERE ab.uid=?) AS caller_app_bot,
  (SELECT COUNT(*) FROM robot r JOIN user u ON u.uid=r.creator_uid AND u.status=1 AND COALESCE(u.is_destroy,0)<>2 AND u.robot=0 JOIN space_member sm ON sm.uid=r.creator_uid AND sm.space_id=? AND sm.status=1 WHERE r.robot_id=? AND r.status=1) AS caller_creator_eligible,
  COALESCE((SELECT u.uid FROM user u WHERE u.uid=? LIMIT 1),'') AS canonical_uid,
  (SELECT COUNT(*) FROM space_member sm WHERE sm.space_id=? AND sm.uid=? AND sm.status=1) AS target_member,
  (SELECT COUNT(*) FROM user u WHERE u.uid=? AND u.status=1 AND COALESCE(u.is_destroy,0)<>2 AND u.robot=0) AS target_human,
  (SELECT COUNT(*) FROM robot r WHERE r.robot_id=? AND r.status=1) AS target_robot_identity,
  (SELECT COUNT(*) FROM robot r JOIN user u ON u.uid=r.robot_id AND u.status=1 AND COALESCE(u.is_destroy,0)<>2 AND u.robot=1 WHERE r.robot_id=? AND r.status=1) AS target_user_bot,
  (SELECT COUNT(*) FROM app_bot ab WHERE ab.uid=?) AS target_app_bot,
  (SELECT COUNT(*) FROM robot r JOIN user u ON u.uid=r.creator_uid AND u.status=1 AND COALESCE(u.is_destroy,0)<>2 AND u.robot=0 JOIN space_member sm ON sm.uid=r.creator_uid AND sm.space_id=? AND sm.status=1 WHERE r.robot_id=? AND r.status=1) AS target_creator_eligible`

func (d *botAPIDB) lookupEligibleSpacePrincipal(callerUID, callerKind, spaceID, targetUID string) (*spacePrincipal, error) {
	var row spacePrincipalEligibilityRow
	err := d.session.QueryRow(spacePrincipalEligibilityQuery, spacePrincipalEligibilityArgs(callerUID, spaceID, targetUID)...).Scan(
		&row.SpaceActive,
		&row.CallerMember,
		&row.CallerUserBot,
		&row.CallerAppBot,
		&row.CallerCreatorEligible,
		&row.CanonicalUID,
		&row.TargetMember,
		&row.TargetHuman,
		&row.TargetRobotIdentity,
		&row.TargetUserBot,
		&row.TargetAppBot,
		&row.TargetCreatorEligible,
	)
	if err != nil {
		return nil, err
	}

	return classifyEligibleSpacePrincipal(callerKind, row)
}

func spacePrincipalEligibilityArgs(callerUID, spaceID, targetUID string) []interface{} {
	return []interface{}{
		spaceID,
		spaceID, callerUID,
		callerUID,
		callerUID,
		spaceID, callerUID,
		targetUID,
		spaceID, targetUID,
		targetUID,
		targetUID,
		targetUID,
		targetUID,
		spaceID, targetUID,
	}
}

func classifyEligibleSpacePrincipal(callerKind string, row spacePrincipalEligibilityRow) (*spacePrincipal, error) {
	if row.SpaceActive != 1 {
		return nil, errSpacePrincipalNotFound
	}
	if callerKind != BotKindUser || row.CallerMember != 1 || row.CallerUserBot != 1 || row.CallerAppBot != 0 || row.CallerCreatorEligible != 1 {
		return nil, errSpacePrincipalNotFound
	}
	if row.CanonicalUID == "" || row.TargetMember != 1 {
		return nil, errSpacePrincipalNotFound
	}

	botIdentities := row.TargetRobotIdentity + row.TargetAppBot
	switch {
	case row.TargetHuman == 1 && botIdentities == 0:
		return &spacePrincipal{UID: row.CanonicalUID, PrincipalType: principalTypeHuman}, nil
	case row.TargetHuman == 0 && row.TargetUserBot == 1 && row.TargetAppBot == 0 && row.TargetCreatorEligible == 1:
		return &spacePrincipal{UID: row.CanonicalUID, PrincipalType: principalTypeUserBot}, nil
	default:
		return nil, errSpacePrincipalNotFound
	}
}
