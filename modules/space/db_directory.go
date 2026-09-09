package space

import (
	"context"

	spacepkg "github.com/Mininglamp-OSS/octo-server/pkg/space"
)

const directoryAgentsPerOwner = 50

// directoryAgentHostingOctoHosted is the only self-reported hosting value the
// Space directory treats as a visible cloud agent. This is not an authz
// signal: any holder of the bot's bf_ token can claim the value.
const directoryAgentHostingOctoHosted = "octo_hosted"

// The legacy user and space_member tables can carry a different collation from
// migration-created user_verification. Pin the completed display expression so
// keyword matching remains valid on both schema shapes without changing the
// DisplayName fallback order.
const directoryOwnerDisplayNameExpr = "(COALESCE(NULLIF(u.name, ''), NULLIF(uv.real_name, ''), CONCAT('" + memberDisplayNamePlaceholderPrefix + "', sm.uid)) COLLATE utf8mb4_general_ci)"

// queryDirectoryOwners loads every active human in a Space. The system-account
// exclusion is shared with the rest of the Space package; a system account can
// have robot=0, so robot=0 alone is not a sufficient human predicate.
func (d *DB) queryDirectoryOwners(ctx context.Context, spaceID, keyword string) ([]*directoryOwnerModel, error) {
	var owners []*directoryOwnerModel
	systemBots := spacepkg.SystemBotList()
	args := make([]interface{}, 0, 8)
	query := `
		SELECT
			sm.uid,
			sm.role,
			IFNULL(u.name, '') AS name,
			IFNULL(uv.real_name, '') AS real_name
		FROM space_member sm
		INNER JOIN ` + "`user`" + ` u ON u.uid=sm.uid
		LEFT JOIN user_verification uv
			ON uv.user_id=sm.uid COLLATE utf8mb4_general_ci
	`
	if keyword != "" {
		query += `
		LEFT JOIN (
			SELECT DISTINCT r.creator_uid
			FROM space_member bot_sm
			INNER JOIN robot r
				ON r.robot_id=bot_sm.uid
				AND r.status=1
				AND r.agent_hosting='` + directoryAgentHostingOctoHosted + `'
			INNER JOIN ` + "`user`" + ` bot_u
				ON bot_u.uid=r.robot_id
				AND bot_u.robot=1
			INNER JOIN space_member owner_sm
				ON owner_sm.space_id=bot_sm.space_id
				AND owner_sm.uid=r.creator_uid
				AND owner_sm.status=1
			INNER JOIN ` + "`user`" + ` owner_u
				ON owner_u.uid=owner_sm.uid
				AND owner_u.robot=0
				AND owner_u.status=1
				AND COALESCE(owner_u.is_destroy, 0)<>2
			WHERE bot_sm.space_id=?
				AND bot_sm.status=1
				AND r.creator_uid<>''
				AND bot_sm.uid NOT IN ?
				AND owner_sm.uid NOT IN ?
				AND IFNULL(bot_u.name, '') LIKE ?` + likeEscapeClause + `
		) matched_bot ON matched_bot.creator_uid=sm.uid
		`
		args = append(args, spaceID, systemBots, systemBots, buildLikePattern(keyword))
	}
	query += `
		WHERE sm.space_id=?
			AND sm.status=1
			AND u.robot=0
			AND u.status=1
			AND COALESCE(u.is_destroy, 0)<>2
			AND sm.uid NOT IN ?
	`
	args = append(args, spaceID, systemBots)
	if keyword != "" {
		query += `
			AND (` + directoryOwnerDisplayNameExpr + ` LIKE ?` + likeEscapeClause + `
				OR matched_bot.creator_uid IS NOT NULL
			)
		`
		args = append(args, buildLikePattern(keyword))
	}
	query += `
		ORDER BY sm.role DESC, sm.created_at ASC, sm.uid ASC
	`
	_, err := d.session.SelectBySql(query, args...).LoadContext(ctx, &owners)
	return owners, err
}

