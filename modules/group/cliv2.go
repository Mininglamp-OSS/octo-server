package group

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/register"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	chservice "github.com/Mininglamp-OSS/octo-server/modules/channel/service"
	aiteampkg "github.com/Mininglamp-OSS/octo-server/pkg/aiteam"
	"github.com/Mininglamp-OSS/octo-server/pkg/cliv2"
	spacepkg "github.com/Mininglamp-OSS/octo-server/pkg/space"
	"go.uber.org/zap"
)

var (
	ErrCLISpaceAccessDenied = errors.New("CLI principal is not an active member of the bound Space")
	ErrCLIGroupNotFound     = errors.New("CLI group not found in the bound Space")
	ErrCLIGroupAccessDenied = errors.New("CLI actor is not an active member of the group")

	// errCLIBotOutsideInviterSpace marks an invited Bot that is not an active
	// member of the human inviter's effective Space (v1 Bot/Space isolation).
	errCLIBotOutsideInviterSpace = errors.New("CLI invited Bot is not in the inviter's effective Space")
)

// CheckCLISpaceAccess validates both the token owner and effective actor against
// the Space currently bound to the credential. This is intentionally a live DB
// check; a previously valid Auth token must not outlive revoked Space access.
func CheckCLISpaceAccess(ctx *config.Context, spaceID, userID, actorID string) error {
	if ctx == nil || strings.TrimSpace(spaceID) == "" || strings.TrimSpace(userID) == "" || strings.TrimSpace(actorID) == "" {
		return ErrCLISpaceAccessDenied
	}
	ownerOK, err := spacepkg.CheckMembership(ctx.DB(), spaceID, userID)
	if err != nil {
		return err
	}
	if !ownerOK {
		return ErrCLISpaceAccessDenied
	}
	if actorID == userID {
		return nil
	}
	actorOK, err := spacepkg.CheckMembership(ctx.DB(), spaceID, actorID)
	if err != nil {
		return err
	}
	if !actorOK {
		return ErrCLISpaceAccessDenied
	}
	return nil
}

// CheckCLIGroupAccess enforces the bound-Space and active native-group membership
// gates shared by group and thread v2 routes.
func CheckCLIGroupAccess(ctx *config.Context, db *DB, groupNo, spaceID, userID, actorID string) (*Model, error) {
	if err := CheckCLISpaceAccess(ctx, spaceID, userID, actorID); err != nil {
		return nil, err
	}
	if db == nil {
		return nil, ErrCLIGroupNotFound
	}
	groupModel, err := db.QueryWithGroupNo(groupNo)
	if err != nil {
		return nil, err
	}
	if groupModel == nil || groupModel.SpaceID != spaceID || groupModel.Status == GroupStatusDisband {
		return nil, ErrCLIGroupNotFound
	}
	isMember, err := db.ExistMemberActive(actorID, groupNo)
	if err != nil {
		return nil, err
	}
	if !isMember {
		return nil, ErrCLIGroupAccessDenied
	}
	return groupModel, nil
}

func (g *Group) registerCLIGroupRoutes(r *wkhttp.WKHttp) {
	groupGuard := []wkhttp.HandlerFunc{g.cliGroupAccess}
	mutationGuard := []wkhttp.HandlerFunc{g.cliProtectAIContainerMutation, g.cliGroupAccess}
	for _, route := range []cliv2.Route{
		{Method: http.MethodGet, Path: "/v2/cli/groups", Action: cliv2.ActionGroupsRead, User: g.cliListGroups, Bot: g.cliListGroups},
		{Method: http.MethodPost, Path: "/v2/cli/groups", Action: cliv2.ActionGroupsWrite, User: g.cliCreateGroup, Bot: g.cliCreateGroup},
		{Method: http.MethodGet, Path: "/v2/cli/groups/:group_id", Action: cliv2.ActionGroupsRead, User: g.cliGetGroup, Bot: g.cliGetGroup, Middlewares: groupGuard},
		{Method: http.MethodGet, Path: "/v2/cli/groups/:group_id/members", Action: cliv2.ActionGroupsRead, User: g.cliGetGroupMembers, Bot: g.cliGetGroupMembers, Middlewares: groupGuard},
		{Method: http.MethodGet, Path: "/v2/cli/groups/:group_id/md", Action: cliv2.ActionGroupsRead, User: g.cliGetGroupMD, Bot: g.cliGetGroupMD, Middlewares: groupGuard},
		{Method: http.MethodPut, Path: "/v2/cli/groups/:group_id/md", Action: cliv2.ActionGroupsWrite, User: g.cliUpdateGroupMD, Bot: g.cliUpdateGroupMD, Middlewares: mutationGuard},
		{Method: http.MethodPatch, Path: "/v2/cli/groups/:group_id/info", Action: cliv2.ActionGroupsWrite, User: g.cliUpdateGroupInfo, Bot: g.cliUpdateGroupInfo, Middlewares: mutationGuard},
		{Method: http.MethodPost, Path: "/v2/cli/groups/:group_id/members/_add", Action: cliv2.ActionGroupsWrite, User: g.cliAddGroupMembers, Bot: g.cliAddGroupMembers, Middlewares: mutationGuard},
		{Method: http.MethodPost, Path: "/v2/cli/groups/:group_id/members/_remove", Action: cliv2.ActionGroupsWrite, User: g.cliRemoveGroupMembers, Bot: g.cliRemoveGroupMembers, Middlewares: mutationGuard},
	} {
		cliv2.Add(r, route)
	}
}

