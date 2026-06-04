package integration

import (
	"errors"
	"fmt"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
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
