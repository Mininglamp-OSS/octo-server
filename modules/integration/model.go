package integration

const (
	defaultClientID   = "octopush"
	defaultClientName = "Octopush"
)

type oidcPrincipal struct {
	UID     string
	Subject string
	Issuer  string
}

type spaceResp struct {
	SpaceID         string `json:"space_id"`
	Name            string `json:"name"`
	Logo            string `json:"logo"`
	Role            int    `json:"role"`
	MemberCount     int    `json:"member_count"`
	IsDefault       bool   `json:"is_default"`
	HasAvailableBot bool   `json:"has_available_bot"`
}

type spacesResp struct {
	UID      string      `json:"uid"`
	ClientID string      `json:"client_id"`
	Spaces   []spaceResp `json:"spaces"`
}

type exchangeReq struct {
	SpaceID     string `json:"space_id"`
	IncludeBots bool   `json:"include_bots"`
}

type exchangeBotResp struct {
	RobotID     string `json:"robot_id"`
	Username    string `json:"username"`
	Name        string `json:"name"`
	Description string `json:"description"`
	CreatedAt   string `json:"created_at"`
}

type exchangeResp struct {
	UID       string            `json:"uid"`
	SpaceID   string            `json:"space_id"`
	SpaceName string            `json:"space_name"`
	ClientID  string            `json:"client_id"`
	APIKey    string            `json:"api_key"`
	Bots      []exchangeBotResp `json:"bots,omitempty"`
}

// createGroupReq —— 用 uk_ key 建团队群的请求体。owner / space / client 全部来自服务端
// 解析的 key 上下文，body 仅这两个字段且不携带任何信任边界。
type createGroupReq struct {
	Name           string   `json:"name"`
	MemberRobotIDs []string `json:"member_robot_ids"`
}

// createGroupResp —— 建群响应（时间 RFC3339，对齐 exchange 风格）。
type createGroupResp struct {
	GroupID        string   `json:"group_id"`
	SpaceID        string   `json:"space_id"`
	OwnerUserID    string   `json:"owner_user_id"`
	MemberRobotIDs []string `json:"member_robot_ids"`
	Name           string   `json:"name"`
	CreatedAt      string   `json:"created_at"`
}

// groupExistsResp —— 用户态存在性检测响应（恒 200，不存在时 exists=false 而非 404）。
type groupExistsResp struct {
	GroupID string `json:"group_id"`
	Exists  bool   `json:"exists"`
}

type managerIntegrationClientReq struct {
	Name   string `json:"name"`
	Status *int   `json:"status"`
}

type managerIntegrationClientResp struct {
	ClientID string `json:"client_id"`
	Name     string `json:"name"`
	Status   int    `json:"status"`
	Enabled  bool   `json:"enabled"`
}
