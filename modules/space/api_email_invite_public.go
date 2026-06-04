package space

import (
	"errors"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	"go.uber.org/zap"
)

// emailInvitePage 返回邀请落地页 H5（公开，注入 API_BASE_URL）。
// JS 在浏览器里调用 previewEmailInvite / acceptEmailInvite 完成实际逻辑。
func (s *Space) emailInvitePage(c *wkhttp.Context) {
	htmlBytes, err := os.ReadFile("./assets/web/space_email_invite.html")
	if err != nil {
		s.Error("加载邮件邀请落地页失败", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrSpaceQueryFailed, nil, nil)
		return
	}
	safeBaseURL := strconv.Quote(s.ctx.GetConfig().External.BaseURL)
	html := strings.Replace(string(htmlBytes), `"{{API_BASE_URL}}"`, safeBaseURL, 1)
	// BaseURL 与部署强相关；邀请落地页本身也不应被搜索引擎索引或 CDN 缓存。
	c.Header("Cache-Control", "no-store")
	c.Header("X-Robots-Tag", "noindex, nofollow")
	// 落地页本身就以"读 sessionStorage 拿登录 token"为目的，一旦出现 XSS 就是
	// 会话凭证泄漏。CSP 把 fetch / script / style 全部钉到同源（inline 暂保留——
	// 服务端模板需要把 BaseURL 注入到 JS 字面量；后续可改 nonce-based 策略），
	// 阻断 token 外发到第三方域；frame-ancestors 'none' 防 clickjacking。
	c.Header("Content-Security-Policy",
		"default-src 'self'; "+
			"script-src 'self' 'unsafe-inline'; "+
			"style-src 'self' 'unsafe-inline'; "+
			"connect-src 'self'; "+
			"img-src 'self' data:; "+
			"base-uri 'self'; "+
			"form-action 'none'; "+
			"frame-ancestors 'none'")
	c.Data(http.StatusOK, "text/html; charset=utf-8", []byte(html))
}

// emailInvitePreviewResp 公开预览响应。owner 与 member 类型共用结构，按 invite_type 取不同字段：
//   - owner：planned_* 字段非空，space_id 为空
//   - member：space_id / space_name / space_logo / member_count 非空
type emailInvitePreviewResp struct {
	InviteType  int    `json:"invite_type"`
	Email       string `json:"email"`
	Status      int    `json:"status"`
	ExpiresAt   string `json:"expires_at,omitempty"`
	Role        int    `json:"role,omitempty"`
	InviterName string `json:"inviter_name,omitempty"`

	// owner-only
	PlannedName        string `json:"planned_name,omitempty"`
	PlannedDescription string `json:"planned_description,omitempty"`
	PlannedLogo        string `json:"planned_logo,omitempty"`
	PlannedMaxUsers    int    `json:"planned_max_users,omitempty"`
	PlannedJoinMode    int    `json:"planned_join_mode,omitempty"`

	// member-only
	SpaceId     string `json:"space_id,omitempty"`
	SpaceName   string `json:"space_name,omitempty"`
	SpaceLogo   string `json:"space_logo,omitempty"`
	MemberCount int    `json:"member_count,omitempty"`
	JoinMode    int    `json:"join_mode,omitempty"`
}

