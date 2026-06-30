package sticker

import (
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	commonmod "github.com/Mininglamp-OSS/octo-server/modules/common"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	appwkhttp "github.com/Mininglamp-OSS/octo-server/pkg/wkhttp"
	"go.uber.org/zap"
)

// field-length caps. path is bounded by the sticker.path column (VARCHAR 512);
// placeholder by sticker.placeholder (VARCHAR 100). Enforced in Go so an
// oversized value gets a clean 400 instead of a DB truncation/error.
const (
	maxStickerPathLen        = 512
	maxStickerPlaceholderLen = 100
	// defaultStickerPlaceholder is stored when the client sends no placeholder,
	// so conversation digests / push notifications have a sensible fallback.
	defaultStickerPlaceholder = "[表情]"
)

// Sticker 用户自定义贴纸 API。
type Sticker struct {
	ctx *config.Context
	log.Log
	db       *stickerDB
	settings *commonmod.SystemSettings
}

// New 创建 Sticker 实例。settings 走进程内共享单例，配额变更（管理端写
// system_setting）经其 60s 快照在多实例间收敛。
func New(ctx *config.Context) *Sticker {
	return &Sticker{
		ctx:      ctx,
		Log:      log.NewTLog("Sticker"),
		db:       newStickerDB(ctx),
		settings: commonmod.EnsureSystemSettings(ctx),
	}
}

// Route 路由配置。所有路由经 AuthMiddleware（个人维度，按 login uid 隔离），
// 并在其后挂 SharedUIDRateLimiter（每用户配额）。
func (s *Sticker) Route(r *wkhttp.WKHttp) {
	uidLimit := appwkhttp.SharedUIDRateLimiter(r, s.ctx)
	auth := r.Group("/v1/sticker", s.ctx.AuthMiddleware(r), uidLimit)
	{
		auth.GET("/user", s.list)
		auth.POST("/user", s.add)
		auth.DELETE("/user/:sticker_id", s.delete)
	}
}

// list 返回当前用户的自定义贴纸（扁平列表，最新在前）。空集合返回
// {"list":[]} 而非 404 —— 正是 issue #26 要消灭的噪音。
func (s *Sticker) list(ctx *wkhttp.Context) {
	loginUID := ctx.GetLoginUID()

	models, err := s.db.listByUID(loginUID)
	if err != nil {
		s.Error("查询贴纸失败", zap.Error(err))
		httperr.ResponseErrorL(ctx, errcode.ErrStickerQueryFailed, nil, nil)
		return
	}

	list := make([]stickerResp, 0, len(models))
	for _, m := range models {
		list = append(list, toStickerResp(m))
	}
	ctx.Response(listStickerResp{List: list})
}

// add 新增一张自定义贴纸。path 来自先前的 /v1/file/upload?type=sticker 上传，
// 服务端只登记元数据并做配额/格式校验，不接收文件本体。
func (s *Sticker) add(ctx *wkhttp.Context) {
	loginUID := ctx.GetLoginUID()

	var req addStickerReq
	if err := ctx.BindJSON(&req); err != nil {
		respondStickerRequestInvalid(ctx, "")
		return
	}
	if req.Path == "" {
		respondStickerRequestInvalid(ctx, "path")
		return
	}
	if len(req.Path) > maxStickerPathLen {
		respondStickerRequestInvalid(ctx, "path")
		return
	}
	format := normalizeStickerFormat(req.Format)
	if !isAllowedStickerFormat(format) {
		respondStickerFormatUnsupported(ctx, format)
		return
	}
	// path 必须指向「本人」走 type=sticker 强约束上传产生的对象
	// （sticker/{loginUID}/<name>.<ext>，且 ext == format）。否则客户端可上传
	// type=chat（100MB/宽松白名单）再把该 URL 注册成贴纸，绕过 1MB + 仅位图的
	// 上传契约；或注册他人/外部对象（PR#508 review）。
	if !validateStickerPath(req.Path, loginUID, format) {
		respondStickerRequestInvalid(ctx, "path")
		return
	}
	placeholder := req.Placeholder
	if placeholder == "" {
		placeholder = defaultStickerPlaceholder
	}
	if len([]rune(placeholder)) > maxStickerPlaceholderLen {
		respondStickerRequestInvalid(ctx, "placeholder")
		return
	}

	// 配额：管理端可配 system_setting sticker.user_max_count，默认 100。计数与插入
	// 放进同一事务，并先对「本人 user 行」加 FOR UPDATE 记录锁串行化同一用户的并发
	// 新增，消除 count→insert 的 TOCTOU —— 否则并发 POST 可同时通过校验、双双插入而
	// 超额。锁 user 行（唯一索引上的记录锁）而非对 sticker 子表 count(*) FOR UPDATE
	// （非唯一索引 → gap 锁，首插死锁），见 db.go lockUserRowTx 说明。
	max := s.settings.StickerUserMaxCount()

	tx, err := s.ctx.DB().Begin()
	if err != nil {
		s.Error("开启事务失败", zap.Error(err))
		httperr.ResponseErrorL(ctx, errcode.ErrStickerStoreFailed, nil, nil)
		return
	}
	defer tx.RollbackUnlessCommitted()

	if err := s.db.lockUserRowTx(tx, loginUID); err != nil {
		s.Error("锁定用户行失败", zap.Error(err))
		httperr.ResponseErrorL(ctx, errcode.ErrStickerStoreFailed, nil, nil)
		return
	}

	count, err := s.db.countByUIDTx(tx, loginUID)
	if err != nil {
		s.Error("查询贴纸数量失败", zap.Error(err))
		httperr.ResponseErrorL(ctx, errcode.ErrStickerQueryFailed, nil, nil)
		return
	}
	if count >= max {
		respondStickerQuotaExceeded(ctx, max)
		return
	}

	m := &StickerModel{
		StickerID:   util.GenerUUID(),
		UID:         loginUID,
		Path:        req.Path,
		Placeholder: placeholder,
		Format:      format,
		Status:      1,
	}
	if err := s.db.insertTx(tx, m); err != nil {
		s.Error("新增贴纸失败", zap.Error(err))
		httperr.ResponseErrorL(ctx, errcode.ErrStickerStoreFailed, nil, nil)
		return
	}
	if err := tx.Commit(); err != nil {
		s.Error("提交事务失败", zap.Error(err))
		httperr.ResponseErrorL(ctx, errcode.ErrStickerStoreFailed, nil, nil)
		return
	}

	ctx.Response(toStickerResp(m))
}

// delete 软删除当前用户名下的一张贴纸。删除他人贴纸或不存在的贴纸一律按
// "不存在" 处理（不暴露存在性，避免跨用户枚举）。
func (s *Sticker) delete(ctx *wkhttp.Context) {
	loginUID := ctx.GetLoginUID()
	stickerID := ctx.Param("sticker_id")

	m, err := s.db.queryByID(stickerID)
	if err != nil {
		s.Error("查询贴纸失败", zap.Error(err))
		httperr.ResponseErrorL(ctx, errcode.ErrStickerQueryFailed, nil, nil)
		return
	}
	if m == nil || m.UID != loginUID {
		httperr.ResponseErrorL(ctx, errcode.ErrStickerNotFound, nil, nil)
		return
	}

	if err := s.db.softDelete(stickerID, loginUID); err != nil {
		s.Error("删除贴纸失败", zap.Error(err))
		httperr.ResponseErrorL(ctx, errcode.ErrStickerStoreFailed, nil, nil)
		return
	}

	ctx.ResponseOK()
}