func (g *Group) cliGroupAccess(c *wkhttp.Context) {
	principal := cliv2.PrincipalFrom(c)
	actorID := c.GetLoginUID()
	model, err := CheckCLIGroupAccess(g.ctx, g.db, c.Param("group_id"), principal.SpaceID, principal.UserID, actorID)
	if err != nil {
		g.cliAccessFailure(c, err)
		c.Abort()
		return
	}
	c.Set("cliv2.group", model)
	c.Next()
}

func (g *Group) cliProtectAIContainerMutation(c *wkhttp.Context) {
	groupNo := c.Param("group_id")
	protected, err := aiteampkg.IsProtectedGroup(g.ctx.DB(), groupNo)
	if err != nil {
		g.Error("CLI AI container protection query failed", zap.Error(err), zap.String("group_no", groupNo))
		cliv2.Fail(c, http.StatusInternalServerError, "INTERNAL_ERROR", "The request could not be completed.")
		c.Abort()
		return
	}
	if protected {
		cliv2.Fail(c, http.StatusForbidden, "FORBIDDEN", "Access denied.")
		c.Abort()
		return
	}
	c.Next()
}

func (g *Group) cliAccessFailure(c *wkhttp.Context, err error) {
	switch {
	case errors.Is(err, ErrCLIGroupNotFound):
		cliv2.Fail(c, http.StatusNotFound, "NOT_FOUND", "Group not found.")
	case errors.Is(err, ErrCLISpaceAccessDenied), errors.Is(err, ErrCLIGroupAccessDenied):
		cliv2.Fail(c, http.StatusForbidden, "FORBIDDEN", "Access denied.")
	default:
		g.Error("CLI group authorization query failed", zap.Error(err))
		cliv2.Fail(c, http.StatusInternalServerError, "INTERNAL_ERROR", "The request could not be completed.")
	}
}

func (g *Group) cliGetGroupModel(c *wkhttp.Context) *Model {
	model, _ := c.Get("cliv2.group")
	groupModel, _ := model.(*Model)
	return groupModel
}

func (g *Group) cliListGroups(c *wkhttp.Context) {
	principal := cliv2.PrincipalFrom(c)
	actorID := c.GetLoginUID()
	if err := CheckCLISpaceAccess(g.ctx, principal.SpaceID, principal.UserID, actorID); err != nil {
		g.cliAccessFailure(c, err)
		return
	}
	page, size, ok := cliPage(c.Query("page"), c.Query("page_size"))
	if !ok {
		cliv2.Fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "Invalid pagination parameters.")
		return
	}
	var total int64
	err := g.ctx.DB().SelectBySql(
		"SELECT COUNT(DISTINCT g.group_no) FROM `group` g INNER JOIN group_member gm ON gm.group_no=g.group_no WHERE gm.uid=? AND gm.is_deleted=0 AND gm.status=? AND g.space_id=? AND g.purpose='' AND g.status<>?",
		actorID, common.GroupMemberStatusNormal, principal.SpaceID, GroupStatusDisband,
	).LoadOne(&total)
	if err != nil {
		g.Error("CLI group count query failed", zap.Error(err))
		cliv2.Fail(c, http.StatusInternalServerError, "INTERNAL_ERROR", "The request could not be completed.")
		return
	}
	var groupNos []string
	_, err = g.ctx.DB().SelectBySql(
		"SELECT g.group_no FROM `group` g WHERE EXISTS (SELECT 1 FROM group_member gm WHERE gm.group_no=g.group_no AND gm.uid=? AND gm.is_deleted=0 AND gm.status=?) AND g.space_id=? AND g.purpose='' AND g.status<>? ORDER BY g.created_at DESC, g.group_no DESC LIMIT ? OFFSET ?",
		actorID, common.GroupMemberStatusNormal, principal.SpaceID, GroupStatusDisband, size, (page-1)*size,
	).Load(&groupNos)
	if err != nil {
		g.Error("CLI group list query failed", zap.Error(err))
		cliv2.Fail(c, http.StatusInternalServerError, "INTERNAL_ERROR", "The request could not be completed.")
		return
	}
	if len(groupNos) == 0 {
		cliRespondPage(c, []any{}, total, page, size)
		return
	}
	groups, err := g.groupService.GetGroupDetails(groupNos, actorID)
	if err != nil {
		g.Error("CLI group details query failed", zap.Error(err))
		cliv2.Fail(c, http.StatusInternalServerError, "INTERNAL_ERROR", "The request could not be completed.")
		return
	}
	roleMap, err := g.db.queryGroupMyRoles(actorID, groupNos)
	if err != nil {
		g.Error("CLI group role query failed", zap.Error(err))
		cliv2.Fail(c, http.StatusInternalServerError, "INTERNAL_ERROR", "The request could not be completed.")
		return
	}
	memberCounts, err := g.db.queryGroupMemberCounts(groupNos)
	if err != nil {
		g.Error("CLI group member count query failed", zap.Error(err))
		cliv2.Fail(c, http.StatusInternalServerError, "INTERNAL_ERROR", "The request could not be completed.")
		return
	}
	botAdmins, err := g.db.queryGroupMyBotAdmin(actorID, groupNos)
	if err != nil {
		g.Error("CLI group bot admin query failed", zap.Error(err))
		cliv2.Fail(c, http.StatusInternalServerError, "INTERNAL_ERROR", "The request could not be completed.")
		return
	}
	for _, groupResp := range groups {
		if groupResp == nil {
			continue
		}
		groupResp.Role = roleMap[groupResp.GroupNo]
		groupResp.MemberCount = int(memberCounts[groupResp.GroupNo])
		groupResp.CanManageBotAdmin = groupResp.Role == MemberRoleCreator || groupResp.Role == MemberRoleManager
		if principal.BotID != "" {
			groupResp.CanEditGroupMd = botAdmins[groupResp.GroupNo]
		} else {
			groupResp.CanEditGroupMd = groupResp.Role == MemberRoleCreator || groupResp.Role == MemberRoleManager
		}
	}
	cliRespondPage(c, groups, total, page, size)
}

