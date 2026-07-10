package doc_binding

// 4 endpoint 全部走 AuthMiddleware（由 Route 挂在 group 上），额外按 mount_type
// 做业务层权限判定：
//   - group 挂载：读 = 群成员(ExistMemberActive)；写 = 群主/群管理员(IsCreatorOrManager)。
//   - thread 挂载：读 = 父群成员；写 = binding 创建者 或 父群主/管理员。
//   - space 挂载：读 = space 活跃成员；写 = space creator 或 role>=1。
// 非成员一律 hidden-404（返回 ErrDocBindingNotFound）而不是 403，避免枚举 slug 猜挂载点。
// 权限不符但对方已经能看到该 slug（例如非 owner 的普通群员改 allow_share_code）时才 403，
// 这样才不会把"这个 slug 存在"当成信号泄给完全不相关的用户。

import (
	"regexp"
	"strings"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/modules/group"
	"github.com/Mininglamp-OSS/octo-server/modules/space"
	"github.com/Mininglamp-OSS/octo-server/modules/thread"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	"go.uber.org/zap"
)

// slug 白名单：小写字母/数字/短横/下划线，1..120 字符。收敛为 [a-z0-9_-] 是为了让 slug 能安全
// 出现在 URL path 与文件名（octo-docs-html 侧渲染），不允许大写/中文/空格/斜杠避免路径歧义。
var slugPattern = regexp.MustCompile(`^[a-z0-9_-]{1,120}$`)

// groupAuth 只暴露 handler 真正用到的两个群 ACL 谓词；用小接口而不是 group.IService 全量，
// 是为了让单测能只 stub 这两个方法，也让"我们依赖群的什么"一眼看清楚。
type groupAuth interface {
	ExistMemberActive(groupNo, uid string) (bool, error)
	IsCreatorOrManager(groupNo, uid string) (bool, error)
}

// spaceAuth 抽象两个 space 权限判定；实现在 New 里适配到 space.DB + 一次 space_member 直查。
type spaceAuth interface {
	IsMember(spaceId, uid string) (bool, error)
	MemberRole(spaceId, uid string) (role int, found bool, err error)
}

// threadAuth 只暴露一个校验：thread(short_id) 是不是真的挂在给定 group 下。
// 抽出来的原因是 create 路径必须先确认 (group_no, thread_id) 归属一致，防止
// caller 借"我是 A 群管"越权把 A 的 slug 绑到属于 B 群的 thread（数据串挂 +
// 之后所有读写都会按错的父群 ACL 走）。
type threadAuth interface {
	ExistInGroup(groupNo, threadId string) (bool, error)
}

// DocBinding 是 doc_binding module 的 HTTP handler。
type DocBinding struct {
	ctx        *config.Context
	db         store
	groupAuth  groupAuth
	spaceAuth  spaceAuth
	threadAuth threadAuth
	log.Log
}

// New 生成 DocBinding handler；被 1module.go 里的 SetupAPI 调用。
func New(ctx *config.Context) *DocBinding {
	return &DocBinding{
		ctx:        ctx,
		db:         newDB(ctx),
		groupAuth:  group.NewService(ctx),
		spaceAuth:  newRealSpaceAuth(ctx),
		threadAuth: newRealThreadAuth(ctx),
		Log:        log.NewTLog("DocBinding"),
	}
}

// realThreadAuth 落到 thread 模块的 ExistByGroupNoAndShortID：thread_id 即 short_id
// （thread.BuildChannelID(groupNo, shortID) 的 shortID），thread 表 UNIQUE(group_no, short_id)
// 一次索引命中即可判定归属。
type realThreadAuth struct {
	db *thread.DB
}

func newRealThreadAuth(ctx *config.Context) *realThreadAuth {
	return &realThreadAuth{db: thread.NewDB(ctx)}
}

func (r *realThreadAuth) ExistInGroup(groupNo, threadId string) (bool, error) {
	return r.db.ExistByGroupNoAndShortID(groupNo, threadId)
}

