// Package aiteam contains leaf-level authority checks shared by group, message,
// and robot without introducing module import cycles.
package aiteam

import (
	"errors"
	"os"
	"strings"

	"github.com/gocraft/dbr/v2"
)

const (
	GroupPurpose       = "ai_session_container"
	TeamGroupPurpose   = "ai_team_group"
	TeamGroupName      = "我的AI团队"
	DefaultSessionName = "新对话"
)

var ErrContainerProtected = errors.New("ai session container is protected")

// Enabled gates AI routing as well as the public API. Container ACL protection
// intentionally does not use this flag: disabling rollout must never reopen an
// already-created private container to ordinary group mutation paths.
func Enabled() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("DM_AI_TEAM_ON")))
	return v == "1" || v == "true"
}

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
	purpose, err := Purpose(session, groupNo)
	return IsProtectedPurpose(purpose), err
}

// IsProtectedPurpose reports whether membership and ownership of a group are
// server-managed by AI Team rather than by ordinary group APIs.
func IsProtectedPurpose(purpose string) bool {
	return purpose == GroupPurpose || purpose == TeamGroupPurpose
}

// Purpose returns the persisted server-managed purpose, or an empty string for
// an ordinary or unknown group.
func Purpose(session *dbr.Session, groupNo string) (string, error) {
	if session == nil || strings.TrimSpace(groupNo) == "" {
		return "", nil
	}
	var purposes []string
	_, err := session.Select("purpose").From("`group`").Where("group_no=?", groupNo).Limit(1).Load(&purposes)
	if err != nil || len(purposes) == 0 {
		return "", err
	}
	return purposes[0], nil
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
		JOIN `+"`group`"+` g ON g.group_no=a.group_no COLLATE utf8mb4_0900_ai_ci AND g.purpose=? AND g.status=1
		JOIN robot r ON r.robot_id=a.bot_id COLLATE utf8mb4_0900_ai_ci AND r.status=1 AND r.creator_uid=a.user_uid COLLATE utf8mb4_0900_ai_ci
		JOIN user human_u ON human_u.uid=a.user_uid COLLATE utf8mb4_0900_ai_ci AND human_u.status=1 AND human_u.is_destroy<>2
		JOIN user bot_u ON bot_u.uid=a.bot_id COLLATE utf8mb4_0900_ai_ci AND bot_u.status=1 AND bot_u.is_destroy<>2
		JOIN space sp ON sp.space_id=a.space_id COLLATE utf8mb4_0900_ai_ci AND sp.status=1
		JOIN space_member human_sm ON human_sm.space_id=a.space_id COLLATE utf8mb4_0900_ai_ci AND human_sm.uid=a.user_uid COLLATE utf8mb4_0900_ai_ci AND human_sm.status=1
		JOIN space_member bot_sm ON bot_sm.space_id=a.space_id COLLATE utf8mb4_0900_ai_ci AND bot_sm.uid=a.bot_id COLLATE utf8mb4_0900_ai_ci AND bot_sm.status=1
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
		if item[1] != "2" && item[1] != "5" {
			continue
		}
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
