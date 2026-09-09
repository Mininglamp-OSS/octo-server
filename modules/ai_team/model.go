package ai_team

import "time"

const (
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
	agentGroupSQL          = "CASE WHEN r.kind='avatar' THEN '" + string(AgentGroupTypeDigitalEmployee) + "' WHEN r.agent_hosting='" + agentHostingOctoHosted + "' THEN '" + string(AgentGroupTypeCloudClone) + "' ELSE '" + string(AgentGroupTypePersonalAssistant) + "' END"
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

type TeamGroup struct {
	GroupNo string `db:"group_no" json:"group_no"`
	Name    string `db:"name" json:"name"`
	State   int    `db:"state" json:"state"`
}

type TeamType string

const (
	TeamTypeAllAgents TeamType = "all_agents"
	TeamTypeCustom    TeamType = "custom"
)

type AgentPage struct {
	Groups     []*AgentGroup `json:"groups"`
	NextCursor string        `json:"next_cursor,omitempty"`
}

type Team struct {
	GroupNo         string    `db:"group_no" json:"group_no"`
	SpaceID         string    `db:"space_id" json:"space_id"`
	UserUID         string    `db:"user_uid" json:"user_uid"`
	Name            string    `db:"name" json:"name"`
	Type            TeamType  `db:"team_type" json:"type"`
	Editable        bool      `db:"-" json:"editable"`
	MembersEditable bool      `db:"-" json:"members_editable"`
	State           int       `db:"state" json:"state"`
	MemberCount     int64     `db:"member_count" json:"member_count"`
	AvatarText      string    `db:"avatar_text" json:"avatar_text"`
	AvatarColor     *int      `db:"avatar_color" json:"avatar_color"`
	IsUploadAvatar  int       `db:"is_upload_avatar" json:"is_upload_avatar"`
	CreatedAt       time.Time `db:"created_at" json:"created_at"`
	UpdatedAt       time.Time `db:"updated_at" json:"updated_at"`
	Agents          []*Agent  `db:"-" json:"agents,omitempty"`
	SortOrder       int       `db:"sort_order" json:"-"`
}

type TeamPage struct {
	Items      []*Team `json:"items"`
	NextCursor string  `json:"next_cursor,omitempty"`
}

type CreateTeamRequest struct {
	Name        string   `json:"name"`
	BotIDs      []string `json:"bot_ids"`
	AvatarText  string   `json:"avatar_text"`
	AvatarColor *int     `json:"avatar_color"`
}

type UpdateTeamRequest struct {
	Name        *string `json:"name"`
	AvatarText  *string `json:"avatar_text"`
	AvatarColor *int    `json:"avatar_color"`
}