// realSpaceAuth 用 space.DB.IsMember + 直查 space_member.role 来落地 spaceAuth；
// 直查 role 是因为 space 包没公开"取角色"的 API，暂不想为此改 space 包。
type realSpaceAuth struct {
	ctx *config.Context
	db  *space.DB
}

func newRealSpaceAuth(ctx *config.Context) *realSpaceAuth {
	return &realSpaceAuth{ctx: ctx, db: space.NewDB(ctx)}
}

func (r *realSpaceAuth) IsMember(spaceId, uid string) (bool, error) {
	return r.db.IsMember(spaceId, uid)
}

func (r *realSpaceAuth) MemberRole(spaceId, uid string) (int, bool, error) {
	var role int
	found, err := r.ctx.DB().SelectBySql(
		"SELECT role FROM space_member WHERE space_id=? AND uid=? AND status=1",
		spaceId, uid).Load(&role)
	if err != nil {
		return 0, false, err
	}
	return role, found > 0, nil
}

// Route 挂 4 个 endpoint 到 /v1/docs/bindings*，全部走 AuthMiddleware。
func (h *DocBinding) Route(r *wkhttp.WKHttp) {
	auth := r.Group("/v1/docs", h.ctx.AuthMiddleware(r))
	{
		auth.POST("/bindings", h.create)
		auth.GET("/bindings/:slug", h.get)
		auth.PUT("/bindings/:slug", h.update)
		auth.DELETE("/bindings/:slug", h.delete)
	}
}

// ---- request/response DTO -------------------------------------------------

type createReq struct {
	Slug      string `json:"slug"`
	MountType string `json:"mount_type"`
	GroupNo   string `json:"group_no,omitempty"`
	ThreadId  string `json:"thread_id,omitempty"`
	SpaceId   string `json:"space_id,omitempty"`
	// 指针区分 "未传" 与 "显式 false"；未传时按默认 false（不开分享码）落库。
	AllowShareCode *bool `json:"allow_share_code,omitempty"`
}

type updateReq struct {
	AllowShareCode *bool `json:"allow_share_code,omitempty"`
}

type bindingResp struct {
	Slug           string `json:"slug"`
	MountType      string `json:"mount_type"`
	GroupNo        string `json:"group_no,omitempty"`
	ThreadId       string `json:"thread_id,omitempty"`
	SpaceId        string `json:"space_id,omitempty"`
	CreatorUID     string `json:"creator_uid"`
	AllowShareCode bool   `json:"allow_share_code"`
}

func toResp(m *Model) *bindingResp {
	return &bindingResp{
		Slug:           m.Slug,
		MountType:      m.MountType,
		GroupNo:        m.GroupNo,
		ThreadId:       m.ThreadId,
		SpaceId:        m.SpaceId,
		CreatorUID:     m.CreatorUID,
		AllowShareCode: m.AllowShareCode == AllowShareCodeOn,
	}
}

// ---- handler ---------------------------------------------------------------

