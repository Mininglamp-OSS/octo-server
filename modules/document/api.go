package document

import (
	"errors"
	"strconv"
	"strings"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n/codes"
	spacepkg "github.com/Mininglamp-OSS/octo-server/pkg/space"
	appwkhttp "github.com/Mininglamp-OSS/octo-server/pkg/wkhttp"
	"go.uber.org/zap"
)

type Document struct {
	ctx *config.Context
	log.Log
	service *DocumentService
}

func New(ctx *config.Context) *Document {
	return &Document{
		ctx:     ctx,
		Log:     log.NewTLog("Document"),
		service: NewDocumentService(newDocumentDB(ctx)),
	}
}

func (d *Document) Route(r *wkhttp.WKHttp) {
	uidLimit := appwkhttp.SharedUIDRateLimiter(r, d.ctx)
	auth := r.Group("/v1/documents", d.ctx.AuthMiddleware(r), uidLimit, spacepkg.SpaceMiddleware(d.ctx))
	{
		auth.GET("/state", d.state)
		auth.POST("/upload", d.upload)
		auth.POST("/archive", d.archive)
		auth.POST("/spaces", d.createSpace)
		auth.POST("/spaces/:space_id", d.updateSpace)
		auth.POST("/spaces/:space_id/disable", d.disableSpace)
		auth.POST("/spaces/:space_id/bind-conversation", d.bindConversation)
		auth.GET("/spaces/:space_id/bindings/search", d.searchSpaceBindings)
		auth.POST("/spaces/:space_id/bindings/:binding_id/remove", d.unbindConversation)
		auth.GET("/spaces/:space_id/members/search", d.searchSpaceMembers)
		auth.POST("/spaces/:space_id/members", d.saveSpaceMember)
		auth.POST("/spaces/:space_id/members/:member_uid/remove", d.removeSpaceMember)
		auth.POST("/:asset_id/rename", d.renameAsset)
		auth.POST("/:asset_id/move", d.moveAsset)
		auth.POST("/:asset_id/preview", d.preview)
		auth.POST("/:asset_id/download", d.download)
		auth.POST("/:asset_id/trash", d.trash)
		auth.POST("/:asset_id/restore", d.restore)
		auth.POST("/:asset_id/permanent-delete", d.permanentDelete)
		auth.POST("/trash/empty", d.emptyTrash)
		auth.GET("/channel-storage-space", d.channelStorageSpace)
		auth.GET("/source/check", d.checkSource)
	}
}

func (d *Document) createSpace(c *wkhttp.Context) {
	var req SaveSpaceReq
	if err := c.BindJSON(&req); err != nil {
		d.respondInvalidRequest(c, err)
		return
	}
	tenantSpaceID, ok := d.requestTenantSpaceID(c)
	if !ok {
		return
	}
	state, err := d.service.CreateSpace(c.GetLoginUID(), tenantSpaceID, req)
	d.respondState(c, state, err)
}

func (d *Document) updateSpace(c *wkhttp.Context) {
	var req SaveSpaceReq
	if err := c.BindJSON(&req); err != nil {
		d.respondInvalidRequest(c, err)
		return
	}
	tenantSpaceID, ok := d.requestTenantSpaceID(c)
	if !ok {
		return
	}
	state, err := d.service.UpdateSpace(c.GetLoginUID(), tenantSpaceID, c.Param("space_id"), req)
	d.respondState(c, state, err)
}

func (d *Document) disableSpace(c *wkhttp.Context) {
	tenantSpaceID, ok := d.requestTenantSpaceID(c)
	if !ok {
		return
	}
	state, err := d.service.DisableSpace(c.GetLoginUID(), tenantSpaceID, c.Param("space_id"))
	d.respondState(c, state, err)
}

func (d *Document) saveSpaceMember(c *wkhttp.Context) {
	var req SaveSpaceMemberReq
	if err := c.BindJSON(&req); err != nil {
		d.respondInvalidRequest(c, err)
		return
	}
	tenantSpaceID, ok := d.requestTenantSpaceID(c)
	if !ok {
		return
	}
	state, err := d.service.UpsertSpaceMember(c.GetLoginUID(), tenantSpaceID, c.Param("space_id"), req)
	d.respondState(c, state, err)
}

func (d *Document) searchSpaceMembers(c *wkhttp.Context) {
	tenantSpaceID, ok := d.requestTenantSpaceID(c)
	if !ok {
		return
	}
	candidates, err := d.service.SearchSpaceMemberCandidates(
		c.GetLoginUID(),
		tenantSpaceID,
		c.Param("space_id"),
		c.Query("keyword"),
	)
	if err != nil {
		d.respondDocumentError(c, err, false)
		return
	}
	c.Response(candidates)
}

