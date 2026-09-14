package message

import (
	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-server/modules/group"
	"github.com/Mininglamp-OSS/octo-server/modules/thread"
	"github.com/Mininglamp-OSS/octo-server/pkg/aiteam"
)

// AITeamConversationSync is a separate result from the existing IM request.
// Clients own unread state: full replaces the Space's snapshot, delta merges
// absolute per-channel values. A delta's sum is not an agent's total unread.
type AITeamConversationSync struct {
	Mode  string                `json:"mode"`
	Items []*AITeamConversation `json:"items"`
}

type AITeamConversation struct {
	BotID       string `json:"bot_id"`
	SessionID   string `json:"session_id"`
	SpaceID     string `json:"space_id"`
	ChannelID   string `json:"channel_id"`
	ChannelType uint8  `json:"channel_type"`
	Unread      int64  `json:"unread"`
	Version     int64  `json:"version"`
	LastMsgSeq  int64  `json:"last_msg_seq"`
	Timestamp   int64  `json:"timestamp"`
}

// aiTeamConversations does not change ordinary conversations, cursors or read
// state. It only reads the already-fetched IM result and indexed associations
// for its AI channels. No Redis, additional IM request or all-session scan.
func (co *Conversation) aiTeamConversations(spaceID, uid string, full bool, conversations []*config.SyncUserConversationResp, groups map[string]*group.GroupResp) (*AITeamConversationSync, error) {
	out := &AITeamConversationSync{Mode: "delta", Items: make([]*AITeamConversation, 0)}
	if full {
		out.Mode = "full"
	}
	shortIDs := make([]string, 0)
	seen := make(map[string]bool)
	for _, conversation := range conversations {
		if conversation == nil || conversation.ChannelType != common.ChannelTypeCommunityTopic.Uint8() {
			continue
		}
		parent, shortID, err := thread.ParseChannelID(conversation.ChannelID)
		if err != nil {
			continue
		}
		info := groups[parent]
		if info == nil || info.Purpose != aiteam.GroupPurpose {
			continue
		}
		if !seen[shortID] {
			seen[shortID] = true
			shortIDs = append(shortIDs, shortID)
		}
	}
	if len(shortIDs) == 0 {
		return out, nil
	}
	var targets []aiteam.SessionTarget
	_, err := co.ctx.DB().SelectBySql(`
  SELECT a.id AS agent_id, a.bot_id, a.group_no, s.short_id
  FROM ai_team_session s
  JOIN ai_team_agent a ON a.id=s.agent_id
  JOIN thread t ON t.short_id=s.short_id AND t.group_no=a.group_no AND t.status<>3
  JOIN `+"`group`"+` g ON g.group_no=a.group_no COLLATE utf8mb4_0900_ai_ci AND g.purpose=? AND g.status=1
  JOIN group_member gm ON gm.group_no=g.group_no COLLATE utf8mb4_0900_ai_ci AND gm.uid=a.user_uid COLLATE utf8mb4_0900_ai_ci AND gm.status=1 AND gm.is_deleted=0
  JOIN robot r ON r.robot_id=a.bot_id COLLATE utf8mb4_0900_ai_ci AND r.status=1 AND r.creator_uid=a.user_uid COLLATE utf8mb4_0900_ai_ci
  JOIN user human_u ON human_u.uid=a.user_uid COLLATE utf8mb4_0900_ai_ci AND human_u.status=1 AND human_u.is_destroy<>2
  JOIN user bot_u ON bot_u.uid=a.bot_id COLLATE utf8mb4_0900_ai_ci AND bot_u.status=1 AND bot_u.is_destroy<>2
  JOIN space sp ON sp.space_id=a.space_id COLLATE utf8mb4_0900_ai_ci AND sp.status=1
  JOIN space_member human_sm ON human_sm.space_id=a.space_id COLLATE utf8mb4_0900_ai_ci AND human_sm.uid=a.user_uid COLLATE utf8mb4_0900_ai_ci AND human_sm.status=1
  JOIN space_member bot_sm ON bot_sm.space_id=a.space_id COLLATE utf8mb4_0900_ai_ci AND bot_sm.uid=a.bot_id COLLATE utf8mb4_0900_ai_ci AND bot_sm.status=1
  WHERE a.space_id=? AND a.user_uid=? AND s.state=2 AND s.short_id IN ?`, aiteam.GroupPurpose, spaceID, uid, shortIDs).Load(&targets)
	if err != nil {
		return nil, err
	}
	targetByChannel := make(map[string]aiteam.SessionTarget, len(targets))
	for _, target := range targets {
		targetByChannel[thread.BuildChannelID(target.GroupNo, target.ShortID)] = target
	}
	// Retain IM response order and values, but never let a wrong channel type or
	// the same short ID under a different parent cross the authority boundary.
	emitted := make(map[string]bool, len(targets))
	for _, conversation := range conversations {
		if conversation == nil || conversation.ChannelType != common.ChannelTypeCommunityTopic.Uint8() {
			continue
		}
		target, ok := targetByChannel[conversation.ChannelID]
		if !ok || emitted[conversation.ChannelID] {
			continue
		}
		emitted[conversation.ChannelID] = true
		out.Items = append(out.Items, &AITeamConversation{
			BotID: target.BotID, SessionID: target.ShortID, SpaceID: spaceID,
			ChannelID: conversation.ChannelID, ChannelType: conversation.ChannelType,
			Unread: max(0, int64(conversation.Unread)), Version: conversation.Version,
			LastMsgSeq: conversation.LastMsgSeq, Timestamp: conversation.Timestamp,
		})
	}
	return out, nil
}