func (h *DocBinding) create(c *wkhttp.Context) {
	loginUID := c.GetLoginUID()

	var req createReq
	if err := c.BindJSON(&req); err != nil {
		httperr.ResponseErrorL(c, errcode.ErrDocBindingRequestInvalid, nil, nil)
		return
	}
	req.Slug = strings.TrimSpace(req.Slug)
	if !slugPattern.MatchString(req.Slug) {
		httperr.ResponseErrorL(c, errcode.ErrDocBindingRequestInvalid, nil, map[string]interface{}{"field": "slug"})
		return
	}

	// 允许的挂载类型 + 对应字段互斥校验：group/thread 需 group_no（thread 再要 thread_id），space 需 space_id。
	switch req.MountType {
	case MountTypeGroup:
		if req.GroupNo == "" || req.ThreadId != "" || req.SpaceId != "" {
			httperr.ResponseErrorL(c, errcode.ErrDocBindingRequestInvalid, nil, map[string]interface{}{"field": "group_no"})
			return
		}
	case MountTypeThread:
		if req.GroupNo == "" || req.ThreadId == "" || req.SpaceId != "" {
			httperr.ResponseErrorL(c, errcode.ErrDocBindingRequestInvalid, nil, map[string]interface{}{"field": "thread_id"})
			return
		}
	case MountTypeSpace:
		if req.SpaceId == "" || req.GroupNo != "" || req.ThreadId != "" {
			httperr.ResponseErrorL(c, errcode.ErrDocBindingRequestInvalid, nil, map[string]interface{}{"field": "space_id"})
			return
		}
	default:
		httperr.ResponseErrorL(c, errcode.ErrDocBindingRequestInvalid, nil, map[string]interface{}{"field": "mount_type"})
		return
	}

	// thread 挂载额外硬校验：thread_id 必须真的挂在 req.GroupNo 下。仅靠 canWrite（父群管理员）
	// 不够——A 群管理员可能拿到 B 群的 thread_id 就想把 slug 绑过去，如果不校验归属，
	// binding 会落成 (group_no=A, thread_id=belongs_to_B)，之后按 GroupNo=A 判 ACL 就是错的。
	if req.MountType == MountTypeThread {
		if ok, err := h.threadAuth.ExistInGroup(req.GroupNo, req.ThreadId); err != nil {
			h.Error("校验 thread 归属失败", zap.Error(err))
			httperr.ResponseErrorL(c, errcode.ErrDocBindingStoreFailed, nil, nil)
			return
		} else if !ok {
			// 归属不上的 thread_id：按 field 报 400，不是 hidden-404——POST 场景 caller 自报的
			// 是无效引用，不涉及"这个 thread 是否存在"的信息泄漏（他本来就没资格查 B 群的 thread）。
			httperr.ResponseErrorL(c, errcode.ErrDocBindingRequestInvalid, nil, map[string]interface{}{"field": "thread_id"})
			return
		}
	}

	// 写权限：按挂载类型判定；不通过一律 403（因为 caller 是主动 POST，泄不泄漏这个挂载点存在都不重要）。
	if ok, err := h.canWrite(loginUID, req.MountType, req.GroupNo, req.ThreadId, req.SpaceId, ""); err != nil {
		h.Error("检查建绑权限失败", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrDocBindingStoreFailed, nil, nil)
		return
	} else if !ok {
		httperr.ResponseErrorL(c, errcode.ErrDocBindingForbidden, nil, nil)
		return
	}

	// slug 冲突：靠 UNIQUE INDEX 兜底，先查是加一次友好返回；并发下漏查后仍会 InsertInto 报 dup，翻成 409。
	if existing, err := h.db.queryBySlug(req.Slug); err != nil {
		h.Error("查询 slug 失败", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrDocBindingStoreFailed, nil, nil)
		return
	} else if existing != nil {
		httperr.ResponseErrorL(c, errcode.ErrDocBindingSlugConflict, nil, map[string]interface{}{"field": "slug"})
		return
	}

	allow := AllowShareCodeOff
	if req.AllowShareCode != nil && *req.AllowShareCode {
		allow = AllowShareCodeOn
	}

	m := &Model{
		Slug:           req.Slug,
		MountType:      req.MountType,
		GroupNo:        req.GroupNo,
		ThreadId:       req.ThreadId,
		SpaceId:        req.SpaceId,
		CreatorUID:     loginUID,
		AllowShareCode: allow,
	}
	if err := h.db.insert(m); err != nil {
		// 并发下依赖 UNIQUE(slug) 兜底：MySQL dup key 一律翻成 409，避免调用方以为写成功。
		if isDupSlugErr(err) {
			httperr.ResponseErrorL(c, errcode.ErrDocBindingSlugConflict, nil, map[string]interface{}{"field": "slug"})
			return
		}
		h.Error("新建 doc_binding 失败", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrDocBindingStoreFailed, nil, nil)
		return
	}

	c.Response(toResp(m))
}

