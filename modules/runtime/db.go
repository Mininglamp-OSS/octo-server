package runtime

import (
	"fmt"
	"strings"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/gocraft/dbr/v2"
)

type runtimeDB struct {
	session *dbr.Session
	ctx     *config.Context
}

func newRuntimeDB(ctx *config.Context) *runtimeDB {
	return &runtimeDB{
		ctx:     ctx,
		session: ctx.DB(),
	}
}

func (d *runtimeDB) upsert(m *agentRuntimeModel) (int64, error) {
	result, err := d.session.InsertBySql(`
		INSERT INTO agent_runtime (space_id, daemon_id, name, provider, runtime_mode, status, version, device_name, device_info, metadata, owner_uid, last_seen_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NOW())
		ON DUPLICATE KEY UPDATE
			name=VALUES(name), status=VALUES(status), version=VALUES(version),
			device_name=VALUES(device_name), device_info=VALUES(device_info),
			metadata=VALUES(metadata), last_seen_at=NOW()`,
		m.SpaceID, m.DaemonID, m.Name, m.Provider, m.RuntimeMode,
		m.Status, m.Version, m.DeviceName, m.DeviceInfo, m.Metadata, m.OwnerUID,
	).Exec()
	if err != nil {
		return 0, err
	}

	id, err := result.LastInsertId()
	if err != nil {
		return 0, err
	}
	if id == 0 {
		existing, err := d.queryByUniqueKey(m.SpaceID, m.DaemonID, m.Provider)
		if err != nil {
			return 0, err
		}
		if existing != nil {
			id = existing.Id
		}
	}
	return id, nil
}

func (d *runtimeDB) queryByUniqueKey(spaceID, daemonID, provider string) (*agentRuntimeModel, error) {
	var m *agentRuntimeModel
	_, err := d.session.Select("*").From("agent_runtime").
		Where("space_id=? AND daemon_id=? AND provider=?", spaceID, daemonID, provider).
		Load(&m)
	return m, err
}

func (d *runtimeDB) queryByID(id int64) (*agentRuntimeModel, error) {
	var m *agentRuntimeModel
	_, err := d.session.Select("*").From("agent_runtime").
		Where("id=?", id).
		Load(&m)
	return m, err
}

func (d *runtimeDB) listBySpaceIDAndOwner(spaceID, ownerUID string) ([]*agentRuntimeModel, error) {
	var list []*agentRuntimeModel
	_, err := d.session.Select("*").From("agent_runtime").
		Where("space_id=? AND owner_uid=?", spaceID, ownerUID).
		OrderDir("status", false).
		OrderAsc("name").
		Load(&list)
	return list, err
}

func (d *runtimeDB) updateHeartbeat(id int64) error {
	_, err := d.session.Update("agent_runtime").
		Set("status", "online").
		Set("last_seen_at", dbr.Expr("NOW()")).
		Where("id=?", id).
		Exec()
	return err
}

func (d *runtimeDB) setOffline(ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := d.session.Update("agent_runtime").
		Set("status", "offline").
		Where("id IN ?", ids).
		Exec()
	return err
}

