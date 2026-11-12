package thread

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/modules/group"
	aiteampkg "github.com/Mininglamp-OSS/octo-server/pkg/aiteam"
	"github.com/Mininglamp-OSS/octo-server/pkg/cliv2"
	"go.uber.org/zap"
)

func (t *Thread) registerCLIThreadRoutes(r *wkhttp.WKHttp) {
	groupGuard := []wkhttp.HandlerFunc{t.cliGroupAccess}
	threadGuard := []wkhttp.HandlerFunc{t.cliThreadAccess}
	groupMutationGuard := []wkhttp.HandlerFunc{t.cliProtectAIContainerMutation, t.cliGroupAccess}
	threadMutationGuard := []wkhttp.HandlerFunc{t.cliProtectAIContainerMutation, t.cliThreadAccess}
	for _, route := range []cliv2.Route{
		{Method: http.MethodPost, Path: "/v2/cli/groups/:group_id/threads", Action: cliv2.ActionThreadsWrite, User: t.cliCreateThread, Bot: t.cliCreateThread, Middlewares: groupMutationGuard},
		{Method: http.MethodGet, Path: "/v2/cli/groups/:group_id/threads", Action: cliv2.ActionThreadsRead, User: t.cliListThreads, Bot: t.cliListThreads, Middlewares: groupGuard},
		{Method: http.MethodGet, Path: "/v2/cli/groups/:group_id/threads/:thread_id", Action: cliv2.ActionThreadsRead, User: t.cliGetThread, Bot: t.cliGetThread, Middlewares: threadGuard},
		{Method: http.MethodGet, Path: "/v2/cli/groups/:group_id/threads/:thread_id/members", Action: cliv2.ActionThreadsRead, User: t.cliGetThreadMembers, Bot: t.cliGetThreadMembers, Middlewares: threadGuard},
		{Method: http.MethodPost, Path: "/v2/cli/groups/:group_id/threads/:thread_id/join", Action: cliv2.ActionThreadsWrite, User: t.cliJoinThread, Bot: t.cliJoinThread, Middlewares: threadMutationGuard},
		{Method: http.MethodPost, Path: "/v2/cli/groups/:group_id/threads/:thread_id/leave", Action: cliv2.ActionThreadsWrite, User: t.cliLeaveThread, Bot: t.cliLeaveThread, Middlewares: threadMutationGuard},
		{Method: http.MethodGet, Path: "/v2/cli/groups/:group_id/threads/:thread_id/md", Action: cliv2.ActionThreadsRead, User: t.cliGetThreadMD, Bot: t.cliGetThreadMD, Middlewares: threadGuard},
		{Method: http.MethodPut, Path: "/v2/cli/groups/:group_id/threads/:thread_id/md", Action: cliv2.ActionThreadsWrite, User: t.cliUpdateThreadMD, Bot: t.cliUpdateThreadMD, Middlewares: threadMutationGuard},
	} {
		cliv2.Add(r, route)
	}
}