func (h *DocBinding) get(c *wkhttp.Context) {
	loginUID := c.GetLoginUID()
	slug := c.Param("slug")
	if !slugPattern.MatchString(slug) {
		httperr.ResponseErrorL(c, errcode.ErrDocBindingNotFound, nil, nil)
		return
	}

	m, err := h.db.queryBySlug(slug)
	if err != nil {
		h.Error("查询 doc_binding 失败", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrDocBindingStoreFailed, nil, nil)
		return
	}
	if m == nil {
		httperr.ResponseErrorL(c, errcode.ErrDocBindingNotFound, nil, nil)
		return
	}

	ok, err := h.canRead(loginUID, m)
	if err != nil {
		h.Error("检查读权限失败", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrDocBindingStoreFailed, nil, nil)
		return
	}
	if !ok {
		// hidden-404：不给非成员"这个 slug 存在"的信号，防枚举探测挂载点。
		httperr.ResponseErrorL(c, errcode.ErrDocBindingNotFound, nil, nil)
		return
	}

	c.Response(toResp(m))
}

func (h *DocBinding) update(c *wkhttp.Context) {
	loginUID := c.GetLoginUID()
	slug := c.Param("slug")
	if !slugPattern.MatchString(slug) {
		httperr.ResponseErrorL(c, errcode.ErrDocBindingNotFound, nil, nil)
		return
	}

	var req updateReq
	if err := c.BindJSON(&req); err != nil {
		httperr.ResponseErrorL(c, errcode.ErrDocBindingRequestInvalid, nil, nil)
		return
	}
	// 目前只支持 allow_share_code；改挂载点必须删了重建，避免历史 slug 权限漂移。
	if req.AllowShareCode == nil {
		httperr.ResponseErrorL(c, errcode.ErrDocBindingRequestInvalid, nil, map[string]interface{}{"field": "allow_share_code"})
		return
	}

	m, err := h.db.queryBySlug(slug)
	if err != nil {
		h.Error("查询 doc_binding 失败", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrDocBindingStoreFailed, nil, nil)
		return
	}
	if m == nil {
		httperr.ResponseErrorL(c, errcode.ErrDocBindingNotFound, nil, nil)
		return
	}

	// 先读权限：非成员/跨 space 直接 hidden-404；成员再判写权限（差别里才 403）。
	if ok, rErr := h.canRead(loginUID, m); rErr != nil {
		h.Error("检查读权限失败", zap.Error(rErr))
		httperr.ResponseErrorL(c, errcode.ErrDocBindingStoreFailed, nil, nil)
		return
	} else if !ok {
		httperr.ResponseErrorL(c, errcode.ErrDocBindingNotFound, nil, nil)
		return
	}
	if ok, wErr := h.canWrite(loginUID, m.MountType, m.GroupNo, m.ThreadId, m.SpaceId, m.CreatorUID); wErr != nil {
		h.Error("检查写权限失败", zap.Error(wErr))
		httperr.ResponseErrorL(c, errcode.ErrDocBindingStoreFailed, nil, nil)
		return
	} else if !ok {
		httperr.ResponseErrorL(c, errcode.ErrDocBindingForbidden, nil, nil)
		return
	}

	allow := AllowShareCodeOff
	if *req.AllowShareCode {
		allow = AllowShareCodeOn
	}
	if err := h.db.updateAllowShareCode(slug, allow); err != nil {
		h.Error("更新 allow_share_code 失败", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrDocBindingStoreFailed, nil, nil)
		return
	}
	m.AllowShareCode = allow
	c.Response(toResp(m))
}