func (d *Document) removeSpaceMember(c *wkhttp.Context) {
	tenantSpaceID, ok := d.requestTenantSpaceID(c)
	if !ok {
		return
	}
	state, err := d.service.RemoveSpaceMember(c.GetLoginUID(), tenantSpaceID, c.Param("space_id"), c.Param("member_uid"))
	d.respondState(c, state, err)
}

func (d *Document) renameAsset(c *wkhttp.Context) {
	var req RenameAssetReq
	if err := c.BindJSON(&req); err != nil {
		d.respondInvalidRequest(c, err)
		return
	}
	tenantSpaceID, ok := d.requestTenantSpaceID(c)
	if !ok {
		return
	}
	state, err := d.service.RenameAsset(c.GetLoginUID(), tenantSpaceID, c.Param("asset_id"), req)
	d.respondState(c, state, err)
}

func (d *Document) moveAsset(c *wkhttp.Context) {
	var req MoveAssetReq
	if err := c.BindJSON(&req); err != nil {
		d.respondInvalidRequest(c, err)
		return
	}
	tenantSpaceID, ok := d.requestTenantSpaceID(c)
	if !ok {
		return
	}
	state, err := d.service.MoveAsset(c.GetLoginUID(), tenantSpaceID, c.Param("asset_id"), req)
	d.respondState(c, state, err)
}

func (d *Document) permanentDelete(c *wkhttp.Context) {
	tenantSpaceID, ok := d.requestTenantSpaceID(c)
	if !ok {
		return
	}
	state, err := d.service.PermanentDelete(c.GetLoginUID(), tenantSpaceID, c.Param("asset_id"))
	d.respondState(c, state, err)
}

func (d *Document) emptyTrash(c *wkhttp.Context) {
	tenantSpaceID, ok := d.requestTenantSpaceID(c)
	if !ok {
		return
	}
	state, err := d.service.EmptyTrash(c.GetLoginUID(), tenantSpaceID)
	d.respondState(c, state, err)
}

func (d *Document) bindConversation(c *wkhttp.Context) {
	var req BindConversationReq
	if err := c.BindJSON(&req); err != nil {
		d.respondInvalidRequest(c, err)
		return
	}
	if req.DocumentSpaceID == "" {
		req.DocumentSpaceID = c.Param("space_id")
	}
	tenantSpaceID, ok := d.requestTenantSpaceID(c)
	if !ok {
		return
	}
	state, err := d.service.BindConversation(c.GetLoginUID(), tenantSpaceID, req)
	d.respondState(c, state, err)
}

func (d *Document) unbindConversation(c *wkhttp.Context) {
	tenantSpaceID, ok := d.requestTenantSpaceID(c)
	if !ok {
		return
	}
	state, err := d.service.UnbindConversation(c.GetLoginUID(), tenantSpaceID, c.Param("space_id"), c.Param("binding_id"))
	d.respondState(c, state, err)
}

func (d *Document) searchSpaceBindings(c *wkhttp.Context) {
	tenantSpaceID, ok := d.requestTenantSpaceID(c)
	if !ok {
		return
	}
	candidates, err := d.service.SearchBindingConversations(
		c.GetLoginUID(),
		tenantSpaceID,
		c.Param("space_id"),
		c.Query("keyword"),
	)
	if err != nil {
		d.respondDocumentError(c, err, false)
		return
	}
	c.Response(candidates)
}

func (d *Document) channelStorageSpace(c *wkhttp.Context) {
	sourceChannelType, _ := strconv.Atoi(c.Query("source_channel_type"))
	tenantSpaceID, ok := d.requestTenantSpaceID(c)
	if !ok {
		return
	}
	resp, err := d.service.ChannelStorageSpace(
		c.GetLoginUID(),
		tenantSpaceID,
		c.Query("source_channel_id"),
		uint8(sourceChannelType),
	)
	if err != nil {
		d.respondDocumentError(c, err, false)
		return
	}
	c.Response(resp)
}

func (d *Document) state(c *wkhttp.Context) {
	tenantSpaceID, ok := d.requestTenantSpaceID(c)
	if !ok {
		return
	}
	state, err := d.service.State(c.GetLoginUID(), tenantSpaceID)
	d.respondState(c, state, err)
}

func (d *Document) upload(c *wkhttp.Context) {
	var req UploadReq
	if err := c.BindJSON(&req); err != nil {
		d.respondInvalidRequest(c, err)
		return
	}
	tenantSpaceID, ok := d.requestTenantSpaceID(c)
	if !ok {
		return
	}
	state, err := d.service.Upload(c.GetLoginUID(), tenantSpaceID, req)
	d.respondState(c, state, err)
}