// previewEmailInvite 公开预览接口。响应总是 200，凡是无效（不存在/已过期/已消费/已撤销）
// 都通过 status 字段表达，避免给攻击者返回 4xx 做枚举区分。
func (s *Space) previewEmailInvite(c *wkhttp.Context) {
	rawToken := c.Param("token")
	if rawToken == "" {
		respondSpaceRequestInvalid(c, "token")
		return
	}
	inv, err := s.db.queryEmailInviteByTokenHash(hashEmailInviteToken(rawToken))
	if err != nil {
		s.Error("查询邮件邀请失败", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrSpaceQueryFailed, nil, nil)
		return
	}
	if inv == nil {
		c.JSON(http.StatusOK, &emailInvitePreviewResp{Status: EmailInviteStatusRevoked})
		return
	}

	resp := &emailInvitePreviewResp{
		InviteType: inv.InviteType,
		Email:      maskInviteEmail(inv.Email),
		Status:     liveStatus(inv),
		Role:       inv.Role,
	}
	if inv.ExpiresAt != nil {
		resp.ExpiresAt = inv.ExpiresAt.String()
	}
	if name, _ := s.db.queryUserName(inv.CreatedBy); name != "" {
		resp.InviterName = name
	}

	switch inv.InviteType {
	case EmailInviteTypeOwner:
		resp.PlannedName = inv.PlannedName
		resp.PlannedDescription = inv.PlannedDescription
		resp.PlannedLogo = inv.PlannedLogo
		resp.PlannedMaxUsers = inv.PlannedMaxUsers
		resp.PlannedJoinMode = inv.PlannedJoinMode
	case EmailInviteTypeMember:
		space, sErr := s.db.querySpaceByID(inv.SpaceId)
		if sErr != nil {
			s.Warn("预览时查询空间失败", zap.Error(sErr), zap.String("spaceId", inv.SpaceId))
		}
		if space != nil {
			resp.SpaceId = space.SpaceId
			resp.SpaceName = space.Name
			resp.SpaceLogo = space.Logo
			resp.JoinMode = space.JoinMode
			if cnt, cErr := s.db.queryActiveMemberCount(inv.SpaceId); cErr == nil {
				resp.MemberCount = cnt
			} else {
				s.Warn("预览时查询成员数失败", zap.Error(cErr), zap.String("spaceId", inv.SpaceId))
			}
		}
	}
	c.JSON(http.StatusOK, resp)
}

// acceptEmailInviteReq 接受邀请请求体。typed email 由前端要求用户重新输入，
// 用作"用户明确意愿确认"和"防御预览阶段邮箱被掩码后猜测"双重校验。
type acceptEmailInviteReq struct {
	Email string `json:"email"`
}

// acceptEmailInvite 已登录用户接受邮件邀请。
//   - owner：在同一事务内消费 token + createSpaceCoreTx，提交后跑 PostCommit 副作用。
//   - member：先消费 token，再 executeJoinSpace；失败则回滚 token 状态到 pending。
//
// 不受 DM_SPACE_DISABLE_USER_CREATE 全局开关影响（管理员邀请视为显式授权）。
//
// 邮箱校验三层（defense in depth）：
//  1. inv.Email 非空（拒绝历史脏数据）
//  2. 请求体 email == inv.email（用户主动输入，确认意愿；预览仅返回掩码邮箱，
//     无法照抄）
//  3. 登录账号 user.email == inv.email（攻击者就算从某处捡到 token，也得控制
//     正确邮箱对应的账号才能 accept）
func (s *Space) acceptEmailInvite(c *wkhttp.Context) {
	loginUID := c.GetLoginUID()
	rawToken := c.Param("token")
	if rawToken == "" {
		respondSpaceRequestInvalid(c, "token")
		return
	}

	var req acceptEmailInviteReq
	if err := c.BindJSON(&req); err != nil {
		respondSpaceRequestInvalid(c, "")
		return
	}
	typedEmail := strings.ToLower(strings.TrimSpace(req.Email))
	if typedEmail == "" {
		respondSpaceRequestInvalid(c, "email")
		return
	}

	inv, err := s.db.queryEmailInviteByTokenHash(hashEmailInviteToken(rawToken))
	if err != nil {
		s.Error("查询邮件邀请失败", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrSpaceQueryFailed, nil, nil)
		return
	}
	if inv == nil {
		httperr.ResponseErrorL(c, errcode.ErrSpaceEmailInviteInvalid, nil, nil)
		return
	}
	if inv.Status != EmailInviteStatusPending {
		httperr.ResponseErrorL(c, errcode.ErrSpaceEmailInviteProcessed, nil, nil)
		return
	}
	if inv.ExpiresAt != nil && time.Time(*inv.ExpiresAt).Before(time.Now()) {
		httperr.ResponseErrorL(c, errcode.ErrSpaceEmailInviteInvalid, nil, nil)
		return
	}

	if inv.Email == "" {
		// 历史脏数据兜底：邀请记录无邮箱时直接拒绝，避免空字符串自匹配。
		httperr.ResponseErrorL(c, errcode.ErrSpaceEmailInviteInvalid, nil, nil)
		return
	}
	if !strings.EqualFold(typedEmail, inv.Email) {
		httperr.ResponseErrorL(c, errcode.ErrSpaceEmailInviteEmailMismatch, nil, nil)
		return
	}
	loginEmail, err := s.db.queryUserEmail(loginUID)
	if err != nil {
		s.Error("查询登录用户邮箱失败", zap.Error(err), zap.String("loginUID", loginUID))
		httperr.ResponseErrorL(c, errcode.ErrSpaceQueryFailed, nil, nil)
		return
	}
	loginEmail = strings.TrimSpace(loginEmail)
	if loginEmail == "" || !strings.EqualFold(loginEmail, inv.Email) {
		httperr.ResponseErrorL(c, errcode.ErrSpaceEmailInviteEmailMismatch, nil, nil)
		return
	}

	switch inv.InviteType {
	case EmailInviteTypeOwner:
		s.acceptOwnerInvite(c, inv, loginUID)
	case EmailInviteTypeMember:
		s.acceptMemberInvite(c, inv, loginUID)
	default:
		httperr.ResponseErrorL(c, errcode.ErrSpaceEmailInviteInvalid, nil, nil)
	}
}

