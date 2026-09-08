package workspace

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/space"
	appwkhttp "github.com/Mininglamp-OSS/octo-server/pkg/wkhttp"
)

const (
	workspaceNameMaxRunes        = 30
	workspaceDescriptionMaxRunes = 500
	workspaceKeywordMaxRunes     = 30
	workspaceMemberBatchMax      = 100
)

// API is the Workspace HTTP adapter.
type API struct {
	ctx     *config.Context
	service *Service
	log.Log
}

// New constructs the Workspace API and its service.
func New(ctx *config.Context) *API {
	return &API{
		ctx:     ctx,
		service: NewService(ctx),
		Log:     log.NewTLog("Workspace"),
	}
}

// Route mounts all Workspace user-facing endpoints. Every route is ordered as
// AuthMiddleware -> SharedUIDRateLimiter -> verified Space adapter. Resource
// routes derive the Space from workspace_id; collection routes require an
// explicit query/body space_id. X-Space-Id is only a consistency assertion.
func (a *API) Route(r *wkhttp.WKHttp) {

	collection := r.Group("/v1/workspaces",
		a.ctx.AuthMiddleware(r),
		appwkhttp.SharedUIDRateLimiter(r, a.ctx),
	)
	collection.GET("", VerifiedSpaceMiddleware(a.ctx, querySpaceResolver), a.listWorkspaces)
	collection.POST("", VerifiedSpaceMiddleware(a.ctx, bodySpaceResolver), a.createWorkspace)
	resource := r.Group("/v1/workspaces",
		a.ctx.AuthMiddleware(r),
		appwkhttp.SharedUIDRateLimiter(r, a.ctx),
		VerifiedSpaceMiddleware(a.ctx, a.workspaceSpaceResolver),
	)
	resource.GET("/:workspace_id", a.getWorkspace)
	resource.PUT("/:workspace_id", a.updateWorkspace)
	resource.DELETE("/:workspace_id", a.archiveWorkspace)
	resource.GET("/:workspace_id/members", a.listMembers)
	// Gin's parameter route would otherwise capture the literal "me". The
	// self-leave route must be registered first for the DELETE method.
	resource.DELETE("/:workspace_id/members/me", a.leaveWorkspace)
	resource.GET("/:workspace_id/members/:uid", a.getMember)
	resource.POST("/:workspace_id/members", a.addMembers)
	resource.PUT("/:workspace_id/members/:uid", a.updateMember)
	resource.DELETE("/:workspace_id/members/:uid", a.removeMember)
	resource.PUT("/:workspace_id/owner", a.transferOwner)
}

func querySpaceResolver(c *wkhttp.Context) (string, error) {
	return strings.TrimSpace(c.Query("space_id")), nil
}

func bodySpaceResolver(c *wkhttp.Context) (string, error) {
	body, err := readJSONBody(c)
	if err != nil {
		return "", ErrRequestInvalid
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) || trimmed[0] != '{' {
		return "", ErrRequestInvalid
	}
	var probe struct {
		SpaceID *string `json:"space_id"`
	}
	if err := json.Unmarshal(trimmed, &probe); err != nil {
		return "", ErrRequestInvalid
	}
	if probe.SpaceID == nil {
		return "", ErrSpaceRequired
	}
	return strings.TrimSpace(*probe.SpaceID), nil
}

func (a *API) workspaceSpaceResolver(c *wkhttp.Context) (string, error) {
	workspaceID := strings.TrimSpace(c.Param("workspace_id"))
	if workspaceID == "" {
		return "", ErrRequestInvalid
	}
	spaceID, err := a.service.ResolveSpace(workspaceID)
	if err != nil {
		return "", err
	}
	spaceID = strings.TrimSpace(spaceID)
	if spaceID == "" {
		return "", ErrNotFound
	}
	return spaceID, nil
}