func (d *Document) archive(c *wkhttp.Context) {
	var req ArchiveReq
	if err := c.BindJSON(&req); err != nil {
		d.respondInvalidRequest(c, err)
		return
	}
	tenantSpaceID, ok := d.requestTenantSpaceID(c)
	if !ok {
		return
	}
	state, err := d.service.Archive(c.GetLoginUID(), tenantSpaceID, req)
	d.respondState(c, state, err)
}

func (d *Document) preview(c *wkhttp.Context) {
	tenantSpaceID, ok := d.requestTenantSpaceID(c)
	if !ok {
		return
	}
	state, err := d.service.Preview(c.GetLoginUID(), tenantSpaceID, c.Param("asset_id"))
	d.respondState(c, state, err)
}

func (d *Document) download(c *wkhttp.Context) {
	tenantSpaceID, ok := d.requestTenantSpaceID(c)
	if !ok {
		return
	}
	state, err := d.service.Download(c.GetLoginUID(), tenantSpaceID, c.Param("asset_id"))
	d.respondState(c, state, err)
}

func (d *Document) trash(c *wkhttp.Context) {
	tenantSpaceID, ok := d.requestTenantSpaceID(c)
	if !ok {
		return
	}
	state, err := d.service.Trash(c.GetLoginUID(), tenantSpaceID, c.Param("asset_id"))
	d.respondState(c, state, err)
}

func (d *Document) restore(c *wkhttp.Context) {
	tenantSpaceID, ok := d.requestTenantSpaceID(c)
	if !ok {
		return
	}
	state, err := d.service.Restore(c.GetLoginUID(), tenantSpaceID, c.Param("asset_id"))
	d.respondState(c, state, err)
}

func (d *Document) checkSource(c *wkhttp.Context) {
	assetID := c.Query("asset_id")
	tenantSpaceID, ok := d.requestTenantSpaceID(c)
	if !ok {
		return
	}
	accessible, err := d.service.CheckSource(c.GetLoginUID(), tenantSpaceID, assetID)
	if err != nil {
		d.Error("检查来源会话失败", zap.Error(err))
		d.respondDocumentError(c, err, false)
		return
	}
	c.Response(map[string]bool{"accessible": accessible})
}

func (d *Document) requestTenantSpaceID(c *wkhttp.Context) (string, bool) {
	tenantSpaceID, ok := tenantSpaceID(c)
	if !ok {
		d.respondDocumentError(c, errors.New("空间不能为空"), false)
		return "", false
	}
	return tenantSpaceID, true
}

func (d *Document) respondState(c *wkhttp.Context, state *DocumentStateResp, err error) {
	if err != nil {
		d.respondDocumentError(c, err, true)
		return
	}
	c.Response(state)
}

func (d *Document) respondInvalidRequest(c *wkhttp.Context, err error) {
	d.Warn("文档中心请求参数无效", zap.Error(err))
	httperr.ResponseErrorL(c, errcode.ErrDocumentRequestInvalid, nil, nil)
}

func (d *Document) respondDocumentError(c *wkhttp.Context, err error, mutation bool) {
	code := documentErrorCode(err, mutation)
	if code.Internal {
		d.Error("文档中心请求失败", zap.String("code", code.ID), zap.Error(err))
	} else {
		d.Warn("文档中心请求失败", zap.String("code", code.ID), zap.Error(err))
	}
	httperr.ResponseErrorL(c, code, nil, nil)
}

func documentErrorCode(err error, mutation bool) codes.Code {
	if err == nil {
		return errcode.ErrDocumentQueryFailed
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "不存在"):
		return errcode.ErrDocumentNotFound
	case strings.Contains(msg, "无权") ||
		strings.Contains(msg, "不是空间成员") ||
		strings.Contains(msg, "仅空间所有者") ||
		strings.Contains(msg, "文件已在回收站") ||
		strings.Contains(msg, "只能彻底删除回收站"):
		return errcode.ErrDocumentForbidden
	case strings.Contains(msg, "不能为空") ||
		strings.Contains(msg, "无效") ||
		strings.Contains(msg, "缺失") ||
		strings.Contains(msg, "已存在") ||
		strings.Contains(msg, "不支持") ||
		strings.Contains(msg, "非法"):
		return errcode.ErrDocumentRequestInvalid
	case mutation:
		return errcode.ErrDocumentStoreFailed
	default:
		return errcode.ErrDocumentQueryFailed
	}
}

func tenantSpaceID(c *wkhttp.Context) (string, bool) {
	if spaceID := spacepkg.GetSpaceID(c); spaceID != "" {
		return spaceID, true
	}
	return "", false
}
