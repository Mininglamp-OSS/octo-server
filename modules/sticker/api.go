// Package sticker implements per-user custom sticker management.
//
// # Two-step registration contract (API)
//
//  1. Upload the image: POST /v1/file/upload?type=sticker (multipart). The
//     response carries `path` and — when the server has signing capability
//     (OCTO_MASTER_KEY configured) — `sticker_handle`.
//  2. Register: POST /v1/sticker/user with body {path, format, placeholder,
//     handle}, passing the upload's `sticker_handle` value as `handle`.
//
// Stickers do NOT support presigned uploads: the upload handle can only be
// minted where modules/file holds both the authenticated uploader and the
// content-validated bytes, so the image must transit the multipart endpoint.
//
// # Handle enforcement (capability vs policy)
//
// Whether `handle` is REQUIRED is governed by the system_setting
// `sticker.handle_required` (SystemSettings.StickerHandleRequired) — a DB-backed,
// admin-toggleable policy independent of the signing capability (OCTO_MASTER_KEY
// / stickersig.Enabled). It lives in system_setting rather than an env var so the
// rollout can be toggled and rolled back from the admin console without a
// restart (converging across replicas within the snapshot TTL). Clients read the
// effective policy from GET /v1/common/appconfig → `sticker_handle_required`. See
// the stickersig package doc for the rollout rationale and behavior matrix.
package sticker