func (a *API) listWorkspaces(c *wkhttp.Context) {
	spaceID := verifiedSpaceID(c)
	if spaceID == "" {
		RespondError(c, ErrSpaceRequired)
		return
	}
	keyword := strings.TrimSpace(c.Query("keyword"))
	if utf8.RuneCountInString(keyword) > workspaceKeywordMaxRunes {
		respondWorkspaceInvalid(c, "keyword")
		return
	}
	result, err := a.service.List(RequestScope(c), spaceID, keyword, ParsePage(c))
	if err != nil {
		RespondError(c, err)
		return
	}
	c.Response(result)
}

func (a *API) getWorkspace(c *wkhttp.Context) {
	workspaceID, ok := requiredPath(c, "workspace_id")
	if !ok {
		return
	}
	result, err := a.service.Get(RequestScope(c), workspaceID)
	if err != nil {
		RespondError(c, err)
		return
	}
	c.Response(result)
}

func (a *API) createWorkspace(c *wkhttp.Context) {
	var req CreateRequest
	raw, err := decodeJSON(c, &req)
	if err != nil {
		RespondError(c, ErrRequestInvalid)
		return
	}
	if isNullField(raw, "space_id") {
		RespondError(c, ErrSpaceRequired)
		return
	}
	if isNullField(raw, "name") || isNullField(raw, "description") || isNullField(raw, "logo") {
		RespondError(c, ErrRequestInvalid)
		return
	}
	req.SpaceID = strings.TrimSpace(req.SpaceID)
	req.Name = strings.TrimSpace(req.Name)
	if req.SpaceID == "" {
		RespondError(c, ErrSpaceRequired)
		return
	}
	if req.Name == "" || utf8.RuneCountInString(req.Name) > workspaceNameMaxRunes {
		respondWorkspaceInvalid(c, "name")
		return
	}
	if utf8.RuneCountInString(req.Description) > workspaceDescriptionMaxRunes {
		respondWorkspaceInvalid(c, "description")
		return
	}

	result, err := a.service.Create(RequestScope(c), req)
	if err != nil {
		RespondError(c, err)
		return
	}
	c.ResponseWithStatus(http.StatusCreated, result)
}

func (a *API) updateWorkspace(c *wkhttp.Context) {
	workspaceID, ok := requiredPath(c, "workspace_id")
	if !ok {
		return
	}
	var req UpdateRequest
	raw, err := decodeJSON(c, &req)
	if err != nil {
		RespondError(c, ErrRequestInvalid)
		return
	}
	for _, field := range []string{"name", "description", "logo"} {
		if isNullField(raw, field) {
			respondWorkspaceInvalid(c, field)
			return
		}
	}
	if req.Name == nil && req.Description == nil && req.Logo == nil {
		RespondError(c, ErrRequestInvalid)
		return
	}
	if req.Name != nil {
		trimmed := strings.TrimSpace(*req.Name)
		if trimmed == "" || utf8.RuneCountInString(trimmed) > workspaceNameMaxRunes {
			respondWorkspaceInvalid(c, "name")
			return
		}
		req.Name = &trimmed
	}
	if req.Description != nil && utf8.RuneCountInString(*req.Description) > workspaceDescriptionMaxRunes {
		respondWorkspaceInvalid(c, "description")
		return
	}

	result, err := a.service.Update(RequestScope(c), workspaceID, req)
	if err != nil {
		RespondError(c, err)
		return
	}
	c.Response(result)
}

func (a *API) archiveWorkspace(c *wkhttp.Context) {
	workspaceID, ok := requiredPath(c, "workspace_id")
	if !ok {
		return
	}
	if err := a.service.Archive(RequestScope(c), workspaceID); err != nil {
		RespondError(c, err)
		return
	}
	c.ResponseOK()
}

func (a *API) listMembers(c *wkhttp.Context) {
	workspaceID, ok := requiredPath(c, "workspace_id")
	if !ok {
		return
	}
	filter, err := parseMemberFilter(c)
	if err != nil {
		RespondError(c, err)
		return
	}
	result, err := a.service.Members(RequestScope(c), workspaceID, filter, ParsePage(c))
	if err != nil {
		RespondError(c, err)
		return
	}
	c.Response(result)
}