func (s *Space) acceptOwnerInvite(c *wkhttp.Context, inv *spaceEmailInviteModel, loginUID string) {
	tx, err := s.ctx.DB().Begin()
	if err != nil {
		s.Error("开启事务失败", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrSpaceStoreFailed, nil, nil)
		return
	}
	defer tx.RollbackUnlessCommitted()

	affected, err := s.db.consumeEmailInviteTx(tx, inv.Id, loginUID)
	if err != nil {
		s.Error("消费 token 失败", zap.Error(err), zap.Int64("inviteID", inv.Id))
		httperr.ResponseErrorL(c, errcode.ErrSpaceStoreFailed, nil, nil)
		return
	}
	if affected == 0 {
		httperr.ResponseErrorL(c, errcode.ErrSpaceEmailInviteProcessed, nil, nil)
		return
	}

	params := createSpaceParams{
		Creator:     loginUID,
		Name:        inv.PlannedName,
		Description: inv.PlannedDescription,
		Logo:        inv.PlannedLogo,
		JoinMode:    inv.PlannedJoinMode,
		MaxUsers:    inv.PlannedMaxUsers,
	}
	spaceId, err := s.createSpaceCoreTx(tx, params)
	if err != nil {
		s.Error("创建空间失败", zap.Error(err), zap.Int64("inviteID", inv.Id))
		httperr.ResponseErrorL(c, errcode.ErrSpaceStoreFailed, nil, nil)
		return
	}
	if err = tx.Commit(); err != nil {
		s.Error("提交事务失败", zap.Error(err), zap.Int64("inviteID", inv.Id))
		httperr.ResponseErrorL(c, errcode.ErrSpaceStoreFailed, nil, nil)
		return
	}

	result := s.createSpaceCorePostCommit(spaceId, params)
	c.Response(map[string]interface{}{
		"space_id":    result.SpaceID,
		"invite_code": result.InviteCode,
	})
}