func (g *Group) cliCreateGroup(c *wkhttp.Context) {
	principal := cliv2.PrincipalFrom(c)
	actorID := c.GetLoginUID()
	if err := CheckCLISpaceAccess(g.ctx, principal.SpaceID, principal.UserID, actorID); err != nil {
		g.cliAccessFailure(c, err)
		return
	}
	var req struct {
		Name    string   `json:"name"`
		Members []string `json:"members"`
		SpaceID string   `json:"space_id"`
	}
	if err := c.BindJSON(&req); err != nil || len(req.Members) == 0 || len(req.Members) > 200 || utf8.RuneCountInString(req.Name) > MaxGroupNameLen {
		cliv2.Fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "Invalid group request.")
		return
	}
	if req.SpaceID != "" && req.SpaceID != principal.SpaceID {
		cliv2.Fail(c, http.StatusForbidden, "FORBIDDEN", "Access denied.")
		return
	}
	members := req.Members
	var botUID string
	if principal.BotID != "" {
		isBot, err := g.groupService.IsRobot(actorID)
		if err != nil {
			g.Error("CLI group creator Bot lookup failed", zap.Error(err))
			cliv2.Fail(c, http.StatusInternalServerError, "INTERNAL_ERROR", "The request could not be completed.")
			return
		}
		if !isBot {
			cliv2.Fail(c, http.StatusForbidden, "FORBIDDEN", "Access denied.")
			return
		}
		botUID = actorID
		memberUsers, err := g.userDB.QueryByUIDs(req.Members)
		if err != nil {
			g.Error("CLI group member lookup failed", zap.Error(err))
			cliv2.Fail(c, http.StatusInternalServerError, "INTERNAL_ERROR", "The request could not be completed.")
			return
		}
		robotSet := make(map[string]struct{}, len(memberUsers))
		for _, member := range memberUsers {
			if member.Robot == 1 {
				robotSet[member.UID] = struct{}{}
			}
		}
		humanMembers := make([]string, 0, len(req.Members))
		for _, uid := range req.Members {
			if _, isBot := robotSet[uid]; !isBot {
				humanMembers = append(humanMembers, uid)
			}
		}
		if len(humanMembers) == 0 {
			cliv2.Fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "At least one human member is required.")
			return
		}
		members = humanMembers
	}
	if principal.BotID == "" {
		// User-subject creation keeps the legacy human gate: the same-day per
		// creator cap runs before any row is written, and the system-account
		// admission switch still governs the file-helper UID.
		count, err := g.db.querySameDayCreateCountWitUID(principal.UserID, util.Toyyyy_MM_dd(time.Now()))
		if err != nil {
			g.Error("CLI group daily create count query failed", zap.Error(err))
			cliv2.Fail(c, http.StatusInternalServerError, "INTERNAL_ERROR", "The request could not be completed.")
			return
		}
		if g.ctx.GetConfig().Group.SameDayCreateMaxCount <= count {
			cliv2.Fail(c, http.StatusTooManyRequests, "RATE_LIMITED", "Too many groups created today.")
			return
		}
		appConfig, err := g.commonService.GetAppConfig()
		if err != nil {
			g.Error("CLI group app config query failed", zap.Error(err))
			cliv2.Fail(c, http.StatusInternalServerError, "INTERNAL_ERROR", "The request could not be completed.")
			return
		}
		if appConfig != nil && appConfig.InviteSystemAccountJoinGroupOn == 0 {
			fileHelperUID := g.ctx.GetConfig().Account.FileHelperUID
			for _, uid := range members {
				if uid == fileHelperUID {
					cliv2.Fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "The file helper account cannot join this group.")
					return
				}
			}
		}
	}
	resp, err := g.groupService.CreateGroup(&CreateGroupServiceReq{
		Creator: principal.UserID,
		Members: members,
		Name:    req.Name,
		SpaceID: principal.SpaceID,
		BotUID:  botUID,
	})
	if err != nil {
		g.Error("CLI group creation failed", zap.Error(err))
		cliv2.Fail(c, http.StatusInternalServerError, "INTERNAL_ERROR", "The request could not be completed.")
		return
	}
	if principal.BotID == "" {
		// Legacy user-subject creation copies the creator's personal message
		// expiry onto the new group channel. Failure here is logged and does not
		// fail the already-committed group.
		creatorUser, uErr := g.userDB.QueryByUID(principal.UserID)
		if uErr != nil {
			g.Warn("CLI group creator lookup for msg auto delete failed", zap.Error(uErr), zap.String("group_no", resp.GroupNo))
		} else if creatorUser != nil && creatorUser.MsgExpireSecond > 0 {
			if obj := register.GetService(ChannelServiceName); obj != nil {
				if channelService, ok := obj.(chservice.IService); ok && channelService != nil {
					if chErr := channelService.CreateOrUpdateMsgAutoDelete(resp.GroupNo, common.ChannelTypeGroup.Uint8(), creatorUser.MsgExpireSecond); chErr != nil {
						g.Warn("CLI group msg auto delete update failed", zap.Error(chErr), zap.String("group_no", resp.GroupNo))
					}
				}
			}
		}
	}
	data := map[string]any{"group_id": resp.GroupNo, "name": resp.Name}
	if len(resp.SkippedMembers) > 0 {
		data["skipped_members"] = resp.SkippedMembers
	}
	cliv2.Respond(c, data)
}