func (a *API) getMember(c *wkhttp.Context) {
	workspaceID, ok := requiredPath(c, "workspace_id")
	if !ok {
		return
	}
	uid, ok := requiredPath(c, "uid")
	if !ok {
		return
	}
	result, err := a.service.Member(RequestScope(c), workspaceID, uid)
	if err != nil {
		RespondError(c, err)
		return
	}
	c.Response(result)
}

type addMembersRequest struct {
	Members []MemberInput `json:"members"`
}

type addMembersResponse struct {
	WorkspaceID string   `json:"workspace_id"`
	Members     []Member `json:"members"`
}

func (a *API) addMembers(c *wkhttp.Context) {
	workspaceID, ok := requiredPath(c, "workspace_id")
	if !ok {
		return
	}
	var req addMembersRequest
	raw, err := decodeJSON(c, &req)
	if err != nil {
		RespondError(c, ErrRequestInvalid)
		return
	}
	if isNullField(raw, "members") || len(req.Members) < 1 {
		RespondError(c, ErrRequestInvalid)
		return
	}
	if len(req.Members) > workspaceMemberBatchMax {
		RespondError(c, ErrCandidateIneligible)
		return
	}
	for i := range req.Members {
		req.Members[i].UID = strings.TrimSpace(req.Members[i].UID)
		req.Members[i].WorkspaceRole = strings.TrimSpace(req.Members[i].WorkspaceRole)
		if req.Members[i].UID == "" {
			RespondError(c, ErrRequestInvalid)
			return
		}
		if req.Members[i].WorkspaceRole != WorkspaceRoleAdmin && req.Members[i].WorkspaceRole != WorkspaceRoleMember {
			RespondError(c, ErrRoleInvalid)
			return
		}
	}

	members, err := a.service.AddMembers(RequestScope(c), workspaceID, req.Members)
	if err != nil {
		RespondError(c, err)
		return
	}
	c.Response(addMembersResponse{WorkspaceID: workspaceID, Members: members})
}

type memberRoleRequest struct {
	WorkspaceRole string `json:"workspace_role"`
}

func (a *API) updateMember(c *wkhttp.Context) {
	workspaceID, ok := requiredPath(c, "workspace_id")
	if !ok {
		return
	}
	uid, ok := requiredPath(c, "uid")
	if !ok {
		return
	}
	var req memberRoleRequest
	raw, err := decodeJSON(c, &req)
	if err != nil {
		RespondError(c, ErrRequestInvalid)
		return
	}
	if isNullField(raw, "workspace_role") {
		RespondError(c, ErrRoleInvalid)
		return
	}
	req.WorkspaceRole = strings.TrimSpace(req.WorkspaceRole)
	if req.WorkspaceRole != WorkspaceRoleAdmin && req.WorkspaceRole != WorkspaceRoleMember {
		RespondError(c, ErrRoleInvalid)
		return
	}
	result, err := a.service.UpdateMember(RequestScope(c), workspaceID, uid, req.WorkspaceRole)
	if err != nil {
		RespondError(c, err)
		return
	}
	c.Response(result)
}

func (a *API) removeMember(c *wkhttp.Context) {
	workspaceID, ok := requiredPath(c, "workspace_id")
	if !ok {
		return
	}
	uid, ok := requiredPath(c, "uid")
	if !ok {
		return
	}
	if err := a.service.RemoveMember(RequestScope(c), workspaceID, uid); err != nil {
		RespondError(c, err)
		return
	}
	c.ResponseOK()
}

type transferOwnerRequest struct {
	UID string `json:"uid"`
}

