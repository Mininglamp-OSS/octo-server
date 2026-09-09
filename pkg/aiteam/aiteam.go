// Package aiteam contains leaf-level authority checks shared by group, message,
// and robot without introducing module import cycles.
package aiteam

import (
	"errors"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/Mininglamp-OSS/octo-server/pkg/botpolicy"
	"github.com/gocraft/dbr/v2"
)

const (
	GroupPurpose       = "ai_session_container"
	TeamGroupPurpose   = "ai_team_group"
	CustomTeamPurpose  = "ai_custom_team_group"
	TeamGroupName      = "我的 OPT"
	DefaultSessionName = "新对话"
)

var ErrContainerProtected = errors.New("ai session container is protected")

type LifecycleRosterMarker func(
	tx *dbr.Tx,
	purpose, groupNo, spaceID, ownerUID string,
	removedUIDs []string,
) error

var lifecycleRosterMarker LifecycleRosterMarker

func RegisterLifecycleRosterMarker(marker LifecycleRosterMarker) {
	lifecycleRosterMarker = marker
}

func MarkLifecycleRosterRemovalTx(
	tx *dbr.Tx,
	purpose, groupNo, spaceID, ownerUID string,
	removedUIDs []string,
) error {
	if lifecycleRosterMarker == nil {
		return nil
	}
	return lifecycleRosterMarker(tx, purpose, groupNo, spaceID, ownerUID, removedUIDs)
}

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
	return purpose == GroupPurpose || purpose == TeamGroupPurpose || purpose == CustomTeamPurpose
}

// IsHiddenPurpose identifies AI-team groups which must stay out of ordinary
// group and conversation surfaces. They are exclusively presented through the
// AI Team API and page, including user-created custom teams.
func IsHiddenPurpose(purpose string) bool {
	return purpose == GroupPurpose || purpose == TeamGroupPurpose || purpose == CustomTeamPurpose
}

// IsImmutablePurpose identifies AI groups whose metadata and roster are fully
// server-managed. A custom AI team has a protected roster but user-managed
// metadata, so it is intentionally excluded.
func IsImmutablePurpose(purpose string) bool {
	return purpose == GroupPurpose || purpose == TeamGroupPurpose
}

// IsDedicatedSessionPurpose reports whether a group is the private owner+Bot
// container. Unlike the AI-team aggregate group, this parent and its threads
// may only be mutated through the AI Team session API.
func IsDedicatedSessionPurpose(purpose string) bool {
	return purpose == GroupPurpose
}

// MaybeSetDefaultSessionTitle replaces the generated title with the first
// non-empty message sent by the owning user. The ai_team_session join prevents
// an ordinary thread named "新对话" from being renamed by this AI-only rule.
// Manual titles and subsequent messages are intentionally left untouched.
func MaybeSetDefaultSessionTitle(session *dbr.Session, groupNo, shortID, senderUID, content string) error {
	if session == nil || groupNo == "" || shortID == "" || senderUID == "" {
		return nil
	}
	title := strings.TrimSpace(content)
	if title == "" {
		return nil
	}
	if utf8.RuneCountInString(title) > 100 {
		title = string([]rune(title)[:100])
	}
	_, err := session.UpdateBySql(`
		UPDATE thread t
		JOIN ai_team_session s ON s.short_id=t.short_id AND s.manual_title=0
		JOIN ai_team_agent a ON a.id=s.agent_id AND a.user_uid=?
		SET t.name=?
		WHERE t.group_no=? AND t.short_id=? AND t.name=?`,
		senderUID, title, groupNo, shortID, DefaultSessionName).Exec()
	return err
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
// persisted authority. is_added is load-bearing: removal preserves the
// historical container/session rows but immediately stops active Bot routing;
// re-adding the same relation sets it back to 1 and reuses that history.
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
		JOIN robot r ON r.robot_id=a.bot_id COLLATE utf8mb4_0900_ai_ci AND r.status=1
			AND `+botpolicy.TeamEligibilitySQL("r", "a.user_uid COLLATE utf8mb4_0900_ai_ci", "a.space_id COLLATE utf8mb4_0900_ai_ci")+`
		JOIN user human_u ON human_u.uid=a.user_uid COLLATE utf8mb4_0900_ai_ci AND human_u.status=1 AND human_u.is_destroy<>2
		JOIN user bot_u ON bot_u.uid=a.bot_id COLLATE utf8mb4_0900_ai_ci AND bot_u.status=1 AND bot_u.is_destroy<>2
		JOIN space sp ON sp.space_id=a.space_id COLLATE utf8mb4_0900_ai_ci AND sp.status=1
		JOIN space_member human_sm ON human_sm.space_id=a.space_id COLLATE utf8mb4_0900_ai_ci AND human_sm.uid=a.user_uid COLLATE utf8mb4_0900_ai_ci AND human_sm.status=1
		JOIN space_member bot_sm ON bot_sm.space_id=a.space_id COLLATE utf8mb4_0900_ai_ci AND bot_sm.uid=a.bot_id COLLATE utf8mb4_0900_ai_ci AND bot_sm.status=1
		WHERE a.group_no=? AND s.short_id=? AND a.user_uid=? AND a.is_added=1
		LIMIT 1`, GroupPurpose, parts[0], parts[1], senderUID).Load(&target)
	return target, err
}

func RuntimeSessionKey(target *SessionTarget, channelID string) string {
	if target == nil {
		return ""
	}
	return target.SpaceID + ":" + target.BotID + ":" + channelID
}

// ExcludeProtectedItems filters every AI-team parent and its thread channel
// from ordinary conversation surfaces.
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
		Where("group_no IN ? AND purpose IN ?", groupNos, []string{GroupPurpose, TeamGroupPurpose, CustomTeamPurpose}).Load(&rows)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		protected[row.GroupNo] = struct{}{}
	}
	return protected, nil
}
