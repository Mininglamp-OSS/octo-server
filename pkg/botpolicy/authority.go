package botpolicy

import (
	"strings"

	"github.com/gocraft/dbr/v2"
)

type Queryer interface {
	SelectBySql(string, ...interface{}) *dbr.SelectStmt
}

type Identity struct {
	UID              string `db:"robot_id"`
	Kind             Kind   `db:"kind"`
	CreatorUID       string `db:"creator_uid"`
	Status           int    `db:"status"`
	Scope            string `db:"management_scope"`
	SpaceID          string `db:"management_space_id"`
	CreatedBy        string `db:"created_by"`
	PublicationState string `db:"publication_state"`
	LifecyclePending int    `db:"lifecycle_pending"`
}

func Lookup(q Queryer, uid string) (*Identity, error) {
	var identity *Identity
	_, err := q.SelectBySql(`SELECT robot_id,kind,creator_uid,status,management_scope,
		management_space_id,created_by,publication_state,lifecycle_pending FROM robot WHERE robot_id=?`, uid).Load(&identity)
	return identity, err
}

func (i *Identity) ActiveAvatar() bool {
	return i != nil && i.Kind == Avatar && i.CreatorUID == "" && i.Status == 1 &&
		i.PublicationState == Published && i.LifecyclePending == 0 && ((i.Scope == ScopePlatform && i.SpaceID == "") ||
		(i.Scope == ScopeSpace && i.SpaceID != ""))
}

// ResolveAvatarDMSharedSpace returns the deterministic active Space that
// authorizes a DM between an Avatar and a human. A non-empty hint narrows the
// answer to that Space; without one, the stable space_id order avoids attaching
// an unrelated platform-Avatar seat to the outbound message envelope.
func ResolveAvatarDMSharedSpace(q Queryer, uid, peerUID, spaceHint string) (string, error) {
	if uid == "" || peerUID == "" {
		return "", nil
	}
	var spaceIDs []string
	_, err := q.SelectBySql(`SELECT s.space_id FROM robot r
		JOIN user bot_u ON bot_u.uid=r.robot_id AND bot_u.status=1 AND bot_u.is_destroy<>2
		JOIN space_member bot_sm ON bot_sm.uid=r.robot_id AND bot_sm.status=1
		JOIN space s ON s.space_id=bot_sm.space_id AND s.status=1
		JOIN space_member human_sm ON human_sm.space_id=s.space_id AND human_sm.uid=? AND human_sm.status=1
		JOIN user human_u ON human_u.uid=human_sm.uid AND human_u.status=1 AND human_u.is_destroy<>2
		WHERE r.robot_id=? AND `+AvatarInSpaceSQL("r", "s.space_id")+`
		AND (?='' OR s.space_id=?) ORDER BY s.space_id LIMIT 1`,
		peerUID, uid, spaceHint, spaceHint).Load(&spaceIDs)
	if err != nil || len(spaceIDs) == 0 {
		return "", err
	}
	return spaceIDs[0], nil
}

// CanManageAvatar deliberately does not substitute CreatedBy for an owner.
// A Space administrator can manage only that Space's avatars, not platform bots.
func CanManageAvatar(q Queryer, identity *Identity, actor string, superadmin bool) (bool, error) {
	if identity == nil || identity.Kind != Avatar || actor == "" {
		return false, nil
	}
	if identity.Scope == ScopePlatform {
		return superadmin, nil
	}
	if identity.Scope != ScopeSpace || identity.SpaceID == "" {
		return false, nil
	}
	var count int
	err := q.SelectBySql(`SELECT COUNT(*) FROM space_member sm
		JOIN space s ON s.space_id=sm.space_id AND s.status=1
		JOIN user u ON u.uid=sm.uid AND u.status=1 AND u.is_destroy<>2
		WHERE sm.space_id=? AND sm.uid=? AND sm.status=1 AND sm.role IN (1,2)`, identity.SpaceID, actor).LoadOne(&count)
	return count == 1, err
}