func (t *Thread) cliProtectAIContainerMutation(c *wkhttp.Context) {
	groupNo := c.Param("group_id")
	protected, err := aiteampkg.IsProtectedGroup(t.ctx.DB(), groupNo)
	if err != nil {
		t.Error("CLI AI container protection query failed", zap.Error(err), zap.String("group_no", groupNo))
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

func (t *Thread) cliGroupAccess(c *wkhttp.Context) {
	_, ok := t.cliAuthorizeGroup(c)
	if !ok {
		c.Abort()
		return
	}
	c.Next()
}

func (t *Thread) cliThreadAccess(c *wkhttp.Context) {
	groupModel, ok := t.cliAuthorizeGroup(c)
	if !ok {
		c.Abort()
		return
	}
	threadID := c.Param("thread_id")
	if !IsValidShortID(threadID) {
		cliv2.Fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "Invalid thread ID.")
		c.Abort()
		return
	}
	threadModel, err := t.db.QueryByGroupNoAndShortID(groupModel.GroupNo, threadID)
	if err != nil {
		t.Error("CLI thread authorization query failed", zap.Error(err))
		cliv2.Fail(c, http.StatusInternalServerError, "INTERNAL_ERROR", "The request could not be completed.")
		c.Abort()
		return
	}
	if threadModel == nil || threadModel.Status == ThreadStatusDeleted {
		cliv2.Fail(c, http.StatusNotFound, "NOT_FOUND", "Thread not found.")
		c.Abort()
		return
	}
	c.Set("cliv2.thread", threadModel)
	c.Next()
}

func (t *Thread) cliAuthorizeGroup(c *wkhttp.Context) (*group.Model, bool) {
	principal := cliv2.PrincipalFrom(c)
	model, err := group.CheckCLIGroupAccess(t.ctx, t.groupDB, c.Param("group_id"), principal.SpaceID, principal.UserID, c.GetLoginUID())
	if err == nil {
		return model, true
	}
	switch {
	case err == group.ErrCLIGroupNotFound:
		cliv2.Fail(c, http.StatusNotFound, "NOT_FOUND", "Group not found.")
	case err == group.ErrCLISpaceAccessDenied, err == group.ErrCLIGroupAccessDenied:
		cliv2.Fail(c, http.StatusForbidden, "FORBIDDEN", "Access denied.")
	default:
		t.Error("CLI thread group authorization query failed", zap.Error(err))
		cliv2.Fail(c, http.StatusInternalServerError, "INTERNAL_ERROR", "The request could not be completed.")
	}
	return nil, false
}

func (t *Thread) cliThreadModel(c *wkhttp.Context) *Model {
	value, _ := c.Get("cliv2.thread")
	threadModel, _ := value.(*Model)
	return threadModel
}

func (t *Thread) cliCreateThread(c *wkhttp.Context) {
	groupNo := c.Param("group_id")
	var req struct {
		Name                 string          `json:"name"`
		SourceMessageID      *int64          `json:"source_message_id"`
		SourceMessagePayload json.RawMessage `json:"source_message_payload"`
	}
	if err := c.BindJSON(&req); err != nil || strings.TrimSpace(req.Name) == "" || utf8.RuneCountInString(req.Name) > 100 {
		cliv2.Fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "Invalid thread request.")
		return
	}
	// The v1 Bot create contract has no source_message_payload field: the shared
	// CreateThread service forwards a caller-supplied payload under the source
	// message's real sender, so a Bot must not be able to smuggle one in. Refuse
	// the request before any thread row exists; source_message_id alone is fine.
	if cliv2.PrincipalFrom(c).BotID != "" && len(req.SourceMessagePayload) > 0 {
		cliv2.Fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "Invalid thread request.")
		return
	}
	if len(req.SourceMessagePayload) > 0 {
		if req.SourceMessageID == nil || len(req.SourceMessagePayload) > maxSourcePayloadBytes || !json.Valid(req.SourceMessagePayload) || bytes.Equal(bytes.TrimSpace(req.SourceMessagePayload), []byte("null")) {
			cliv2.Fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "Invalid source message.")
			return
		}
	}
	resp, err := t.service.CreateThread(&CreateThreadReq{
		GroupNo:              groupNo,
		Name:                 req.Name,
		CreatorUID:           c.GetLoginUID(),
		CreatorName:          c.GetLoginName(),
		SourceMessageID:      req.SourceMessageID,
		SourceMessagePayload: req.SourceMessagePayload,
	})
	if err != nil {
		t.Error("CLI thread creation failed", zap.Error(err), zap.String("group_id", groupNo))
		t.cliThreadError(c, err)
		return
	}
	cliv2.Respond(c, resp)
}

func (t *Thread) cliListThreads(c *wkhttp.Context) {
	groupNo := c.Param("group_id")
	statuses, err := parseListThreadStatuses(c.Query("status"))
	if err != nil {
		cliv2.Fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "Invalid thread status filter.")
		return
	}
	page, size, ok := cliThreadPage(c.Query("page"), c.Query("page_size"))
	if !ok {
		cliv2.Fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "Invalid pagination parameters.")
		return
	}
	threads, total, err := t.service.GetThreads(groupNo, statuses, page, size)
	if err != nil {
		t.Error("CLI thread list query failed", zap.Error(err))
		cliv2.Fail(c, http.StatusInternalServerError, "INTERNAL_ERROR", "The request could not be completed.")
		return
	}
	cliRespondPage(c, threads, total, page, size)
}