// queryDirectoryAgents returns at most directoryAgentsPerOwner details per
// eligible owner while retaining the exact count for that owner. Only
// octo_hosted bots with an eligible human owner in this Space are visible.
// agent_hosting is bot self-reported telemetry and therefore filters this
// presentation only; it must never be reused for authorization or other
// security decisions.
func (d *DB) queryDirectoryAgents(ctx context.Context, spaceID, loginUID, keyword string) ([]*directoryAgentModel, error) {
	var agents []*directoryAgentModel
	systemBots := spacepkg.SystemBotList()
	args := []interface{}{spaceID, systemBots, systemBots}
	query := `
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
				AND r.agent_hosting='` + directoryAgentHostingOctoHosted + `'
			INNER JOIN ` + "`user`" + ` bot_u
				ON bot_u.uid=r.robot_id
				AND bot_u.robot=1
			INNER JOIN space_member owner_sm
				ON owner_sm.space_id=bot_sm.space_id
				AND owner_sm.uid=r.creator_uid
				AND owner_sm.status=1
			INNER JOIN ` + "`user`" + ` owner_u
				ON owner_u.uid=owner_sm.uid
				AND owner_u.robot=0
				AND owner_u.status=1
				AND COALESCE(owner_u.is_destroy, 0)<>2
			WHERE bot_sm.space_id=?
				AND bot_sm.status=1
				AND r.creator_uid<>''
				AND bot_sm.uid NOT IN ?
				AND owner_sm.uid NOT IN ?
	`
	if keyword != "" {
		query += `
				AND IFNULL(bot_u.name, '') LIKE ?` + likeEscapeClause + `
		`
		args = append(args, buildLikePattern(keyword))
	}
	query += `
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
		INNER JOIN ` + "`user`" + ` bot_u ON bot_u.uid=sa.robot_id
		LEFT JOIN friend f
			ON f.uid=?
			AND f.to_uid=sa.robot_id
			AND f.is_deleted=0
		ORDER BY sa.creator_uid ASC, sa.robot_id ASC
	`
	args = append(args, directoryAgentsPerOwner, loginUID)
	_, err := d.session.SelectBySql(query, args...).LoadContext(ctx, &agents)
	return agents, err
}

// queryDirectoryAvatars returns published organization-managed identities as
// their own directory section. They have no human creator, so attaching them
// to queryDirectoryOwners would either hide them or invent ownership.
func (d *DB) queryDirectoryAvatars(ctx context.Context, spaceID, loginUID, keyword string) ([]*directoryAgentModel, error) {
	query := `SELECT '' AS creator_uid,r.robot_id AS uid,IFNULL(u.name,'') AS name,
		IFNULL(r.description,'') AS description,CASE WHEN f.uid IS NULL THEN 0 ELSE 1 END AS is_friend,
		'' AS hosting,NULL AS hosting_reported_at,0 AS agent_count
		FROM robot r
		JOIN user u ON u.uid=r.robot_id AND u.robot=1 AND u.status=1 AND u.is_destroy<>2
		JOIN space_member sm ON sm.uid=r.robot_id AND sm.space_id=? AND sm.status=1
		JOIN space s ON s.space_id=sm.space_id AND s.status=1
		LEFT JOIN friend f ON f.uid=? AND f.to_uid=r.robot_id AND f.is_deleted=0
		WHERE r.kind='avatar' AND r.status=1 AND r.creator_uid=''
		AND r.publication_state='published' AND r.lifecycle_pending=0
		AND ((r.management_scope='platform' AND r.management_space_id='') OR
			(r.management_scope='space' AND r.management_space_id=s.space_id))`
	// The placeholders are the requested Space first, then the viewer UID.
	args := []interface{}{spaceID, loginUID}
	if keyword != "" {
		query += ` AND IFNULL(u.name,'') LIKE ?` + likeEscapeClause
		args = append(args, buildLikePattern(keyword))
	}
	query += ` ORDER BY r.robot_id ASC`
	var avatars []*directoryAgentModel
	_, err := d.session.SelectBySql(query, args...).LoadContext(ctx, &avatars)
	return avatars, err
}
