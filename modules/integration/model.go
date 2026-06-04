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
