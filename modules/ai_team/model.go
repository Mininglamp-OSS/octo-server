package ai_team

import "time"

const (
	containerUnassigned   = 0
	containerProvisioning = 1
	containerReady        = 2
	containerFailed       = 3

	sessionProvisioning = 1
	sessionReady        = 2
	sessionFailed       = 3

	maxPageSize       = 100
	defaultPageSize   = 20
	maxIdempotencyKey = 128
)

type AgentGroupType string

const (
	AgentGroupTypeCloudClone        AgentGroupType = "cloud_clone"
	AgentGroupTypePersonalAssistant AgentGroupType = "personal_assistant"
	AgentGroupTypeDigitalEmployee   AgentGroupType = "digital_employee"

	agentHostingOctoHosted = "octo_hosted"
	agentGroupSQL          = "CASE WHEN r.agent_hosting='" + agentHostingOctoHosted + "' THEN '" + string(AgentGroupTypeCloudClone) + "' ELSE '" + string(AgentGroupTypePersonalAssistant) + "' END"
)

type Agent struct {
	ID             int64          `db:"id" json:"-"`
	SpaceID        string         `db:"space_id" json:"space_id"`
	UserUID        string         `db:"user_uid" json:"user_uid"`
	BotID          string         `db:"bot_id" json:"bot_id"`
	BotName        string         `db:"bot_name" json:"bot_name"`
	GroupNo        string         `db:"group_no" json:"group_no,omitempty"`
	IsAdded        int            `db:"is_added" json:"is_added"`
	ContainerState int            `db:"container_state" json:"container_state"`
	SessionCount   int64          `db:"session_count" json:"session_count"`
	CreatedAt      time.Time      `db:"created_at" json:"created_at"`
	UpdatedAt      time.Time      `db:"updated_at" json:"updated_at"`
	GroupType      AgentGroupType `db:"agent_group" json:"-"`
}

type Session struct {
	ID                 int64      `db:"id" json:"-"`
	AgentID            int64      `db:"agent_id" json:"-"`
	ShortID            string     `db:"short_id" json:"session_id"`
	GroupNo            string     `db:"group_no" json:"group_no"`
	ChannelID          string     `db:"-" json:"channel_id"`
	Name               string     `db:"name" json:"name"`
	Status             int        `db:"status" json:"status"`
	MessageCount       int64      `db:"message_count" json:"message_count"`
	LastMessageContent string     `db:"last_message_content" json:"last_message_content,omitempty"`
	LastMessageAt      *time.Time `db:"last_message_at" json:"last_message_at,omitempty"`
	CreatedAt          time.Time  `db:"created_at" json:"created_at"`
	UpdatedAt          time.Time  `db:"updated_at" json:"updated_at"`
	State              int        `db:"state" json:"state"`
	ManualTitle        int        `db:"manual_title" json:"manual_title"`
	Mute               int        `db:"mute" json:"mute"`
	IsPinned           bool       `db:"is_pinned" json:"is_pinned"`
	RequestHash        string     `db:"request_hash" json:"-"`
}

type SessionPage struct {
	Items     []*Session `json:"items"`
	PageIndex int        `json:"page_index"`
	PageSize  int        `json:"page_size"`
	HasMore   bool       `json:"has_more"`
}

type AgentGroup struct {
	Type  AgentGroupType `json:"type"`
	Count int64          `json:"count"`
	Items []*Agent       `json:"items"`
}

type AgentPage struct {
	Groups     []*AgentGroup `json:"groups"`
	NextCursor string        `json:"next_cursor,omitempty"`
}