func (h *DocBinding) delete(c *wkhttp.Context) {
	loginUID := c.GetLoginUID()
	slug := c.Param("slug")
	if !slugPattern.MatchString(slug) {
		httperr.ResponseErrorL(c, errcode.ErrDocBindingNotFound, nil, nil)
		return
	}

	m, err := h.db.queryBySlug(slug)
	if err != nil {
		h.Error("查询 doc_binding 失败", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrDocBindingStoreFailed, nil, nil)
		return
	}
	if m == nil {
		httperr.ResponseErrorL(c, errcode.ErrDocBindingNotFound, nil, nil)
		return
	}

	if ok, rErr := h.canRead(loginUID, m); rErr != nil {
		h.Error("检查读权限失败", zap.Error(rErr))
		httperr.ResponseErrorL(c, errcode.ErrDocBindingStoreFailed, nil, nil)
		return
	} else if !ok {
		httperr.ResponseErrorL(c, errcode.ErrDocBindingNotFound, nil, nil)
		return
	}
	if ok, wErr := h.canWrite(loginUID, m.MountType, m.GroupNo, m.ThreadId, m.SpaceId, m.CreatorUID); wErr != nil {
		h.Error("检查写权限失败", zap.Error(wErr))
		httperr.ResponseErrorL(c, errcode.ErrDocBindingStoreFailed, nil, nil)
		return
	} else if !ok {
		httperr.ResponseErrorL(c, errcode.ErrDocBindingForbidden, nil, nil)
		return
	}

	if err := h.db.deleteBySlug(slug); err != nil {
		h.Error("删除 doc_binding 失败", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrDocBindingStoreFailed, nil, nil)
		return
	}
	c.ResponseOK()
}

// ---- 权限判定 --------------------------------------------------------------

// canRead / canWrite 独立函数是为了让写路径三个 handler（create/update/delete）复用同一套判定，
// 顺便让它们的单元测试可 stub group/space/thread service 表面。
func (h *DocBinding) canRead(uid string, m *Model) (bool, error) {
	switch m.MountType {
	case MountTypeGroup:
		return h.groupAuth.ExistMemberActive(m.GroupNo, uid)
	case MountTypeThread:
		// thread 挂载的读权限 = 父群成员（thread 自己没有独立成员概念，共父群 ACL）。
		return h.groupAuth.ExistMemberActive(m.GroupNo, uid)
	case MountTypeSpace:
		return h.spaceAuth.IsMember(m.SpaceId, uid)
	}
	return false, nil
}

// canWrite: mount=group/thread 走群 owner/manager；mount=space 走 creator 或 role>=1；
// create 阶段调用时 bindingCreator 传 ""，只按挂载点判；update/delete 时把 binding.creator_uid
// 传进来允许原创建者对自己建的 thread binding 拥有写权限（与父单 §5.2 一致）。
func (h *DocBinding) canWrite(uid, mountType, groupNo, threadId, spaceId, bindingCreator string) (bool, error) {
	switch mountType {
	case MountTypeGroup:
		return h.groupAuth.IsCreatorOrManager(groupNo, uid)
	case MountTypeThread:
		// binding creator 自己可写；否则回落到群主/管理员。
		if bindingCreator != "" && bindingCreator == uid {
			// 但仍需确认 caller 现在还是父群成员，避免离群后残留写权限。
			still, err := h.groupAuth.ExistMemberActive(groupNo, uid)
			if err != nil || !still {
				return false, err
			}
			return true, nil
		}
		return h.groupAuth.IsCreatorOrManager(groupNo, uid)
	case MountTypeSpace:
		// Space 写权限：role>=1 (1=管理员, 2=拥有者)；non-member ⇒ false, 顺势也当作"看不到"。
		role, found, err := h.spaceAuth.MemberRole(spaceId, uid)
		if err != nil || !found {
			return false, err
		}
		return role >= 1, nil
	}
	return false, nil
}

// isDupSlugErr 判定是不是 UNIQUE(slug) 冲突；用字符串匹配是因为 gocraft/dbr 不吐 mysql 错误码结构体。
// MySQL 报文形如 "Error 1062: Duplicate entry 'xxx' for key 'doc_binding_slug'"。
func isDupSlugErr(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "Duplicate entry") && strings.Contains(s, "doc_binding_slug")
}