func (d *runtimeDB) markStaleOffline(threshold time.Duration) (int64, error) {
	cutoff := time.Now().Add(-threshold)
	result, err := d.session.Update("agent_runtime").
		Set("status", "offline").
		Where("status=? AND last_seen_at < ?", "online", cutoff).
		Exec()
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

func (d *runtimeDB) deleteStaleOffline(threshold time.Duration) (int64, error) {
	cutoff := time.Now().Add(-threshold)
	result, err := d.session.DeleteFrom("agent_runtime").
		Where("status=? AND last_seen_at < ?", "offline", cutoff).
		Exec()
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

func (d *runtimeDB) deleteByID(id int64) error {
	_, err := d.session.DeleteFrom("agent_runtime").
		Where("id=?", id).
		Exec()
	return err
}

func (d *runtimeDB) deleteBySpaceAndDaemon(spaceID, daemonID string) error {
	_, err := d.session.DeleteFrom("agent_runtime").
		Where("space_id=? AND daemon_id=?", spaceID, daemonID).
		Exec()
	return err
}

func (d *runtimeDB) queryLatestVersions() (map[string]string, error) {
	var rows []struct {
		Component     string `db:"component"`
		LatestVersion string `db:"latest_version"`
	}
	_, err := d.session.Select("component", "latest_version").From("runtime_latest_version").Load(&rows)
	if err != nil {
		return nil, err
	}
	result := make(map[string]string, len(rows))
	for _, r := range rows {
		result[r.Component] = r.LatestVersion
	}
	return result, nil
}

// upsertLatestVersion inserts or updates a component's latest version + release_meta.
// Called by version syncer after pulling version.json from COS.
func (d *runtimeDB) upsertLatestVersion(component, latestVersion, releaseMeta string) error {
	_, err := d.session.InsertBySql(
		`INSERT INTO runtime_latest_version (component, latest_version, release_meta)
		 VALUES (?, ?, ?)
		 ON DUPLICATE KEY UPDATE latest_version=VALUES(latest_version), release_meta=VALUES(release_meta)`,
		component, latestVersion, releaseMeta,
	).Exec()
	return err
}

// Ping DB operations

func (d *runtimeDB) insertPing(p *pingEntry) error {
	_, err := d.session.InsertBySql(
		`INSERT INTO runtime_ping (id, space_id, daemon_id, server_ts, status) VALUES (?, ?, ?, ?, ?)`,
		p.ID, p.SpaceID, p.DaemonID, p.ServerTS, p.Status,
	).Exec()
	return err
}

// claimPendingPing atomically claims a pending ping by setting status to 'dispatched'.
// Returns the claimed ping entry, or nil if none pending.
func (d *runtimeDB) claimPendingPing(spaceID, daemonID string, dispatchTS int64) (*pingEntry, error) {
	// Atomic: only one heartbeat can claim each ping
	result, err := d.session.UpdateBySql(
		`UPDATE runtime_ping SET status='dispatched', server_ts=? WHERE space_id=? AND daemon_id=? AND status='pending' ORDER BY created_at DESC LIMIT 1`,
		dispatchTS, spaceID, daemonID,
	).Exec()
	if err != nil {
		return nil, err
	}
	rows, _ := result.RowsAffected()
	if rows == 0 {
		return nil, nil
	}
	// Fetch the one we just claimed
	var p *pingEntry
	_, err = d.session.SelectBySql(
		`SELECT id, space_id, daemon_id, server_ts, daemon_ts, rtt_ms, status FROM runtime_ping WHERE space_id=? AND daemon_id=? AND status='dispatched' ORDER BY created_at DESC LIMIT 1`,
		spaceID, daemonID,
	).Load(&p)
	return p, err
}

func (d *runtimeDB) getPing(pingID string) (*pingEntry, error) {
	var p *pingEntry
	_, err := d.session.SelectBySql(
		`SELECT id, space_id, daemon_id, server_ts, daemon_ts, rtt_ms, status FROM runtime_ping WHERE id=?`,
		pingID,
	).Load(&p)
	return p, err
}

func (d *runtimeDB) updatePingResult(pingID string, daemonTS, rtt int64) error {
	_, err := d.session.UpdateBySql(
		`UPDATE runtime_ping SET daemon_ts=?, rtt_ms=?, status='done' WHERE id=? AND status='dispatched'`,
		daemonTS, rtt, pingID,
	).Exec()
	return err
}

func (d *runtimeDB) timeoutPing(pingID string) error {
	_, err := d.session.UpdateBySql(
		`UPDATE runtime_ping SET status='timeout' WHERE id=? AND status='pending'`,
		pingID,
	).Exec()
	return err
}

func (d *runtimeDB) timeoutStalePending() {
	d.session.UpdateBySql(
		`UPDATE runtime_ping SET status='timeout' WHERE status IN ('pending','dispatched') AND created_at < DATE_SUB(NOW(), INTERVAL 60 SECOND)`,
	).Exec()
}

func (d *runtimeDB) cleanOldPings(maxAge time.Duration) (int64, error) {
	d.timeoutStalePending()
	cutoff := time.Now().Add(-maxAge)
	result, err := d.session.DeleteBySql(
		`DELETE FROM runtime_ping WHERE status NOT IN ('pending','dispatched') AND created_at < ?`,
		cutoff,
	).Exec()
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return fmt.Sprintf("%d-%02d-%02d %02d:%02d:%02d",
		t.Year(), t.Month(), t.Day(),
		t.Hour(), t.Minute(), t.Second())
}

// queryBotInfoByUIDs 查询合法 bot（robot.status=1 + user.robot=1 + 属于当前 space）的显示信息。
// 入参 uids 已 dedupe；只返回合法 bot，不合法 / 跨 space 的 uid 不在结果里。
// 这是为 /v1/runtimes 的 route enrich 服务的，防止 daemon 上报任意 uid 被 enrich 出 user 真名。
// 表名 `user` 反引号包起来，避免 MySQL 对保留字解析差异。
func (d *runtimeDB) queryBotInfoByUIDs(spaceID string, uids []string) (map[string]botInfo, error) {
	if len(uids) == 0 || spaceID == "" {
		return map[string]botInfo{}, nil
	}
	// dbr 的 IN 参数需要 []interface{}
	args := make([]interface{}, 0, len(uids)+1)
	args = append(args, spaceID)
	placeholders := make([]string, len(uids))
	for i, uid := range uids {
		placeholders[i] = "?"
		args = append(args, uid)
	}
	sql := fmt.Sprintf(
		"SELECT u.uid, u.name FROM `user` u "+
			"INNER JOIN robot r ON r.robot_id = u.uid AND r.status = 1 "+
			"INNER JOIN space_member sm ON sm.uid = u.uid AND sm.space_id = ? AND sm.status = 1 "+
			"WHERE u.robot = 1 AND u.uid IN (%s)",
		strings.Join(placeholders, ","),
	)
	var rows []struct {
		UID  string `db:"uid"`
		Name string `db:"name"`
	}
	_, err := d.session.SelectBySql(sql, args...).Load(&rows)
	if err != nil {
		return nil, err
	}
	result := make(map[string]botInfo, len(rows))
	for _, r := range rows {
		result[r.UID] = botInfo{UID: r.UID, Name: r.Name}
	}
	return result, nil
}