func (g *Group) cliGetGroup(c *wkhttp.Context) {
	groupModel := g.cliGetGroupModel(c)
	if groupModel == nil {
		g.cliAccessFailure(c, ErrCLIGroupNotFound)
		return
	}
	groupResp, err := g.groupService.GetGroupDetail(groupModel.GroupNo, c.GetLoginUID())
	if err != nil {
		g.Error("CLI group detail query failed", zap.Error(err))
		cliv2.Fail(c, http.StatusInternalServerError, "INTERNAL_ERROR", "The request could not be completed.")
		return
	}
	if groupResp == nil {
		cliv2.Fail(c, http.StatusNotFound, "NOT_FOUND", "Group not found.")
		return
	}
	if cliv2.PrincipalFrom(c).BotID != "" {
		// For a Bot, the legacy GROUP.md write authority is bot_admin, not the
		// native member role. Derive the projection from the same predicate the
		// PUT /md handler enforces so the two can never disagree.
		allowed, adminErr := g.groupService.IsBotAdmin(groupModel.GroupNo, c.GetLoginUID())
		if adminErr != nil {
			g.Error("CLI Bot group detail admin query failed", zap.Error(adminErr))
			cliv2.Fail(c, http.StatusInternalServerError, "INTERNAL_ERROR", "The request could not be completed.")
			return
		}
		groupResp.CanEditGroupMd = allowed
	}
	cliv2.Respond(c, groupResp)
}

func (g *Group) cliGetGroupMembers(c *wkhttp.Context) {
	groupModel := g.cliGetGroupModel(c)
	if groupModel == nil {
		g.cliAccessFailure(c, ErrCLIGroupNotFound)
		return
	}
	page, size, ok := cliPage(c.Query("page"), c.Query("page_size"))
	if !ok {
		cliv2.Fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "Invalid pagination parameters.")
		return
	}
	loginUID, keyword := c.GetLoginUID(), c.Query("keyword")
	total, err := g.db.countMembersWithKeyword(groupModel.GroupNo, loginUID, keyword)
	if err != nil {
		g.Error("CLI group member count query failed", zap.Error(err))
		cliv2.Fail(c, http.StatusInternalServerError, "INTERNAL_ERROR", "The request could not be completed.")
		return
	}
	members, err := g.db.queryMembersWithKeyword(groupModel.GroupNo, loginUID, keyword, page, size)
	if err != nil {
		g.Error("CLI group members query failed", zap.Error(err))
		cliv2.Fail(c, http.StatusInternalServerError, "INTERNAL_ERROR", "The request could not be completed.")
		return
	}
	result := make([]memberDetailResp, 0, len(members))
	for _, member := range members {
		result = append(result, (memberDetailResp{}).from(member))
	}
	cliRespondPage(c, result, total, page, size)
}

func cliPage(rawPage, rawSize string) (uint64, uint64, bool) {
	page, size := uint64(1), uint64(20)
	if rawPage != "" {
		parsed, err := strconv.ParseUint(rawPage, 10, 64)
		if err != nil || parsed == 0 {
			return 0, 0, false
		}
		page = parsed
	}
	if rawSize != "" {
		parsed, err := strconv.ParseUint(rawSize, 10, 64)
		if err != nil || parsed == 0 || parsed > 100 {
			return 0, 0, false
		}
		size = parsed
	}
	const maxInt64 uint64 = 1<<63 - 1
	if page > maxInt64 || page-1 > maxInt64/size {
		return 0, 0, false
	}
	return page, size, true
}

