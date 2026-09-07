// Package aiteam contains leaf-level authority checks shared by group, message,
// and robot without introducing module import cycles.
package aiteam

import (
	"errors"
	"strings"

	"github.com/gocraft/dbr/v2"
)

const GroupPurpose = "ai_session_container"

var ErrContainerProtected = errors.New("ai session container is protected")

type SessionTarget struct {
	AgentID     int64  `db:"agent_id"`
	SpaceID     string `db:"space_id"`
	UserUID     string `db:"user_uid"`
	BotID       string `db:"bot_id"`
	GroupNo     string `db:"group_no"`
	ShortID     string `db:"short_id"`
	ManualTitle int    `db:"manual_title"`
}

func IsProtectedGroup(session *dbr.Session, groupNo string) (bool, error) {
	if session == nil || strings.TrimSpace(groupNo) == "" {
		return false, nil
	}
	var count int
	err := session.Select("COUNT(*)").From("`group`").
		Where("group_no=? AND purpose=?", groupNo, GroupPurpose).LoadOne(&count)
	return count > 0, err
}

// LookupReadySessionTarget resolves automatic Bot delivery exclusively from
// persisted authority. is_added is intentionally not a predicate: removing an
// AI hides it from the picker but does not terminate an already-open session.
func LookupReadySessionTarget(session *dbr.Session, channelID, senderUID string) (*SessionTarget, error) {
	parts := strings.Split(channelID, "____")
	if session == nil || len(parts) != 2 || parts[0] == "" || parts[1] == "" || senderUID == "" {
		return nil, nil
	}
	var target *SessionTarget
	_, err := session.SelectBySql(`
		SELECT a.id AS agent_id, a.space_id, a.user_uid, a.bot_id,
		       a.group_no, s.short_id, s.manual_title
		FROM ai_team_agent a
		JOIN ai_team_session s ON s.agent_id=a.id AND s.state=2
		JOIN thread t ON t.short_id=s.short_id AND t.group_no=a.group_no AND t.status<>3
		JOIN `+"`group`"+` g ON g.group_no=a.group_no AND g.purpose=? AND g.status=1
		JOIN robot r ON r.robot_id=a.bot_id AND r.status=1 AND r.creator_uid=a.user_uid
		JOIN user human_u ON human_u.uid=a.user_uid AND human_u.status=1 AND human_u.is_destroy<>2
		JOIN user bot_u ON bot_u.uid=a.bot_id AND bot_u.status=1 AND bot_u.is_destroy<>2
		JOIN space sp ON sp.space_id=a.space_id AND sp.status=1
		JOIN space_member human_sm ON human_sm.space_id=a.space_id AND human_sm.uid=a.user_uid AND human_sm.status=1
		JOIN space_member bot_sm ON bot_sm.space_id=a.space_id AND bot_sm.uid=a.bot_id AND bot_sm.status=1
		WHERE a.group_no=? AND s.short_id=? AND a.user_uid=?
		LIMIT 1`, GroupPurpose, parts[0], parts[1], senderUID).Load(&target)
	return target, err
}

func RuntimeSessionKey(target *SessionTarget, channelID string) string {
	if target == nil {
		return ""
	}
	return target.SpaceID + ":" + target.BotID + ":" + channelID
}

// ExcludeProtectedItems filters both a dedicated parent and its thread channel.
// channelType values follow WuKongIM: group=2, community-topic=5.
func ExcludeProtectedItems(session *dbr.Session, items [][2]string) (map[string]struct{}, error) {
	groupNos := make([]string, 0, len(items))
	seen := make(map[string]struct{}, len(items))
	for _, item := range items {
		groupNo := item[0]
		if item[1] == "5" {
			parts := strings.Split(groupNo, "____")
			if len(parts) == 2 {
				groupNo = parts[0]
			}
		}
		if groupNo != "" {
			if _, ok := seen[groupNo]; !ok {
				seen[groupNo] = struct{}{}
				groupNos = append(groupNos, groupNo)
			}
		}
	}
	protected := make(map[string]struct{})
	if len(groupNos) == 0 {
		return protected, nil
	}
	var rows []struct {
		GroupNo string `db:"group_no"`
	}
	_, err := session.Select("group_no").From("`group`").
		Where("group_no IN ? AND purpose=?", groupNos, GroupPurpose).Load(&rows)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		protected[row.GroupNo] = struct{}{}
	}
	return protected, nil
}