func cliThreadPage(rawPage, rawSize string) (int64, int64, bool) {
	page, size := int64(1), int64(20)
	if rawPage != "" {
		parsed, err := strconv.ParseInt(rawPage, 10, 64)
		if err != nil || parsed < 1 {
			return 0, 0, false
		}
		page = parsed
	}
	if rawSize != "" {
		parsed, err := strconv.ParseInt(rawSize, 10, 64)
		if err != nil || parsed < 1 || parsed > MaxThreadPageSize {
			return 0, 0, false
		}
		size = parsed
	}
	const maxInt64 = int64(1<<63 - 1)
	if page-1 > maxInt64/size {
		return 0, 0, false
	}
	return page, size, true
}

func cliRespondPage(c *wkhttp.Context, data any, total int64, page, pageSize int64) {
	c.JSON(http.StatusOK, map[string]any{
		"data": data,
		"pagination": map[string]any{
			"total": total, "page": page, "page_size": pageSize,
		},
	})
}

func (t *Thread) cliGetThread(c *wkhttp.Context) {
	threadModel := t.cliThreadModel(c)
	if threadModel == nil {
		cliv2.Fail(c, http.StatusNotFound, "NOT_FOUND", "Thread not found.")
		return
	}
	resp, err := t.service.GetThread(threadModel.GroupNo, threadModel.ShortID, c.GetLoginUID())
	if err != nil {
		t.Error("CLI thread detail query failed", zap.Error(err))
		t.cliThreadError(c, err)
		return
	}
	cliv2.Respond(c, resp)
}

func (t *Thread) cliGetThreadMembers(c *wkhttp.Context) {
	threadModel := t.cliThreadModel(c)
	if threadModel == nil {
		cliv2.Fail(c, http.StatusNotFound, "NOT_FOUND", "Thread not found.")
		return
	}
	page, size, ok := cliThreadPage(c.Query("page"), c.Query("page_size"))
	if !ok {
		cliv2.Fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "Invalid pagination parameters.")
		return
	}
	members, err := t.service.GetMembers(threadModel.GroupNo, threadModel.ShortID)
	if err != nil {
		t.Error("CLI thread member query failed", zap.Error(err))
		t.cliThreadError(c, err)
		return
	}
	total := int64(len(members))
	offset := (page - 1) * size
	if offset >= total {
		members = members[:0]
	} else {
		start := int(offset)
		remaining := len(members) - start
		if remaining > int(size) {
			members = members[start : start+int(size)]
		} else {
			members = members[start:]
		}
	}
	cliRespondPage(c, members, total, page, size)
}

func (t *Thread) cliJoinThread(c *wkhttp.Context) {
	threadModel := t.cliThreadModel(c)
	if threadModel == nil {
		cliv2.Fail(c, http.StatusNotFound, "NOT_FOUND", "Thread not found.")
		return
	}
	if err := t.service.JoinThread(threadModel.GroupNo, threadModel.ShortID, c.GetLoginUID()); err != nil {
		t.Error("CLI thread join failed", zap.Error(err))
		t.cliThreadError(c, err)
		return
	}
	cliv2.Respond(c, map[string]any{"ok": true})
}

func (t *Thread) cliLeaveThread(c *wkhttp.Context) {
	threadModel := t.cliThreadModel(c)
	if threadModel == nil {
		cliv2.Fail(c, http.StatusNotFound, "NOT_FOUND", "Thread not found.")
		return
	}
	if err := t.service.LeaveThread(threadModel.GroupNo, threadModel.ShortID, c.GetLoginUID()); err != nil {
		t.Error("CLI thread leave failed", zap.Error(err))
		t.cliThreadError(c, err)
		return
	}
	cliv2.Respond(c, map[string]any{"ok": true})
}