func cliRespondPage(c *wkhttp.Context, data any, total int64, page, pageSize uint64) {
	c.JSON(http.StatusOK, map[string]any{
		"data": data,
		"pagination": map[string]any{
			"total": total, "page": page, "page_size": pageSize,
		},
	})
}

func (g *Group) cliGetGroupMD(c *wkhttp.Context) {
	groupModel := g.cliGetGroupModel(c)
	if groupModel == nil {
		g.cliAccessFailure(c, ErrCLIGroupNotFound)
		return
	}
	result, err := g.groupService.GetGroupMd(groupModel.GroupNo)
	if err != nil {
		g.Error("CLI group GROUP.md query failed", zap.Error(err))
		cliv2.Fail(c, http.StatusInternalServerError, "INTERNAL_ERROR", "The request could not be completed.")
		return
	}
	if result == nil {
		cliv2.Respond(c, groupMdResp{Content: "", Version: 0, UpdatedAt: nil, UpdatedBy: ""})
		return
	}
	cliv2.Respond(c, groupMdResp{Content: result.Content, Version: result.Version, UpdatedAt: result.UpdatedAt, UpdatedBy: result.UpdatedBy})
}

func (g *Group) cliUpdateGroupMD(c *wkhttp.Context) {
	groupModel := g.cliGetGroupModel(c)
	if groupModel == nil {
		g.cliAccessFailure(c, ErrCLIGroupNotFound)
		return
	}
	principal := cliv2.PrincipalFrom(c)
	operator := c.GetLoginUID()
	var allowed bool
	var err error
	if principal.BotID != "" {
		// The v1 Bot endpoint grants GROUP.md writes through bot_admin, not
		// through the native human role.
		allowed, err = g.groupService.IsBotAdmin(groupModel.GroupNo, operator)
	} else {
		allowed, err = g.groupService.IsCreatorOrManager(groupModel.GroupNo, operator)
	}
	if err != nil {
		g.Error("CLI group GROUP.md permission query failed", zap.Error(err))
		cliv2.Fail(c, http.StatusInternalServerError, "INTERNAL_ERROR", "The request could not be completed.")
		return
	}
	if !allowed {
		cliv2.Fail(c, http.StatusForbidden, "FORBIDDEN", "Access denied.")
		return
	}
	var req struct {
		Content string `json:"content"`
	}
	if err := c.BindJSON(&req); err != nil || len(req.Content) > GetGroupMdMaxSize() {
		cliv2.Fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "Invalid GROUP.md content.")
		return
	}
	updatedBy := operator
	version, err := g.groupService.UpdateGroupMd(groupModel.GroupNo, req.Content, updatedBy)
	if err != nil {
		g.Error("CLI group GROUP.md update failed", zap.Error(err))
		cliv2.Fail(c, http.StatusInternalServerError, "INTERNAL_ERROR", "The request could not be completed.")
		return
	}
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				g.Error("CLI GROUP.md notification panic", zap.Any("recover", recovered))
			}
		}()
		g.sendGroupMdNotification(groupModel.GroupNo, updatedBy, version, "group_md_updated", "GROUP.md updated")
	}()
	cliv2.Respond(c, map[string]any{"version": version})
}

func (g *Group) cliUpdateGroupInfo(c *wkhttp.Context) {
	groupModel := g.cliGetGroupModel(c)
	if groupModel == nil {
		g.cliAccessFailure(c, ErrCLIGroupNotFound)
		return
	}
	var req struct {
		Name   *string `json:"name"`
		Notice *string `json:"notice"`
	}
	if err := c.BindJSON(&req); err != nil || (req.Name == nil && req.Notice == nil) {
		cliv2.Fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "Invalid group update request.")
		return
	}
	if req.Name != nil && utf8.RuneCountInString(*req.Name) > MaxGroupNameLen {
		cliv2.Fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "Group name is too long.")
		return
	}
	operator := c.GetLoginUID()
	principal := cliv2.PrincipalFrom(c)
	if operator != principal.UserID {
		allowed, err := g.groupService.IsBotAdmin(groupModel.GroupNo, operator)
		if err != nil {
			g.Error("CLI Bot group update permission query failed", zap.Error(err))
			cliv2.Fail(c, http.StatusInternalServerError, "INTERNAL_ERROR", "The request could not be completed.")
			return
		}
		if !allowed {
			cliv2.Fail(c, http.StatusForbidden, "FORBIDDEN", "Access denied.")
			return
		}
	} else {
		if req.Notice != nil {
			allowed, err := g.groupService.IsCreatorOrManager(groupModel.GroupNo, operator)
			if err != nil {
				g.Error("CLI group update permission query failed", zap.Error(err))
				cliv2.Fail(c, http.StatusInternalServerError, "INTERNAL_ERROR", "The request could not be completed.")
				return
			}
			if !allowed {
				cliv2.Fail(c, http.StatusForbidden, "FORBIDDEN", "Access denied.")
				return
			}
		} else {
			internal, err := g.db.ExistMemberActiveInternal(operator, groupModel.GroupNo)
			if err != nil {
				g.Error("CLI group rename permission query failed", zap.Error(err))
				cliv2.Fail(c, http.StatusInternalServerError, "INTERNAL_ERROR", "The request could not be completed.")
				return
			}
			isBot, botErr := g.groupService.IsRobot(operator)
			if botErr != nil {
				g.Error("CLI group rename actor lookup failed", zap.Error(botErr))
				cliv2.Fail(c, http.StatusInternalServerError, "INTERNAL_ERROR", "The request could not be completed.")
				return
			}
			if !internal || isBot {
				cliv2.Fail(c, http.StatusForbidden, "FORBIDDEN", "Access denied.")
				return
			}
		}
	}
	if err := g.groupService.UpdateGroupInfo(&UpdateGroupInfoServiceReq{
		GroupNo:      groupModel.GroupNo,
		OperatorUID:  operator,
		OperatorName: c.GetLoginName(),
		Name:         req.Name,
		Notice:       req.Notice,
	}); err != nil {
		g.Error("CLI group update failed", zap.Error(err))
		if strings.Contains(err.Error(), "group not found or disbanded") {
			cliv2.Fail(c, http.StatusNotFound, "NOT_FOUND", "Group not found.")
			return
		}
		cliv2.Fail(c, http.StatusInternalServerError, "INTERNAL_ERROR", "The request could not be completed.")
		return
	}
	cliv2.Respond(c, map[string]any{"ok": true})
}

