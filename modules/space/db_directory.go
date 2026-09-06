package space

import (
	"context"

	spacepkg "github.com/Mininglamp-OSS/octo-server/pkg/space"
)

const directoryAgentsPerOwner = 50

// queryDirectoryOwners loads every active human in a Space. The system-account
// exclusion is shared with the rest of the Space package; a system account can
// have robot=0, so robot=0 alone is not a sufficient human predicate.
func (d *DB) queryDirectoryOwners(ctx context.Context, spaceID string) ([]*directoryOwnerModel, error) {
	var owners []*directoryOwnerModel
	_, err := d.session.SelectBySql(`
		SELECT
			sm.uid,
			sm.role,
			IFNULL(u.name, '') AS name,
			IFNULL(uv.real_name, '') AS real_name
		FROM space_member sm
		INNER JOIN `+"`user`"+` u ON u.uid=sm.uid
		LEFT JOIN user_verification uv
			ON uv.user_id COLLATE utf8mb4_general_ci=sm.uid COLLATE utf8mb4_general_ci
		WHERE sm.space_id=?
			AND sm.status=1
			AND u.robot=0
			AND u.status=1
			AND COALESCE(u.is_destroy, 0)<>2
			AND sm.uid NOT IN ?
		ORDER BY sm.role DESC, sm.created_at ASC, sm.uid ASC
	`, spaceID, spacepkg.SystemBotList()).LoadContext(ctx, &owners)
	return owners, err
}

// queryDirectoryAgents returns at most directoryAgentsPerOwner details per
// eligible owner while retaining the exact count for that owner. agent_hosting
// is bot self-reported telemetry and therefore filters this presentation only;
// it must never be reused for authorization or other security decisions.
func (d *DB) queryDirectoryAgents(ctx context.Context, spaceID, loginUID string) ([]*directoryAgentModel, error) {
	var agents []*directoryAgentModel
	systemBots := spacepkg.SystemBotList()
	_, err := d.session.SelectBySql(`
		WITH candidate_agents AS (
			SELECT
				r.creator_uid,
				r.robot_id,
				COUNT(*) OVER (PARTITION BY r.creator_uid) AS agent_count,
				ROW_NUMBER() OVER (PARTITION BY r.creator_uid ORDER BY r.robot_id ASC) AS rn
			FROM space_member bot_sm
			INNER JOIN robot r
				ON r.robot_id=bot_sm.uid
				AND r.status=1
				AND r.agent_hosting<>'self_hosted'
			INNER JOIN `+"`user`"+` bot_u
				ON bot_u.uid=r.robot_id
				AND bot_u.robot=1
			INNER JOIN space_member owner_sm
				ON owner_sm.space_id=bot_sm.space_id
				AND owner_sm.uid=r.creator_uid
				AND owner_sm.status=1
			INNER JOIN `+"`user`"+` owner_u
				ON owner_u.uid=owner_sm.uid
				AND owner_u.robot=0
				AND owner_u.status=1
				AND COALESCE(owner_u.is_destroy, 0)<>2
			WHERE bot_sm.space_id=?
				AND bot_sm.status=1
				AND r.creator_uid<>''
				AND bot_sm.uid NOT IN ?
				AND owner_sm.uid NOT IN ?
		), selected_agents AS (
			SELECT creator_uid, robot_id, agent_count
			FROM candidate_agents
			WHERE rn<=?
		)
		SELECT
			sa.creator_uid,
			sa.robot_id AS uid,
			IFNULL(bot_u.name, '') AS name,
			IFNULL(r.description, '') AS description,
			CASE WHEN f.uid IS NULL THEN 0 ELSE 1 END AS is_friend,
			IFNULL(r.agent_hosting, '') AS hosting,
			r.agent_reported_hosting_at AS hosting_reported_at,
			sa.agent_count
		FROM selected_agents sa
		INNER JOIN robot r ON r.robot_id=sa.robot_id
		INNER JOIN `+"`user`"+` bot_u ON bot_u.uid=sa.robot_id
		LEFT JOIN friend f
			ON f.uid=?
			AND f.to_uid=sa.robot_id
			AND f.is_deleted=0
		ORDER BY sa.creator_uid ASC, sa.robot_id ASC
	`, spaceID, systemBots, systemBots, directoryAgentsPerOwner, loginUID).LoadContext(ctx, &agents)
	return agents, err
}