func (s *Space) acceptMemberInvite(c *wkhttp.Context, inv *spaceEmailInviteModel, loginUID string) {
	space, err := s.db.querySpaceByID(inv.SpaceId)
	if err != nil {
		s.Error("查询空间失败", zap.Error(err), zap.String("spaceId", inv.SpaceId))
		httperr.ResponseErrorL(c, errcode.ErrSpaceQueryFailed, nil, nil)
		return
	}
	if space == nil || space.Status != SpaceStatusNormal {
		// 显式 defense-in-depth：即使 querySpaceByID 已过滤 status=1，未来若放宽条件也不会
		// 让 token 在已解散/封禁空间上被消耗。
		httperr.ResponseErrorL(c, errcode.ErrSpaceNotFound, nil, nil)
		return
	}

	// 1) 在独立事务内消费 token；2) 提交后调用 executeJoinSpace（其内部有自己的 FOR UPDATE 事务）。
	// 失败则把 token 状态回滚到 pending —— 这与 approveJoinApply 现有模式一致，避免嵌套事务。
	tx, err := s.ctx.DB().Begin()
	if err != nil {
		s.Error("开启事务失败", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrSpaceStoreFailed, nil, nil)
		return
	}
	defer tx.RollbackUnlessCommitted()

	affected, err := s.db.consumeEmailInviteTx(tx, inv.Id, loginUID)
	if err != nil {
		s.Error("消费 token 失败", zap.Error(err), zap.Int64("inviteID", inv.Id))
		httperr.ResponseErrorL(c, errcode.ErrSpaceStoreFailed, nil, nil)
		return
	}
	if affected == 0 {
		httperr.ResponseErrorL(c, errcode.ErrSpaceEmailInviteProcessed, nil, nil)
		return
	}
	if err = tx.Commit(); err != nil {
		s.Error("提交 token 消费失败", zap.Error(err), zap.Int64("inviteID", inv.Id))
		httperr.ResponseErrorL(c, errcode.ErrSpaceStoreFailed, nil, nil)
		return
	}

	if err := s.executeJoinSpace(loginUID, inv.SpaceId, space); err != nil {
		// 幂等：已是成员视为成功，保留 consumed 状态。仍需 fall-through 到下方角色处理逻辑——
		// 否则管理员对已有成员发 admin 邀请时，token 被消费但角色不会更新（PR #1172 review）。
		if errors.Is(err, ErrAlreadyMember) {
			s.Info("接受邀请时用户已是成员，保留 consumed 状态", zap.Int64("inviteID", inv.Id))
		} else {
			s.rollbackConsumedInvite(inv.Id, loginUID)
			if errors.Is(err, ErrSpaceFull) {
				httperr.ResponseErrorL(c, errcode.ErrSpaceFull, nil, nil)
				return
			}
			s.Error("加入空间失败", zap.Error(err), zap.String("spaceId", inv.SpaceId))
			httperr.ResponseErrorL(c, errcode.ErrSpaceStoreFailed, nil, nil)
			return
		}
	}

	// admin 邀请仅可升级；防止 owner（role=2）接受一条 role=admin 的邀请被降级为 1。
	if inv.Role == EmailInviteRoleAdmin {
		mem, mErr := s.db.queryMember(inv.SpaceId, loginUID)
		if mErr != nil {
			s.Warn("加入后查询成员失败，跳过角色提升", zap.Error(mErr),
				zap.String("spaceId", inv.SpaceId), zap.String("uid", loginUID))
		} else if mem != nil && mem.Role < 1 {
			if rErr := s.db.updateMemberRole(inv.SpaceId, loginUID, 1); rErr != nil {
				s.Warn("提升为管理员失败，成员关系仍生效", zap.Error(rErr),
					zap.String("spaceId", inv.SpaceId), zap.String("uid", loginUID))
			}
		}
	}

	c.Response(map[string]interface{}{"space_id": inv.SpaceId})
}

// rollbackConsumedInvite 调用 DB 层做最终一致性补偿；失败仅打告警日志，不上抛。
//
// 失败语义：以 alert=email_invite_rollback_failed 标记结构化字段，便于监控告警检索。
// 短期内 invite 会卡在 consumed 但用户未加入空间的状态，需要 ops 手工修复。Phase 6 后续
// 会用乐观锁/异步任务做自动补偿（参见 PR #1172 跟进项）。
func (s *Space) rollbackConsumedInvite(id int64, consumedBy string) {
	if err := s.db.rollbackConsumedEmailInvite(id, consumedBy); err != nil {
		s.Error("回滚邮件邀请状态失败，需要人工介入",
			zap.String("alert", "email_invite_rollback_failed"),
			zap.Error(err),
			zap.Int64("inviteID", id),
			zap.String("consumedBy", consumedBy),
		)
	}
}

// liveStatus 用过期时间动态推导邀请的展示状态：pending 且已过期则展示 expired。
func liveStatus(inv *spaceEmailInviteModel) int {
	if inv.Status == EmailInviteStatusPending && inv.ExpiresAt != nil &&
		time.Time(*inv.ExpiresAt).Before(time.Now()) {
		return EmailInviteStatusExpired
	}
	return inv.Status
}