func (g *Group) cliAddGroupMembers(c *wkhttp.Context) {
	groupModel := g.cliGetGroupModel(c)
	if groupModel == nil {
		g.cliAccessFailure(c, ErrCLIGroupNotFound)
		return
	}
	var req memberAddReq
	if err := c.BindJSON(&req); err != nil || len(req.Members) == 0 || len(req.Members) > 200 {
		cliv2.Fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "Invalid members request.")
		return
	}
	principal := cliv2.PrincipalFrom(c)
	operator := c.GetLoginUID()
	members := req.Members
	var skippedBots []string
	if principal.BotID != "" {
		memberUsers, err := g.userDB.QueryByUIDs(req.Members)
		if err != nil {
			g.Error("CLI group member Bot lookup failed", zap.Error(err))
			cliv2.Fail(c, http.StatusInternalServerError, "INTERNAL_ERROR", "The request could not be completed.")
			return
		}
		robotSet := make(map[string]struct{}, len(memberUsers))
		for _, member := range memberUsers {
			if member.Robot == 1 {
				robotSet[member.UID] = struct{}{}
			}
		}
		members = make([]string, 0, len(req.Members))
		skippedBots = make([]string, 0)
		for _, uid := range req.Members {
			if _, isBot := robotSet[uid]; isBot {
				skippedBots = append(skippedBots, uid)
				continue
			}
			members = append(members, uid)
		}
		if len(members) == 0 {
			cliv2.Fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "At least one human member is required.")
			return
		}
	}

	if err := checkBotOwnership(g.ctx.DB(), operator, members); err != nil {
		if errors.Is(err, ErrBotOwnershipDenied) {
			cliv2.Fail(c, http.StatusForbidden, "FORBIDDEN", "Access denied.")
			return
		}
		g.Error("CLI group member Bot ownership check failed", zap.Error(err))
		cliv2.Fail(c, http.StatusInternalServerError, "INTERNAL_ERROR", "The request could not be completed.")
		return
	}
	// The file-helper admission switch and the invite=1 creator/manager gate
	// belong to the human invitation endpoint. The v1 Bot endpoint admits
	// humans with plain Bot membership, so a bound-Bot actor must not inherit
	// either check just because the group happens to carry invite=1.
	if principal.BotID == "" {
		appConfig, err := g.commonService.GetAppConfig()
		if err != nil {
			g.Error("CLI group app config query failed", zap.Error(err))
			cliv2.Fail(c, http.StatusInternalServerError, "INTERNAL_ERROR", "The request could not be completed.")
			return
		}
		if appConfig != nil && appConfig.InviteSystemAccountJoinGroupOn == 0 {
			for _, uid := range members {
				if uid == g.ctx.GetConfig().Account.FileHelperUID {
					cliv2.Fail(c, http.StatusForbidden, "FORBIDDEN", "Access denied.")
					return
				}
			}
		}
		if groupModel.Invite == 1 {
			allowed, err := g.groupService.IsCreatorOrManager(groupModel.GroupNo, operator)
			if err != nil {
				g.Error("CLI group invite permission query failed", zap.Error(err))
				cliv2.Fail(c, http.StatusInternalServerError, "INTERNAL_ERROR", "The request could not be completed.")
				return
			}
			if !allowed {
				cliv2.Fail(c, http.StatusForbidden, "FORBIDDEN", "Access denied.")
				return
			}
		}
		// Legacy human invitation also enforces Bot/Space isolation: a Bot must
		// be an active member of the inviter's effective Space. Bot-subject
		// invitations deliberately skip this, matching the v1 Bot endpoint.
		if err := g.cliCheckBotInviterSpace(groupModel, operator, members); err != nil {
			if errors.Is(err, errCLIBotOutsideInviterSpace) {
				cliv2.Fail(c, http.StatusForbidden, "FORBIDDEN", "Access denied.")
				return
			}
			g.Error("CLI group member Bot Space check failed", zap.Error(err), zap.String("group_no", groupModel.GroupNo))
			cliv2.Fail(c, http.StatusInternalServerError, "INTERNAL_ERROR", "The request could not be completed.")
			return
		}
	}
	resp, err := g.groupService.AddGroupMembers(&AddGroupMembersServiceReq{
		GroupNo:      groupModel.GroupNo,
		Members:      members,
		OperatorUID:  operator,
		OperatorName: c.GetLoginName(),
	})
	if err != nil {
		g.Error("CLI group member add failed", zap.Error(err))
		if strings.Contains(err.Error(), "group not found or disbanded") {
			cliv2.Fail(c, http.StatusNotFound, "NOT_FOUND", "Group not found.")
			return
		}
		cliv2.Fail(c, http.StatusInternalServerError, "INTERNAL_ERROR", "The request could not be completed.")
		return
	}
	data := map[string]any{"added": resp.Added}
	if principal.BotID != "" {
		data["skipped_bots"] = skippedBots
	}
	cliv2.Respond(c, data)
}