import (
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	commonmod "github.com/Mininglamp-OSS/octo-server/modules/common"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	"github.com/Mininglamp-OSS/octo-server/pkg/metrics"
	"github.com/Mininglamp-OSS/octo-server/pkg/stickersig"
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
	s := &Sticker{
		ctx:      ctx,
		Log:      log.NewTLog("Sticker"),
		db:       newStickerDB(ctx),
		settings: commonmod.EnsureSystemSettings(ctx),
	}
	// 运营可见性：签发/校验 handle 的「能力」由 OCTO_MASTER_KEY 决定（stickersig.Enabled，
	// 部署级 env），「是否强制客户端必须带 handle」的「策略」由 system_setting
	// sticker.handle_required 决定（s.settings.StickerHandleRequired，运营可热切的 DB 真源），
	// 两者正交、互不派生、连配置载体都不同（详见 stickersig 包 doc）。把两类降级/冲突姿态
	// 在启动时打日志并落到 handle_policy gauge，使运营对部署可见。一次性、进程级；策略为
	// DB 运行时可变，故 gauge 的 required 维度是「启动快照」，实时值以 appconfig 为准。
	required := s.settings.StickerHandleRequired()
	metrics.SetStickerHandlePolicy(stickersig.Enabled(), required)
	switch {
	case required && !stickersig.Enabled():
		// 配置冲突：策略声明要求 handle，却没有有效 OCTO_MASTER_KEY 提供签名/校验能力。
		// 不 panic（不让贴纸策略误配拖垮整个服务启动、也不与 master key 生命周期强行
		// 关联）；改打 ERROR 强告警 + handle_policy gauge 暴露之。运行时 add() 对该冲突
		// fail-closed：拒绝新增贴纸并记 rejected_no_capability（见 add()），既不静默放行也
		// 不拖垮服务——靠本告警 + 指标驱动运营尽快修复（补 32 字节 OCTO_MASTER_KEY）。
		s.Error("配置冲突：system_setting sticker.handle_required=true 但 OCTO_MASTER_KEY 未配置或非恰好 32 字节，" +
			"无有效签名能力，贴纸新增将 fail-closed 拒绝；请配置 32 字节 OCTO_MASTER_KEY 或关闭 handle_required")
	case !stickersig.Enabled():
		s.Warn("OCTO_MASTER_KEY 未配置或非恰好 32 字节：自定义贴纸上传句柄校验已禁用，" +
			"注册退化为仅路径形状校验（配置 32 字节 OCTO_MASTER_KEY 可启用密码学来源绑定）")
	}
	return s
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
	// path 必须指向「本人」走 type=sticker 强约束上传产生的对象。两层防护：
	//  (1) 路径形状校验 validateStickerPath（始终执行）——挡住他人 uid 段 / 非
	//      sticker 桶 / 缺扩展名 / ext≠format 等明显非法路径；
	//  (2) 上传句柄 stickersig.Verify（配置了 OCTO_MASTER_KEY 时附加）——密码学证明
	//      Path 确由本人经 type=sticker 上传门（1MB + 魔数 + 仅位图）产生。这层封死
	//      (1) 的尾匹配残留：形如 "chat/sticker/{uid}/x.gif" 能过形状校验，但客户端
	//      无法为未经贴纸上传的对象伪造句柄，故被拒——堵住「以 type=chat(100MB/宽松
	//      白名单)上传再注册成贴纸」的旁路。
	//
	// 是否「强制」带 handle 由 system_setting sticker.handle_required 策略决定（与 master
	// key 能力解耦）：required=false 兼容期内缺 handle 暂放行（记 compat_missing 指标 + INFO
	// 日志，供灰度观察老客户端缺失率），此时 (2) 的强防护降级为仅 (1)；required=true 时
	// 缺/非法 handle 一律拒。非法 handle 无论策略恒拒。各拒因都收敛到同一 request_invalid，
	// 不暴露具体原因（anti-enumeration），原因只进指标/日志。
	//
	// 配置冲突 fail-closed：策略要求 handle（required=true）但服务端无有效 OCTO_MASTER_KEY
	// 提供校验能力（!Enabled）——此时既无法验签，策略又要求必须验，唯一正确姿态是拒绝
	// 而非静默放行（放行会让 appconfig/dashboard 声称已强制、实则每个缺/伪造 handle 都放过，
	// 是最坏的静默安全洞）。故在分类前显式拦截：拒绝该次注册并记 rejected_no_capability。
	// 这只拒「新增贴纸」这一个操作，不 panic、不拖垮服务启动（启动另有 ERROR 强告警），
	// 是安全控制误配应有的响亮失败——运营补上 32 字节 OCTO_MASTER_KEY 即恢复。
	// 注意 required=false（兼容/裸跑）时 !Enabled 仍走下方 classify 的路径形状放行（不回归）。
	//
	// 请求内一致性：required（策略，system_setting 60s 热重载）与 enabled（能力，env）各在
	// 本请求内快照一次并贯穿始终，避免 add() 与 classifyStickerPath 多点各读一次、被中途快照
	// 刷新割裂（TOCTOU）。两向都安全（fail-closed↔compat-allow，绝不 fail-open），此处显式化
	// 以免疫将来可能引入的运行时 env-reload。
	required := s.settings.StickerHandleRequired()
	enabled := stickersig.Enabled()
	if required && !enabled {
		metrics.ObserveStickerRegister(metrics.StickerRegisterRejectedNoCapability)
		s.Error("拒绝注册：sticker.handle_required=true 但 OCTO_MASTER_KEY 未配置或非恰好 32 字节，"+
			"无签名能力无法校验 handle，fail-closed 拒绝；请配置 32 字节 OCTO_MASTER_KEY 或关闭 handle_required",
			zap.String("uid", loginUID))
		respondStickerRequestInvalid(ctx, "path")
		return
	}
	switch classifyStickerPath(req.Path, loginUID, format, req.Handle, enabled) {
	case stickerPathInvalid:
		metrics.ObserveStickerRegister(metrics.StickerRegisterRejectedPath)
		respondStickerRequestInvalid(ctx, "path")
		return
	case stickerHandleInvalid:
		metrics.ObserveStickerRegister(metrics.StickerRegisterRejectedInvalid)
		s.Warn("拒绝注册：贴纸 handle 非法或与 path 不匹配", zap.String("uid", loginUID))
		respondStickerRequestInvalid(ctx, "path")
		return
	case stickerHandleMissing:
		if required {
			metrics.ObserveStickerRegister(metrics.StickerRegisterRejectedMissing)
			s.Warn("拒绝注册：强制模式下缺少贴纸 handle", zap.String("uid", loginUID))
			respondStickerRequestInvalid(ctx, "path")
			return
		}
		// 兼容模式（sticker.handle_required=false）：放行但记录，供切强制前观察老客户端缺失率归零。
		metrics.ObserveStickerRegister(metrics.StickerRegisterCompatMissing)
		s.Info("兼容模式放行：注册缺少贴纸 handle（待 system_setting sticker.handle_required 切 true 后将被拒）",
			zap.String("uid", loginUID))
	case stickerOK:
		metrics.ObserveStickerRegister(metrics.StickerRegisterOK)
	default:
		// fail-closed：classifyStickerPath 返回了未识别的分类（理论上不可达——若将来
		// 给 stickerPathClass 加分类却漏处理，宁可拒绝并告警，也不默认放行落库。安全门控
		// 不允许 fail-open）。
		metrics.ObserveStickerRegister(metrics.StickerRegisterRejectedPath)
		s.Error("拒绝注册：未识别的 path 分类（疑似漏处理的 stickerPathClass）", zap.String("uid", loginUID))
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

// stickerPathClass is the outcome of authorizing a client-supplied sticker
// object path for registration. It separates the path-shape verdict from the
// handle verdict so add() can apply the enforcement policy (required vs compat)
// and emit a precise metric per outcome.
type stickerPathClass int

const (
	// stickerOK: path shape passed AND (when signing is enabled) a valid handle
	// was supplied — or signing is disabled and the shape passed.
	stickerOK stickerPathClass = iota
	// stickerPathInvalid: the path-shape check failed (wrong uid segment / not a
	// sticker key / missing or mismatched extension). Always rejected.
	stickerPathInvalid
	// stickerHandleMissing: signing is enabled, the shape passed, but no handle
	// was supplied. add() rejects under required=true, allows (recorded) otherwise.
	stickerHandleMissing
	// stickerHandleInvalid: signing is enabled, the shape passed, a handle was
	// supplied but it does not verify (forged / minted for another object).
	// Always rejected regardless of the required policy.
	stickerHandleInvalid
)

// classifyStickerPath authorizes a client-supplied sticker object path for
// registration. It always applies the path-shape check (validateStickerPath);
// when upload-handle signing is active (OCTO_MASTER_KEY configured) it
// classifies the handle as OK / missing / invalid. When no master key is
// configured it cannot verify a handle and returns stickerOK on the shape check
// alone (matching the pre-handle posture) — this branch is reached ONLY in the
// compat case (handle_required=false); the caller (add) intercepts the
// required-but-no-capability conflict BEFORE calling this and fails closed, so a
// missing capability never silently allows an enforced registration. The
// enforcement decision (reject vs compat-allow on a missing handle) is made by
// the caller from SystemSettings.StickerHandleRequired(). `enabled` is the
// caller's per-request snapshot of stickersig.Enabled() (threaded in for
// request-scoped consistency); Verify still reads the master key internally, but
// it is only reached when enabled is true, so the key is present.
func classifyStickerPath(path, loginUID, format, handle string, enabled bool) stickerPathClass {
	if !validateStickerPath(path, loginUID, format) {
		return stickerPathInvalid
	}
	if !enabled {
		return stickerOK
	}
	if handle == "" {
		return stickerHandleMissing
	}
	if !stickersig.Verify(loginUID, path, handle) {
		return stickerHandleInvalid
	}
	return stickerOK
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
