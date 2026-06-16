package integration

import (
	"errors"
	"fmt"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-server/modules/user"
	"github.com/gocraft/dbr/v2"
)

type integrationDB struct {
	session *dbr.Session
}

func newIntegrationDB(ctx *config.Context) *integrationDB {
	return &integrationDB{session: ctx.DB()}
}

func (d *integrationDB) isClientEnabled(clientID string) (bool, error) {
	var status int
	err := d.session.Select("status").From("integration_client").
		Where("client_id=?", clientID).LoadOne(&status)
	if errors.Is(err, dbr.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("integration: query client %q: %w", clientID, err)
	}
	return status == 1, nil
}

func (d *integrationDB) isActiveUser(uid string) (bool, error) {
	if uid == "" {
		return false, nil
	}
	var n int
	err := d.session.Select("COUNT(*)").From("user").
		Where("uid=? AND status<>0 AND is_destroy=0", uid).
		LoadOne(&n)
	if err != nil {
		return false, fmt.Errorf("integration: query active user uid=%q: %w", uid, err)
	}
	return n > 0, nil
}

func (d *integrationDB) revokeUserAPIKey(id int64) error {
	_, err := d.session.Update("user_api_key").
		Set("status", 0).
		Set("revoked_at", dbr.Expr("NOW()")).
		Where("id=? AND status=1", id).
		Exec()
	if err != nil {
		return fmt.Errorf("integration: revoke user api key id=%d: %w", id, err)
	}
	return nil
}