// cliCheckBotInviterSpace mirrors the legacy human invitation rule: when the
// group is bound to a Space, every invited Bot must be an active member of the
// inviter's effective Space — the group's Space, or the external inviter's
// source Space. A missing user row is treated as non-Bot, matching v1's
// COALESCE((SELECT robot ...), 0) lookup.
func (g *Group) cliCheckBotInviterSpace(groupModel *Model, operator string, members []string) error {
	if groupModel.SpaceID == "" {
		return nil
	}
	inviterSpaceID := groupModel.SpaceID
	operatorMember, err := g.db.QueryMemberWithUID(operator, groupModel.GroupNo)
	if err != nil {
		return err
	}
	if operatorMember != nil && operatorMember.IsExternal == 1 && operatorMember.SourceSpaceID != "" {
		inviterSpaceID = operatorMember.SourceSpaceID
	}
	memberUsers, err := g.userDB.QueryByUIDs(members)
	if err != nil {
		return err
	}
	for _, member := range memberUsers {
		if member.Robot != 1 {
			continue
		}
		inSpace, err := spacepkg.CheckMembership(g.ctx.DB(), inviterSpaceID, member.UID)
		if err != nil {
			return err
		}
		if !inSpace {
			return errCLIBotOutsideInviterSpace
		}
	}
	return nil
}

func (g *Group) cliRemoveGroupMembers(c *wkhttp.Context) {
	groupModel := g.cliGetGroupModel(c)
	if groupModel == nil {
		g.cliAccessFailure(c, ErrCLIGroupNotFound)
		return
	}
	var req memberRemoveReq
	if err := c.BindJSON(&req); err != nil || len(req.Members) == 0 {
		cliv2.Fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "Invalid members request.")
		return
	}
	members := uniqueCLIMembers(req.Members)
	operator := c.GetLoginUID()
	principal := cliv2.PrincipalFrom(c)
	if principal.BotID != "" {
		g.cliRemoveGroupMembersAsBot(c, groupModel, operator, members)
		return
	}
	operatorMember, err := g.db.QueryMemberWithUID(operator, groupModel.GroupNo)
	if err != nil || operatorMember == nil {
		if err != nil {
			g.Error("CLI group operator lookup failed", zap.Error(err))
			cliv2.Fail(c, http.StatusInternalServerError, "INTERNAL_ERROR", "The request could not be completed.")
		} else {
			cliv2.Fail(c, http.StatusForbidden, "FORBIDDEN", "Access denied.")
		}
		return
	}
	for _, uid := range members {
		if uid == operator {
			cliv2.Fail(c, http.StatusForbidden, "FORBIDDEN", "Access denied.")
			return
		}
	}
	isManager := operatorMember.Role == MemberRoleCreator || operatorMember.Role == MemberRoleManager
	botOwnerSelfRemoval := false
	if !isManager {
		ownedBots, err := g.db.QueryBotUIDsOwnedByUIDs(groupModel.GroupNo, []string{operator})
		if err != nil {
			g.Error("CLI group owned Bot query failed", zap.Error(err))
			cliv2.Fail(c, http.StatusInternalServerError, "INTERNAL_ERROR", "The request could not be completed.")
			return
		}
		owned := make(map[string]struct{}, len(ownedBots))
		for _, uid := range ownedBots {
			owned[uid] = struct{}{}
		}
		for _, uid := range members {
			if _, ok := owned[uid]; !ok {
				cliv2.Fail(c, http.StatusForbidden, "FORBIDDEN", "Access denied.")
				return
			}
		}
		botOwnerSelfRemoval = true
	}
	targets, err := g.db.QueryMembersWithUids(members, groupModel.GroupNo)
	if err != nil {
		g.Error("CLI group removal targets query failed", zap.Error(err))
		cliv2.Fail(c, http.StatusInternalServerError, "INTERNAL_ERROR", "The request could not be completed.")
		return
	}
	if len(targets) == 0 {
		cliv2.Fail(c, http.StatusNotFound, "NOT_FOUND", "Group member not found.")
		return
	}
	for _, member := range targets {
		if botOwnerSelfRemoval && member.Role != MemberRoleCommon {
			cliv2.Fail(c, http.StatusForbidden, "FORBIDDEN", "Access denied.")
			return
		}
		if operatorMember.Role == MemberRoleManager && member.Role != MemberRoleCommon {
			cliv2.Fail(c, http.StatusForbidden, "FORBIDDEN", "Access denied.")
			return
		}
	}
	resp, err := g.groupService.RemoveGroupMembers(&RemoveGroupMembersServiceReq{
		GroupNo:             groupModel.GroupNo,
		Members:             members,
		OperatorUID:         operator,
		OperatorName:        c.GetLoginName(),
		BotOwnerSelfRemoval: botOwnerSelfRemoval,
	})
	if err != nil {
		g.Error("CLI group member remove failed", zap.Error(err))
		if strings.Contains(err.Error(), "none of the members are in this group") || strings.Contains(err.Error(), "group not found or disbanded") {
			cliv2.Fail(c, http.StatusNotFound, "NOT_FOUND", "Group member not found.")
			return
		}
		cliv2.Fail(c, http.StatusInternalServerError, "INTERNAL_ERROR", "The request could not be completed.")
		return
	}
	if botOwnerSelfRemoval && resp != nil && len(missingRemovalTargets(members, resp.RemovedUIDs)) > 0 {
		cliv2.Fail(c, http.StatusConflict, "CONFLICT", "The group membership changed; retry the request.")
		return
	}
	cliv2.Respond(c, map[string]any{"removed": resp.Removed})
}