func (t *Thread) cliGetThreadMD(c *wkhttp.Context) {
	threadModel := t.cliThreadModel(c)
	if threadModel == nil {
		cliv2.Fail(c, http.StatusNotFound, "NOT_FOUND", "Thread not found.")
		return
	}
	result, err := t.service.GetThreadMd(threadModel.GroupNo, threadModel.ShortID)
	if err != nil {
		t.Error("CLI thread GROUP.md query failed", zap.Error(err))
		cliv2.Fail(c, http.StatusInternalServerError, "INTERNAL_ERROR", "The request could not be completed.")
		return
	}
	if result == nil {
		cliv2.Respond(c, map[string]any{"content": "", "version": 0, "updated_at": nil, "updated_by": ""})
		return
	}
	cliv2.Respond(c, result)
}

func (t *Thread) cliUpdateThreadMD(c *wkhttp.Context) {
	threadModel := t.cliThreadModel(c)
	if threadModel == nil {
		cliv2.Fail(c, http.StatusNotFound, "NOT_FOUND", "Thread not found.")
		return
	}
	var req struct {
		Content string `json:"content"`
	}
	if err := c.BindJSON(&req); err != nil || strings.TrimSpace(req.Content) == "" || len(req.Content) > group.GetGroupMdMaxSize() {
		cliv2.Fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "Invalid thread GROUP.md content.")
		return
	}
	principal := cliv2.PrincipalFrom(c)
	var allowed bool
	var err error
	if principal.BotID != "" {
		// The v1 Bot thread endpoint gates GROUP.md writes on bot_admin, not on
		// the Bot's thread creator identity or its native group role. Reusing the
		// Bot's own login UID keeps the stored author consistent with enforcement.
		allowed, err = t.groupService.IsBotAdmin(threadModel.GroupNo, c.GetLoginUID())
	} else {
		allowed, err = t.service.CanEditThreadMd(threadModel.GroupNo, threadModel.ShortID, c.GetLoginUID())
	}
	if err != nil {
		t.Error("CLI thread GROUP.md permission query failed", zap.Error(err))
		cliv2.Fail(c, http.StatusInternalServerError, "INTERNAL_ERROR", "The request could not be completed.")
		return
	}
	if !allowed {
		cliv2.Fail(c, http.StatusForbidden, "FORBIDDEN", "Access denied.")
		return
	}
	updatedBy := c.GetLoginUID()
	groupNo, threadID := threadModel.GroupNo, threadModel.ShortID
	version, err := t.service.UpdateThreadMd(groupNo, threadID, req.Content, updatedBy)
	if err != nil {
		t.Error("CLI thread GROUP.md update failed", zap.Error(err))
		cliv2.Fail(c, http.StatusInternalServerError, "INTERNAL_ERROR", "The request could not be completed.")
		return
	}
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				t.Error("CLI thread GROUP.md notification panic", zap.Any("recover", recovered))
			}
		}()
		t.sendThreadMdNotification(groupNo, threadID, updatedBy, version, "thread_md_updated", "Thread GROUP.md updated")
	}()
	cliv2.Respond(c, map[string]any{"version": version})
}

func (t *Thread) cliThreadError(c *wkhttp.Context, err error) {
	message := strings.ToLower(err.Error())
	switch {
	case strings.Contains(message, "not found"), strings.Contains(message, "deleted"):
		cliv2.Fail(c, http.StatusNotFound, "NOT_FOUND", "Thread not found.")
	case strings.Contains(message, "not a group member"), strings.Contains(message, "no permission"), strings.Contains(message, "creator cannot leave"):
		cliv2.Fail(c, http.StatusForbidden, "FORBIDDEN", "Access denied.")
	case strings.Contains(message, "name is required"), strings.Contains(message, "invalid"):
		cliv2.Fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "Invalid thread request.")
	default:
		cliv2.Fail(c, http.StatusInternalServerError, "INTERNAL_ERROR", "The request could not be completed.")
	}
}