// CanAccessChannel combines capability-independent resource authority with
// live lifecycle and tenant membership. No owner bypass applies to avatars.
func CanAccessChannel(q Queryer, uid, channelID string, channelType uint8, spaceHint string) (bool, error) {
	if uid == "" || channelID == "" {
		return false, nil
	}
	var count int
	if channelType == 1 {
		spaceID, err := ResolveAvatarDMSharedSpace(q, uid, channelID, spaceHint)
		return spaceID != "", err
	}
	groupNo, shortID := channelID, ""
	if channelType == 5 {
		parts := strings.Split(channelID, "____")
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return false, nil
		}
		groupNo, shortID = parts[0], parts[1]
	} else if channelType != 2 {
		return false, nil
	}
	err := q.SelectBySql(`SELECT COUNT(*) FROM robot r
		JOIN user bot_u ON bot_u.uid=r.robot_id AND bot_u.status=1 AND bot_u.is_destroy<>2
		JOIN group_member gm ON gm.uid=r.robot_id AND gm.group_no=? AND gm.status=1 AND gm.is_deleted=0 AND gm.is_external=0
		JOIN `+"`group`"+` g ON g.group_no=gm.group_no AND g.status=1
		JOIN space s ON s.space_id=g.space_id AND s.status=1
		JOIN space_member sm ON sm.space_id=s.space_id AND sm.uid=r.robot_id AND sm.status=1
		WHERE r.robot_id=? AND `+AvatarInSpaceSQL("r", "s.space_id")+`
		AND (?='' OR s.space_id=?)
		AND g.purpose IN (?, ?)
		AND (?='' OR EXISTS (SELECT 1 FROM thread t WHERE t.group_no=g.group_no AND t.short_id=? AND t.status<>3))
		AND (IFNULL(g.project_id,'')='' OR EXISTS (SELECT 1 FROM octo_project_member pm
			WHERE pm.project_id=g.project_id AND pm.uid=r.robot_id AND pm.status=1 AND pm.removing=0))
		AND ((g.purpose='ai_session_container' AND EXISTS (SELECT 1 FROM ai_team_agent a
			JOIN space_member owner_sm ON owner_sm.space_id=a.space_id COLLATE utf8mb4_0900_ai_ci
				AND owner_sm.uid=a.user_uid COLLATE utf8mb4_0900_ai_ci AND owner_sm.status=1
			JOIN user owner_u ON owner_u.uid=owner_sm.uid AND owner_u.status=1 AND owner_u.is_destroy<>2
			WHERE a.group_no=g.group_no COLLATE utf8mb4_0900_ai_ci AND a.bot_id=r.robot_id COLLATE utf8mb4_0900_ai_ci
			AND a.is_added=1 AND a.container_state=2)) OR
			(g.purpose='ai_team_group' AND EXISTS (SELECT 1 FROM ai_team_group tg
			JOIN ai_team_agent a ON a.space_id=tg.space_id COLLATE utf8mb4_0900_ai_ci
				AND a.user_uid=tg.user_uid COLLATE utf8mb4_0900_ai_ci
			WHERE tg.group_no=g.group_no COLLATE utf8mb4_0900_ai_ci AND tg.state=2
				AND a.bot_id=r.robot_id COLLATE utf8mb4_0900_ai_ci AND a.is_added=1
				AND a.container_state=2)))`,
		groupNo, uid, spaceHint, spaceHint, "ai_session_container", "ai_team_group", shortID, shortID).LoadOne(&count)
	return count == 1, err
}

// LockEnrollmentTx must precede any Space/robot locks in an enrollment write.
func LockEnrollmentTx(tx *dbr.Tx) error {
	if _, err := tx.InsertBySql("INSERT IGNORE INTO avatar_enrollment_lock (id) VALUES (1)").Exec(); err != nil {
		return err
	}
	var id int
	return tx.SelectBySql("SELECT id FROM avatar_enrollment_lock WHERE id=1 FOR UPDATE").LoadOne(&id)
}

// EnrollPlatformAvatarsTx materializes ordinary seats for a newly created Space.
// Caller holds LockEnrollmentTx. No group/project/AI membership is implied.
func EnrollPlatformAvatarsTx(tx *dbr.Tx, spaceID string) error {
	_, err := tx.InsertBySql(`INSERT INTO space_member (space_id,uid,role,status)
		SELECT ?,r.robot_id,0,1 FROM robot r WHERE `+ActiveAvatarSQL("r")+`
		AND r.management_scope='platform'
		ON DUPLICATE KEY UPDATE status=1,updated_at=NOW()`, spaceID).Exec()
	return err
}