// cliRemoveGroupMembersAsBot mirrors the v1 Bot endpoint. bot_admin is the
// authority (plain Bot membership is not enough), the acting Bot is silently
// dropped from the target list, and only common-role members may be removed:
// creator and human managers are protected exactly as in the legacy handler.
func (g *Group) cliRemoveGroupMembersAsBot(c *wkhttp.Context, groupModel *Model, operator string, members []string) {
	allowed, err := g.groupService.IsBotAdmin(groupModel.GroupNo, operator)
	if err != nil {
		g.Error("CLI Bot group admin query failed", zap.Error(err))
		cliv2.Fail(c, http.StatusInternalServerError, "INTERNAL_ERROR", "The request could not be completed.")
		return
	}
	if !allowed {
		cliv2.Fail(c, http.StatusForbidden, "FORBIDDEN", "Access denied.")
		return
	}
	targets := make([]string, 0, len(members))
	for _, uid := range members {
		if uid != operator {
			targets = append(targets, uid)
		}
	}
	if len(targets) == 0 {
		cliv2.Respond(c, map[string]any{"removed": 0})
		return
	}
	// The role guard filters rows after MySQL collation resolves uid matching,
	// so a case variant cannot slip past it while still hitting the real
	// manager row in the service layer.
	targetMembers, err := g.db.QueryMembersWithUids(targets, groupModel.GroupNo)
	if err != nil {
		g.Error("CLI Bot removal targets query failed", zap.Error(err))
		cliv2.Fail(c, http.StatusInternalServerError, "INTERNAL_ERROR", "The request could not be completed.")
		return
	}
	if len(targetMembers) == 0 {
		cliv2.Fail(c, http.StatusNotFound, "NOT_FOUND", "Group member not found.")
		return
	}
	for _, member := range targetMembers {
		if member.Role != MemberRoleCommon {
			cliv2.Fail(c, http.StatusForbidden, "FORBIDDEN", "Access denied.")
			return
		}
	}
	resp, err := g.groupService.RemoveGroupMembers(&RemoveGroupMembersServiceReq{
		GroupNo:      groupModel.GroupNo,
		Members:      targets,
		OperatorUID:  operator,
		OperatorName: c.GetLoginName(),
	})
	if err != nil {
		g.Error("CLI Bot group member remove failed", zap.Error(err))
		if strings.Contains(err.Error(), "none of the members are in this group") || strings.Contains(err.Error(), "group not found or disbanded") {
			cliv2.Fail(c, http.StatusNotFound, "NOT_FOUND", "Group member not found.")
			return
		}
		cliv2.Fail(c, http.StatusInternalServerError, "INTERNAL_ERROR", "The request could not be completed.")
		return
	}
	cliv2.Respond(c, map[string]any{"removed": resp.Removed})
}

func uniqueCLIMembers(members []string) []string {
	result := make([]string, 0, len(members))
	seen := make(map[string]struct{}, len(members))
	for _, member := range members {
		member = strings.TrimSpace(member)
		if member == "" {
			continue
		}
		if _, ok := seen[member]; ok {
			continue
		}
		seen[member] = struct{}{}
		result = append(result, member)
	}
	return result
}