func (a *API) transferOwner(c *wkhttp.Context) {
	workspaceID, ok := requiredPath(c, "workspace_id")
	if !ok {
		return
	}
	var req transferOwnerRequest
	raw, err := decodeJSON(c, &req)
	if err != nil {
		RespondError(c, ErrRequestInvalid)
		return
	}
	uidRaw, present := raw["uid"]
	if !present || bytes.Equal(bytes.TrimSpace(uidRaw), []byte("null")) {
		RespondError(c, ErrRequestInvalid)
		return
	}
	req.UID = strings.TrimSpace(req.UID)
	if req.UID == "" {
		RespondError(c, ErrCandidateIneligible)
		return
	}
	result, err := a.service.TransferOwner(RequestScope(c), workspaceID, req.UID)
	if err != nil {
		RespondError(c, err)
		return
	}
	c.Response(result)
}

func (a *API) leaveWorkspace(c *wkhttp.Context) {
	workspaceID, ok := requiredPath(c, "workspace_id")
	if !ok {
		return
	}
	if err := a.service.Leave(RequestScope(c), workspaceID); err != nil {
		RespondError(c, err)
		return
	}
	c.ResponseOK()
}

func parseMemberFilter(c *wkhttp.Context) (MemberFilter, error) {
	filter := MemberFilter{Status: MemberStatusActive}
	rawRole, rolePresent := c.GetQuery("workspace_role")
	if rolePresent {
		rawRole = strings.TrimSpace(rawRole)
		if rawRole == "" {
			return MemberFilter{}, ErrRequestInvalid
		}
		seen := make(map[string]struct{}, 3)
		for _, part := range strings.Split(rawRole, ",") {
			role := strings.TrimSpace(part)
			if role != WorkspaceRoleOwner && role != WorkspaceRoleAdmin && role != WorkspaceRoleMember {
				return MemberFilter{}, ErrRequestInvalid
			}
			if _, ok := seen[role]; ok {
				continue
			}
			seen[role] = struct{}{}
			filter.Roles = append(filter.Roles, role)
		}
	}
	rawStatus, statusPresent := c.GetQuery("status")
	if statusPresent {
		rawStatus = strings.TrimSpace(rawStatus)
		if rawStatus == "" {
			return MemberFilter{}, ErrRequestInvalid
		}
		value, err := strconv.Atoi(rawStatus)
		if err != nil || (value != MemberStatusActive && value != MemberStatusInactive) {
			return MemberFilter{}, ErrRequestInvalid
		}
		filter.Status = value
	}
	return filter, nil
}

func requiredPath(c *wkhttp.Context, name string) (string, bool) {
	value := strings.TrimSpace(c.Param(name))
	if value == "" {
		respondWorkspaceInvalid(c, name)
		return "", false
	}
	return value, true
}

func verifiedSpaceID(c *wkhttp.Context) string {
	return strings.TrimSpace(space.GetSpaceID(c))
}

func isJSONContentType(c *wkhttp.Context) bool {
	if c == nil {
		return false
	}
	value := strings.TrimSpace(c.GetHeader("Content-Type"))
	if value == "" {
		return false
	}
	mediaType, _, err := mime.ParseMediaType(value)
	return err == nil && strings.EqualFold(mediaType, "application/json")
}

func readJSONBody(c *wkhttp.Context) ([]byte, error) {
	if c == nil || c.Request == nil || !isJSONContentType(c) {
		return nil, errors.New("workspace: JSON content type required")
	}
	if c.Request.Body == nil {
		return nil, io.EOF
	}
	body, err := io.ReadAll(c.Request.Body)
	c.Request.Body = io.NopCloser(bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	return body, nil
}

func decodeJSON(c *wkhttp.Context, dst interface{}) (map[string]json.RawMessage, error) {
	body, err := readJSONBody(c)
	if err != nil {
		return nil, err
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) || trimmed[0] != '{' {
		return nil, errors.New("workspace: JSON object required")
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &raw); err != nil || raw == nil {
		return nil, errors.New("workspace: invalid JSON object")
	}
	if err := json.Unmarshal(trimmed, dst); err != nil {
		return nil, err
	}
	return raw, nil
}

func isNullField(raw map[string]json.RawMessage, field string) bool {
	value, ok := raw[field]
	return ok && bytes.Equal(bytes.TrimSpace(value), []byte("null"))
}