func (d *integrationDB) upsertClient(clientID, name string, status int) error {
	tx, err := d.session.Begin()
	if err != nil {
		return fmt.Errorf("integration: begin upsert client %q: %w", clientID, err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	_, err = tx.InsertBySql(
		"INSERT INTO integration_client (client_id, name, status) VALUES (?, ?, ?) "+
			"ON DUPLICATE KEY UPDATE name=VALUES(name), status=VALUES(status)",
		clientID, name, status,
	).Exec()
	if err != nil {
		return fmt.Errorf("integration: upsert client %q: %w", clientID, err)
	}
	if status == 0 {
		_, err = tx.Update("user_api_key").
			Set("status", 0).
			Set("revoked_at", dbr.Expr("NOW()")).
			Where("client_id=? AND status=1", clientID).
			Exec()
		if err != nil {
			return fmt.Errorf("integration: revoke active keys for disabled client %q: %w", clientID, err)
		}
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("integration: commit upsert client %q: %w", clientID, err)
	}
	committed = true
	return nil
}

func (d *integrationDB) queryActiveSpaceName(spaceID string) (string, error) {
	if spaceID == "" {
		return "", nil
	}
	var name string
	err := d.session.Select("name").From("space").
		Where("space_id=? AND status=1", spaceID).LoadOne(&name)
	if errors.Is(err, dbr.ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("integration: query space name %q: %w", spaceID, err)
	}
	return name, nil
}

func (d *integrationDB) querySpaces(uid string) ([]spaceResp, error) {
	spaces := make([]spaceResp, 0)
	_, err := d.session.SelectBySql(`
		SELECT s.space_id,
		       s.name,
		       s.logo,
		       sm.role,
		       (SELECT COUNT(*) FROM space_member smc
		         WHERE smc.space_id=s.space_id AND smc.status=1) AS member_count
		FROM space_member sm
		INNER JOIN space s ON s.space_id=sm.space_id AND s.status=1
		WHERE sm.uid=? AND sm.status=1
		ORDER BY sm.created_at ASC, s.space_id ASC`,
		uid,
	).Load(&spaces)
	if err != nil {
		return nil, fmt.Errorf("integration: query spaces for uid=%q: %w", uid, err)
	}
	if len(spaces) == 0 {
		return spaces, nil
	}
	spaces[0].IsDefault = true

	spaceIDs := make([]string, 0, len(spaces))
	for _, sp := range spaces {
		spaceIDs = append(spaceIDs, sp.SpaceID)
	}
	available, err := d.queryAvailableBotSpaces(uid, spaceIDs)
	if err != nil {
		return nil, err
	}
	for i := range spaces {
		spaces[i].HasAvailableBot = available[spaces[i].SpaceID]
	}
	return spaces, nil
}

func (d *integrationDB) queryAvailableBotSpaces(uid string, spaceIDs []string) (map[string]bool, error) {
	out := make(map[string]bool)
	if uid == "" || len(spaceIDs) == 0 {
		return out, nil
	}
	var ids []string
	_, err := d.session.SelectBySql(`
		SELECT sm.space_id
		FROM robot r
		INNER JOIN space_member sm ON sm.uid=r.robot_id AND sm.status=1
		WHERE r.creator_uid=? AND r.status=1 AND r.bound_agent_ref='' AND sm.space_id IN ?
		GROUP BY sm.space_id`,
		uid, spaceIDs,
	).Load(&ids)
	if err != nil {
		return nil, fmt.Errorf("integration: query available bot spaces uid=%q: %w", uid, err)
	}
	for _, id := range ids {
		out[id] = true
	}
	return out, nil
}

// queryGroupCreatedAt 读取群的真实建群时间（time.Time）。CreateGroupServiceResp 不含
// created_at，且 group.IService.GetGroupWithGroupNo 的 InfoResp.CreatedAt 是 time.Time.String()
// 格式（非 RFC3339），故这里直读原始列，由调用方 .UTC().Format(time.RFC3339) 输出。按 space_id
// 一并限定，与同模块 queryGroupStatus 的 Space 隔离口径一致（防御性，避免跨 Space 直读）。
func (d *integrationDB) queryGroupCreatedAt(groupNo, spaceID string) (time.Time, error) {
	var createdAt time.Time
	err := d.session.Select("created_at").From("`group`").
		Where("group_no=? AND space_id=?", groupNo, spaceID).LoadOne(&createdAt)
	if err != nil {
		return time.Time{}, fmt.Errorf("integration: query group created_at %q: %w", groupNo, err)
	}
	return createdAt, nil
}

// queryGroupStatus returns the group's status and creator, scoped to the given space, and
// explicitly distinguishes "group not in this space / not found" (found=false, err=nil) from
// a DB failure (err!=nil). Scoping by space_id keeps the existence check inside the uk_ key's
// space binding — a key for space A must not confirm groups that live in space B. The creator
// is returned so the existence check can be owner-scoped (creator==uid), not merely
// member-scoped.
func (d *integrationDB) queryGroupStatus(groupNo, spaceID string) (status int, creator string, found bool, err error) {
	var row struct {
		Status  int    `db:"status"`
		Creator string `db:"creator"`
	}
	e := d.session.Select("status", "creator").From("`group`").
		Where("group_no=? AND space_id=?", groupNo, spaceID).LoadOne(&row)
	if errors.Is(e, dbr.ErrNotFound) {
		return 0, "", false, nil
	}
	if e != nil {
		return 0, "", false, fmt.Errorf("integration: query group status %q: %w", groupNo, e)
	}
	return row.Status, row.Creator, true, nil
}

// isHumanUser reports whether uid is a human account (user row exists with robot=0). Bots
// never obtain a uk_ key (they don't go through OIDC exchange) and AuthByKey only checks
// account activity, not the robot flag — so this is the guard that keeps a team group's
// owner/creator always a human member.
func (d *integrationDB) isHumanUser(uid string) (bool, error) {
	if uid == "" {
		return false, nil
	}
	var robot int
	err := d.session.Select("robot").From("user").Where("uid=?", uid).LoadOne(&robot)
	if errors.Is(err, dbr.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("integration: query user robot flag uid=%q: %w", uid, err)
	}
	return robot == 0, nil
}

// queryOwnedActiveBotIDs returns the eligible team-bot set for this owner+space: owned
// (creator_uid) and active (robot.status=1), active in the space (space_member.status=1), AND
// backed by a usable user row — it exists and is not finalized-destroyed
// (is_destroy<>IsDestroyDone). Validating member_robot_ids against this set up front turns an
// unusable bot into a 404 *before* any group is created, instead of a created-then-500 that
// leaks an orphaned group.
//
// This is a SUPERSET-tightening of CreateGroup's own insertion filter, not a byte-for-byte
// mirror: CreateGroup inserts from userDB.QueryByUIDs (existence) + an IsDestroyDone skip, and
// does NOT itself re-check robot.status=1 / space_member.status=1. The overlapping, load-bearing
// predicate is "user row exists and is not finalized-destroyed"; the extra robot/space-active
// joins here only further restrict the set, so a bot that passes this query is always insertable
// by CreateGroup. (The remaining gap is a destroy racing in between validation and insert — a
// narrow TOCTOU the response's actual-members read-back already tolerates.)
//
// Intentionally does NOT filter on robot.bound_agent_ref (unlike the sibling
// queryAvailableBotSpaces, which lists only unoccupied bots for the exchange picker):
// agent-occupation and team-group membership are orthogonal — a user may put their own bot in
// a team group regardless of whether it is currently bound to an agent.
func (d *integrationDB) queryOwnedActiveBotIDs(uid, spaceID string) (map[string]bool, error) {
	out := make(map[string]bool)
	if uid == "" || spaceID == "" {
		return out, nil
	}
	var ids []string
	_, err := d.session.SelectBySql(`
		SELECT r.robot_id
		FROM robot r
		INNER JOIN space_member sm ON sm.uid=r.robot_id AND sm.space_id=? AND sm.status=1
		INNER JOIN `+"`user`"+` u ON u.uid=r.robot_id AND u.is_destroy<>?
		WHERE r.creator_uid=? AND r.status=1`,
		spaceID, user.IsDestroyDone, uid,
	).Load(&ids)
	if err != nil {
		return nil, fmt.Errorf("integration: query owned active bots uid=%q space=%q: %w", uid, spaceID, err)
	}
	for _, id := range ids {
		out[id] = true
	}
	return out, nil
}

func (d *integrationDB) queryBots(uid, spaceID string) ([]exchangeBotResp, error) {
	var rows []struct {
		RobotID     string
		Username    string
		Name        string
		Description string
		CreatedAt   time.Time
	}
	_, err := d.session.SelectBySql(`
		SELECT r.robot_id,
		       r.username,
		       COALESCE(NULLIF(u.name, ''), r.username, r.robot_id) AS name,
		       r.description,
		       r.created_at
		FROM robot r
		INNER JOIN space_member sm ON sm.uid=r.robot_id AND sm.space_id=? AND sm.status=1
		LEFT JOIN user u ON u.uid=r.robot_id AND u.status=1
		WHERE r.creator_uid=? AND r.status=1
		ORDER BY r.created_at DESC, r.robot_id ASC`,
		spaceID, uid,
	).Load(&rows)
	if err != nil {
		return nil, fmt.Errorf("integration: query bots uid=%q space=%q: %w", uid, spaceID, err)
	}
	bots := make([]exchangeBotResp, 0, len(rows))
	for _, row := range rows {
		bots = append(bots, exchangeBotResp{
			RobotID:     row.RobotID,
			Username:    row.Username,
			Name:        row.Name,
			Description: row.Description,
			CreatedAt:   row.CreatedAt.Format(time.RFC3339),
		})
	}
	return bots, nil
}
