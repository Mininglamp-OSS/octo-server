package space

import (
	"context"
	"sync"
	"time"

	commonapi "github.com/Mininglamp-OSS/octo-server/modules/base/common"
	"github.com/Mininglamp-OSS/octo-server/modules/base/common/emailtmpl"
	common "github.com/Mininglamp-OSS/octo-server/modules/common"
	octoi18n "github.com/Mininglamp-OSS/octo-server/pkg/i18n"
	"go.uber.org/zap"
)

// inviteEmailSendTimeout 异步发送的硬上限。SMTP 层已有自己的 dial / IO 超时，
// 这里再叠一层是为了避免任何下游 hang 让 goroutine 不可回收。
const inviteEmailSendTimeout = 30 * time.Second

// inviteEmailSender 抽象掉具体的 SMTP 发送实现，便于在测试中替换为 recorder。
// 故意保持 1 个方法 —— 发邮件的所有上层细节（subject/html/plain 构造、链接拼接）
// 都在 dispatchInviteEmail 中完成。
//
// 走 SendTransactionalHTML（而非 SendHTMLEmail）：邀请是高价值事务邮件，带
// plaintext 兜底 + 标准事务邮件 header 可显著降低被反垃圾静默丢弃的概率。
type inviteEmailSender interface {
	SendTransactionalHTML(ctx context.Context, to, subject, htmlBody, plainBody string) error
}

// inviteEmailSenderOverride 测试钩子：非 nil 时优先于默认 EmailService。
// 使用包级变量是因为 testutil.NewTestServer() 在 TestMain 阶段就构造好 Space 单例，
// 测试用例无法在构造期注入；只有 override 能覆盖该单例。
var (
	inviteEmailSenderOverride   inviteEmailSender
	inviteEmailSenderOverrideMu sync.RWMutex
)

// SetInviteEmailSenderForTest 测试专用：替换全局发送器。production 代码不要调用。
func SetInviteEmailSenderForTest(s inviteEmailSender) {
	inviteEmailSenderOverrideMu.Lock()
	defer inviteEmailSenderOverrideMu.Unlock()
	inviteEmailSenderOverride = s
}

// getInviteEmailSenderForTest 测试专用：取当前 override，便于 t.Cleanup 还原。
func getInviteEmailSenderForTest() inviteEmailSender {
	inviteEmailSenderOverrideMu.RLock()
	defer inviteEmailSenderOverrideMu.RUnlock()
	return inviteEmailSenderOverride
}

// currentInviteSender 优先返回 override；否则按需构造 EmailService。
// 每次 New 都构造 EmailService 也行，但这里保持 lazy + override 优先。
func (s *Space) currentInviteSender() inviteEmailSender {
	if v := getInviteEmailSenderForTest(); v != nil {
		return v
	}
	return commonapi.NewEmailService(s.ctx, common.EnsureSystemSettings(s.ctx))
}

// dispatchInviteEmail 同步发送一封 owner/member 邀请邮件。
// 失败仅打 alert 日志：邀请记录已落库，调用方通过 list 接口仍能看到该邀请，
// 后续支持手工重发；让创建接口失败会留下"DB 有 invite 但用户没收到邮件"的
// 不一致状态。
//
// 调用方约定：在创建端点的 DB 写入成功后用 `go` 异步触发，避免阻塞 API。
func (s *Space) dispatchInviteEmail(inv *spaceEmailInviteModel, rawToken string) {
	if inv == nil || inv.Email == "" || rawToken == "" {
		return
	}

	// ctx 提前到入口：即便 DB 查询本身不接 ctx（dbr 同步调用），也能让 SendHTMLEmail
	// 在 DB hang 后立即超时退出，整个 dispatch 真正在 inviteEmailSendTimeout 内收敛。
	ctx, cancel := context.WithTimeout(context.Background(), inviteEmailSendTimeout)
	defer cancel()

	// 收件人语言：邀请是异步发送、收件人未必是已注册用户（无 uid）、且 ≠ 请求
	// 发起人，因此本期（issue #221 决策 A）统一渲染为 OCTO_DEFAULT_LANGUAGE。
	// OutboundLanguage 在脱离请求的 background ctx 下自然退回默认语言；lang 参数
	// 贯穿渲染与链接，将来接入 ResolveUserLanguage(recipient) 即在此处替换。
	lang := octoi18n.OutboundLanguage(ctx)

	// 落地页由后端在 BaseURL/v1/space/email-invite 提供（emailInvitePage handler）；
	// H5BaseURL 在多数部署里是独立 H5 服务，不会响应该路径。这里必须用 BaseURL，
	// 否则邮件链接会 404。修正于 PR #1194 review (W1)。
	acceptURL := emailInviteAcceptURL(s.ctx.GetConfig().External.BaseURL, rawToken, lang)
	if acceptURL == "" {
		s.Warn("跳过邀请邮件：未配置 External.BaseURL",
			zap.String("alert", "email_invite_send_skipped_no_base"),
			zap.Int64("inviteID", inv.Id))
		return
	}

	inviterName, qErr := s.db.queryUserName(inv.CreatedBy)
	if qErr != nil {
		// 不阻塞发送：拿不到名字就走匿名兜底文案。但要记日志便于排障。
		s.Warn("查询邀请人名失败，使用兜底文案",
			zap.Error(qErr), zap.String("createdBy", inv.CreatedBy))
		inviterName = ""
	}

	var (
		rendered emailtmpl.Rendered
		bErr     error
	)
	switch inv.InviteType {
	case EmailInviteTypeOwner:
		rendered, bErr = buildOwnerInviteEmail(inv, inviterName, lang, acceptURL)
	case EmailInviteTypeMember:
		spaceName := ""
		sp, sErr := s.db.querySpaceByID(inv.SpaceId)
		if sErr != nil {
			s.Warn("查询空间名失败，邮件正文将不显示空间名",
				zap.Error(sErr), zap.String("spaceId", inv.SpaceId))
		} else if sp != nil {
			spaceName = sp.Name
		}
		rendered, bErr = buildMemberInviteEmail(inv, inviterName, spaceName, lang, acceptURL)
	default:
		s.Warn("未知 invite_type，跳过邮件发送", zap.Int("inviteType", inv.InviteType))
		return
	}
	if bErr != nil {
		s.Error("构造邀请邮件失败",
			zap.String("alert", "email_invite_render_failed"),
			zap.Error(bErr), zap.Int64("inviteID", inv.Id))
		return
	}

	if err := s.currentInviteSender().SendTransactionalHTML(
		ctx, inv.Email, rendered.Subject, rendered.HTML, rendered.Text,
	); err != nil {
		s.Error("发送邀请邮件失败",
			zap.String("alert", "email_invite_send_failed"),
			zap.Error(err),
			zap.Int64("inviteID", inv.Id),
			zap.String("to", inv.Email))
	}
}
