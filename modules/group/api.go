package group

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/model"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/register"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkevent"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/modules/base/event"
	chservice "github.com/Mininglamp-OSS/octo-server/modules/channel/service"
	common2 "github.com/Mininglamp-OSS/octo-server/modules/common"
	"github.com/Mininglamp-OSS/octo-server/modules/conversation_ext"
	"github.com/Mininglamp-OSS/octo-server/modules/file"
	"github.com/Mininglamp-OSS/octo-server/modules/source"
	spacemod "github.com/Mininglamp-OSS/octo-server/modules/space"
	"github.com/Mininglamp-OSS/octo-server/modules/user"
	"github.com/Mininglamp-OSS/octo-server/pkg/auth"
	"github.com/Mininglamp-OSS/octo-server/pkg/avatarrender"
	"github.com/Mininglamp-OSS/octo-server/pkg/avatarversion"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	octoredis "github.com/Mininglamp-OSS/octo-server/pkg/redis"
	"github.com/Mininglamp-OSS/octo-server/pkg/reqid"
	spacepkg "github.com/Mininglamp-OSS/octo-server/pkg/space"
	appwkhttp "github.com/Mininglamp-OSS/octo-server/pkg/wkhttp"
	"github.com/gin-gonic/gin"
	"github.com/go-redis/redis"
	"github.com/gocraft/dbr/v2"
	"go.uber.org/zap"
)

// Group 群组相关API
type Group struct {
	ctx *config.Context
	log.Log
	db            *DB
	settingDB     *settingDB
	userDB        *user.DB
	groupService  IService
	fileService   file.IService
	commonService common2.IService
}

// New New
func New(ctx *config.Context) *Group {

	g := &Group{
		ctx:           ctx,
		Log:           log.NewTLog("Group"),
		db:            NewDB(ctx),
		userDB:        user.NewDB(ctx),
		settingDB:     newSettingDB(ctx),
		groupService:  NewService(ctx),
		fileService:   file.NewService(ctx),
		commonService: common2.NewService(ctx),
	}
	g.ctx.AddEventListener(event.GroupDisband, g.handleGroupDisbandEvent)
	g.ctx.AddEventListener(event.EventUserRegister, g.handleRegisterUserEvent)
	g.ctx.AddEventListener(event.GroupMemberAdd, g.handleGroupMemberAddEvent)
	g.ctx.AddEventListener(event.OrgOrDeptCreate, g.handleOrgOrDeptCreateEvent)
	g.ctx.AddEventListener(event.OrgOrDeptEmployeeUpdate, g.handleOrgOrDeptEmployeeUpdate)
	g.ctx.AddEventListener(event.OrgEmployeeExit, g.handleOrgEmployeeExit)
	source.SetGroupMemberProvider(g)
	return g
}

// Route 路由配置
func (g *Group) Route(r *wkhttp.WKHttp) {
	group := r.Group("/v1/group", g.ctx.AuthMiddleware(r))
	{
		group.POST("/create", g.groupCreate)
		group.GET("/my", g.list)                            //我保存的群
		group.GET("/forbidden_times", g.forbiddenTimesList) // 获取禁言时常列表
	}
	groups := r.Group("/v1/groups", g.ctx.AuthMiddleware(r))
	{
		groups.POST("/:group_no/members", g.memberAdd)                                     // 添加群成员
		groups.DELETE("/:group_no/members", g.memberRemove)                                // 移除群成员
		groups.GET("/:group_no/members", g.membersGet)                                     // 获取群成员
		groups.GET("/:group_no/members/:uid", g.memberGet)                                 // 查询单个 uid 是否为群成员（命中时返回成员详情）
		groups.POST("/:group_no/members_delete", g.memberRemove)                           // 移除群成员
		groups.GET("/:group_no/membersync", g.syncMembers)                                 // 同步群成员
		groups.GET("/:group_no", g.groupGet)                                               // 获取群信息
		groups.PUT("/:group_no/setting", g.groupSettingUpdate)                             // 修改群设置
		groups.PUT("/:group_no", g.groupUpdate)                                            // 修改群信息
		groups.PUT("/:group_no/members/:uid", g.memberUpdate)                              // 修改群的群成员信息
		groups.POST("/:group_no/exit", g.groupExit)                                        // 退出群聊
		groups.POST("/:group_no/managers", g.managerAdd)                                   // 添加群管理员
		groups.DELETE("/:group_no/managers", g.managerRemove)                              // 移除群管理员
		groups.POST("/:group_no/forbidden/:on", g.groupForbidden)                          // 群全员禁言
		groups.GET("/:group_no/qrcode", g.groupQRCode)                                     // 获取群二维码信息
		groups.POST("/:group_no/transfer/:to_uid", g.transferGrouper)                      // 群主转让
		groups.POST("/:group_no/member/invite", g.groupMemberInviteAdd)                    // 群成员邀请
		groups.GET("/:group_no/member/h5confirm", g.getToGroupMemberConfirmInviteDetailH5) // 获取确认邀请的h5页面
		groups.POST("/:group_no/blacklist/:action", g.blacklist)                           // 添加或移除黑名单
		groups.POST("/:group_no/forbidden_with_member", g.forbiddenWithGroupMember)        // 禁言或解禁某个群成员
		groups.POST("/:group_no/avatar", g.avatarUpload)                                   // 上传群头像
		groups.DELETE("/:group_no/disband", g.disband)                                     // 解散群
		groups.GET("/:group_no/detail", g.groupDetailGet)                                  // 获取群详情
		groups.GET("/:group_no/md", g.groupMdGet)                                          // 获取GROUP.md
		groups.PUT("/:group_no/md", g.groupMdUpdate)                                       // 更新GROUP.md
		groups.DELETE("/:group_no/md", g.groupMdDelete)                                    // 删除GROUP.md
		groups.PUT("/:group_no/bot_admin/:uid", g.botAdminSet)                             // 设置Bot管理员
		groups.DELETE("/:group_no/bot_admin/:uid", g.botAdminRemove)                       // 移除Bot管理员
	}
	openGroups := r.Group("/v1/groups")
	{ // 获取群头像
		openGroups.GET("/:group_no/avatar", g.avatarGet) // 获取群头像
	}
	authGroups := r.Group("/v1/groups", g.ctx.AuthMiddleware(r))
	{
		authGroups.GET("/:group_no/scanjoin", g.groupScanJoin) // 扫码加入群（需要认证）
	}
	// H5 公开落地页配套的认证接口：把公开 code（二维码 UUID）换成当前登录用户的 auth_code。
	// 之后前端直接调用 /v1/groups/:group_no/scanjoin?auth_code=xxx 完成入群。
	//
	// 挂载 SharedUIDRateLimiter：authorize 每次调用都会往 Redis 写一条 TTL=30min 的 auth_code
	// 记录。虽然有 AuthMiddleware，但登录用户仍可高频批量调用灌满 Redis。进程级共享的 per-UID
	// 令牌桶（默认 2 rps, burst 60）把 UID 粒度的配额统一封顶，同时与 /v1/message、/v1/conversation
	// 等认证路由保持一致的“按登录用户公平”语义，避免 NAT 场景下误伤同办公室合法用户。
	authInviteGroup := r.Group("/v1/group", g.ctx.AuthMiddleware(r), appwkhttp.SharedUIDRateLimiter(r, g.ctx))
	{
		authInviteGroup.POST("/invite/authorize", g.groupInviteAuthorize)
	}
	// 公开邀请落地页（无需认证）per-IP 限流：防枚举 + 暴破。
	// preview/detail 共享同一 limiter。
	//
	// 默认值（YUJ-43 决策，2026-04-25）：60 rps / burst 200 ≈ 3600 req/min，
	// 实质上不限流。理由：
	//   - 外部群功能刚上线，真人访问量很小，短期没 DDoS 风险
	//   - TestBot E2E 真人模拟（10 次刷新同一 URL）之前撞 10 req/min 阻塞测试
	//   - 测试服务器临时改 env 不便，选择代码默认放开、生产需要时再通过 env 收紧
	// 后续由 #1179 最终调优策略定夺严格默认值（原 space 模块默认 10 req/min, burst 5）。
	//
	// 支持通过环境变量覆盖：
	//   - DM_API_GROUP_INVITE_RPS   每秒填充速率（float，缺省 60.0）
	//   - DM_API_GROUP_INVITE_BURST 桶容量（int，缺省 200）
	// 生产环境如需收紧，在部署时设置 env（如 RPS=0.1667 / BURST=5 恢复到 10 req/min）。
	rlRedis := octoredis.NewInstrumentedClient(g.ctx.GetConfig(), func(o *redis.Options) {
		o.MaxRetries = 1
		o.PoolSize = 10
	})
	inviteRPS := wkhttp.ParseRPSFromEnv("DM_API_GROUP_INVITE_RPS", 60.0) // 默认 60 rps ≈ 3600 req/min
	inviteBurst := wkhttp.ParseBurstFromEnv("DM_API_GROUP_INVITE_BURST", 200)
	groupInviteLimit := r.StrictIPRateLimitMiddleware(context.Background(), rlRedis, "group_invite", inviteRPS, inviteBurst)

	openGroup := r.Group("/v1/group")
	{

		openGroup.POST("invite/sure", g.groupMemberInviteSure)                 // 确认邀请
		openGroup.GET("/invite", groupInviteLimit, g.groupInvitePage)          // H5 邀请落地页（公开）
		openGroup.GET("/invite/detail", groupInviteLimit, g.groupInviteDetail) // 群邀请预览信息（公开）
	}
	// 邀请详情需要认证
	group.GET("/invites/:invite_no", g.groupMemberInviteDetail) // 获取邀请详情
	go g.CheckForbiddenLoop()
}

// 解散群
func (g *Group) disband(c *wkhttp.Context) {
	groupNo := c.Param("group_no")
	loginUID := c.GetLoginUID()
	loginName := c.GetLoginName()
	if groupNo == "" {
		respondGroupRequestInvalid(c, "group_no")
		return
	}
	group, err := g.db.QueryWithGroupNo(groupNo)
	if err != nil {
		g.Error("查询群资料错误", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrGroupQueryFailed, nil, nil)
		return
	}
	if group == nil || group.Status == GroupStatusDisband {
		c.ResponseOK()
		return
	}
	loginMember, err := g.db.QueryMemberWithUID(loginUID, groupNo)
	if err != nil {
		g.Error("查询用户群内身份错误", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrGroupQueryFailed, nil, nil)
		return
	}
	if loginMember == nil || loginMember.Role != MemberRoleCreator {
		g.Error("用户无权执行此操作", zap.Error(err))
		respondGroupForbidden(c)
		return
	}

	// todo
	tx, err := g.ctx.DB().Begin()
	if err != nil {
		g.Error("开启事务失败！", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrGroupStoreFailed, nil, nil)
		return
	}
	defer func() {
		if err := recover(); err != nil {
			tx.RollbackUnlessCommitted()
			fmt.Fprintf(os.Stderr, "recovered panic in goroutine: %v\n%s\n", err, debug.Stack())
		}
	}()
	group.Status = GroupStatusDisband
	err = g.db.UpdateTx(group, tx)
	if err != nil {
		tx.Rollback()
		g.Error("修改群状态错误", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrGroupStoreFailed, nil, nil)
		return
	}
	// err = g.db.deleteMembersWithGroupNOTx(groupNo, tx)
	// if err != nil {
	// 	tx.Rollback()
	// 	g.Error("删除群成员错误", zap.Error(err))
	// 	c.ResponseError(errors.New("删除群成员错误"))
	// 	return
	// }
	// 发布群解散事件
	eventID, err := g.ctx.EventBegin(&wkevent.Data{
		Event: event.GroupDisband,
		Type:  wkevent.Message,
		Data: &config.MsgGroupDisband{
			GroupNo:      groupNo,
			Operator:     loginUID,
			OperatorName: loginName,
		},
	}, tx)
	if err != nil {
		tx.RollbackUnlessCommitted()
		g.Error("开启事件失败！", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrGroupStoreFailed, nil, nil)
		return
	}
	if err := tx.Commit(); err != nil {
		tx.RollbackUnlessCommitted()
		g.Error("提交事务失败！", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrGroupStoreFailed, nil, nil)
		return
	}
	g.ctx.EventCommit(eventID)
	c.ResponseOK()
}

func (g *Group) membersGet(c *wkhttp.Context) {
	keyword := c.Query("keyword")
	groupNo := resolveGroupNo(c.Param("group_no"))
	limit, _ := strconv.ParseUint(c.Query("limit"), 10, 64)
	page, _ := strconv.ParseUint(c.Query("page"), 10, 64)
	if page <= 0 {
		page = 1
	}

	if limit <= 0 || limit > 100000 {
		limit = 100
	}
	// Verify user is a group member
	loginUID := c.GetLoginUID()
	isMember, err := g.db.ExistMember(loginUID, groupNo)
	if err != nil {
		g.Error("查询群成员关系失败", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrGroupQueryFailed, nil, nil)
		return
	}
	if !isMember {
		httperr.ResponseErrorL(c, errcode.ErrGroupViewForbidden, nil, nil)
		return
	}

	var members []*MemberDetailModel
	members, err = g.db.queryMembersWithKeyword(groupNo, loginUID, keyword, page, limit)
	if err != nil {
		g.Error("查询成员列表失败！", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrGroupQueryFailed, nil, nil)
		return
	}

	resps := make([]memberDetailResp, 0)
	if len(members) > 0 {
		for _, memberModel := range members {
			resp := memberDetailResp{}
			resps = append(resps, resp.from(memberModel))
		}
	}
	// membersGet 路径未提前查过 group 表，传空 groupSpaceID 让 fillSpaceRelatedFields
	// 内部按需兜底（仅在存在内部成员时才多做一次 group 查询）。
	g.fillSpaceRelatedFields(groupNo, "", resps)
	// YUJ-413 Scope B：批量回填实名字段（零 N+1），Android 气泡 + 群成员列表依赖。
	g.fillRealnameFields(resps)

	c.Response(resps)
}

// memberGet 查询单个 uid 是否为该群成员。
//
// 鉴权：调用方必须是该群成员（与 membersGet 一致），否则返回 400。
// 命中：返回 {"exists": true, "member": {...memberDetailResp}}；
// 未命中或已删除：返回 {"exists": false}。
func (g *Group) memberGet(c *wkhttp.Context) {
	groupNo := resolveGroupNo(c.Param("group_no"))
	targetUID := c.Param("uid")
	loginUID := c.GetLoginUID()

	isMember, err := g.db.ExistMember(loginUID, groupNo)
	if err != nil {
		g.Error("查询群成员关系失败", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrGroupQueryFailed, nil, nil)
		return
	}
	if !isMember {
		httperr.ResponseErrorL(c, errcode.ErrGroupViewForbidden, nil, nil)
		return
	}

	detail, err := g.db.queryMemberWithGroupNoAndUID(groupNo, targetUID)
	if err != nil {
		g.Error("查询群成员失败", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrGroupQueryFailed, nil, nil)
		return
	}
	if detail == nil {
		c.Response(memberCheckResp{Exists: false})
		return
	}

	resps := []memberDetailResp{(memberDetailResp{}).from(detail)}
	g.fillSpaceRelatedFields(groupNo, "", resps)
	// YUJ-413 Scope B：单成员查询也需要回填实名字段（单 uid 也走批量 API，
	// 调用成本与 N=1 一致），Android 客户端 /v1/groups/:group_no/members/:uid
	// 是资料卡 / @提及 等路径的数据源。
	g.fillRealnameFields(resps)

	c.Response(memberCheckResp{Exists: true, Member: &resps[0]})
}

// memberCheckResp memberGet 的响应体。
type memberCheckResp struct {
	Exists bool              `json:"exists"`
	Member *memberDetailResp `json:"member,omitempty"`
}

func (g *Group) avatarGet(c *wkhttp.Context) {
	groupNo := c.Param("group_no")
	v := c.Query("v")
	//是否为系统群
	if groupNo == g.ctx.GetConfig().Account.SystemGroupID {
		c.Header("Content-Type", "image/jpeg")
		avatarBytes, err := os.ReadFile("assets/assets/g_avatar.jpeg")
		if err != nil {
			g.Error("头像读取失败！", zap.Error(err))
			c.Writer.WriteHeader(http.StatusNotFound)
			return
		}
		c.Writer.Write(avatarBytes)
		return
	}
	// 组织群
	if strings.HasPrefix(groupNo, "org_") {
		c.Header("Content-Type", "image/jpeg")
		avatarBytes, err := os.ReadFile("assets/assets/org_avatar.png")
		if err != nil {
			g.Error("头像读取失败！", zap.Error(err))
			c.Writer.WriteHeader(http.StatusNotFound)
			return
		}
		c.Writer.Write(avatarBytes)
		return
	}
	// 部门群
	if strings.HasPrefix(groupNo, "dept_") {
		c.Header("Content-Type", "image/jpeg")
		avatarBytes, err := os.ReadFile("assets/assets/dept_avatar.png")
		if err != nil {
			g.Error("头像读取失败！", zap.Error(err))
			c.Writer.WriteHeader(http.StatusNotFound)
			return
		}
		c.Writer.Write(avatarBytes)
		return
	}
	groupInfo, err := g.db.QueryWithGroupNo(groupNo)
	if err != nil {
		g.Error("查询群资料错误", zap.String("group_no", groupNo), zap.Error(err))
		c.Writer.WriteHeader(http.StatusInternalServerError)
		return
	}
	// 群不存在或已解散 → 404（与 UserAvatar 对未知用户、以及本模块 getGroupInfo /
	// UpdateGroupAvatarCustom 等对 GroupStatusDisband 的处理一致），不再为其渲染默认图：
	// 否则公开未鉴权端点会把已解散群的群名前 2 字渲成 PNG（信息泄露 + 可区分「已解散」与
	// 「从未存在」的枚举面）。系统群 / org_ / dept_ 已在上方静态分支处理，到这里必为普通群。
	if groupInfo == nil || groupInfo.Status == GroupStatusDisband {
		c.Writer.WriteHeader(http.StatusNotFound)
		return
	}

	// 群主已上传自定义头像：重定向到版本化对象存储（沿用历史逻辑）。
	if groupInfo.IsUploadAvatar == 1 {
		path := g.ctx.GetConfig().GetGroupAvatarFilePath(groupNo, groupInfo.AvatarVersion)
		downloadUrl, err := g.fileService.DownloadURL(path, "")
		if err != nil {
			g.Error("获取下载路径失败！", zap.Error(err))
			c.Writer.WriteHeader(http.StatusInternalServerError)
			return
		}
		if strings.Contains(downloadUrl, "?") {
			c.Redirect(http.StatusFound, fmt.Sprintf("%s&v=%s", downloadUrl, v))
		} else {
			c.Redirect(http.StatusFound, fmt.Sprintf("%s?v=%s", downloadUrl, v))
		}
		return
	}

	// 无自定义上传（含历史合成群、新建群）：服务端实时渲染默认头像——
	// 浅底描边圆 + 群名前 2 字（或自定义文字），群名为空/不可渲染时回退群组图标。
	g.writeGroupDefaultAvatar(c, groupNo, groupInfo)
}

// writeGroupDefaultAvatar 服务端渲染并返回群默认头像（无自定义上传时）。文字优先
// 自定义 avatar_text，否则群名前 2 字；颜色优先自定义 avatar_color，否则按 group_no
// 稳定派生（改名不变色、跨页面一致）。群名为空或不可渲染时回退群组图标。
//
// URL 稳定为 groups/{group_no}/avatar，内容随群名/自定义变化，故用内容相关弱 ETag +
// 短缓存 + must-revalidate：改名后最多 5 分钟内 revalidate 到新图，并支持 304 省渲染。
// ETag 只依赖输入（无需渲染），故先算 ETag 命中 If-None-Match 时直接 304。
func (g *Group) writeGroupDefaultAvatar(c *wkhttp.Context, groupNo string, groupInfo *Model) {
	style := avatarrender.GroupStyleForSeed(groupNo)
	colorTag := "seed"
	var text string
	if groupInfo != nil {
		if groupInfo.AvatarText != "" {
			// 用户**显式设置**的自定义文字：原样渲染(写入时已按 ≤4 规范化)，不走群名
			// 自动取字规则(不截成 2 字、不做首字母缩写)。
			text = avatarrender.GroupText(groupInfo.AvatarText)
		} else {
			// 无自定义文字：按群名**自动取字**(script 感知 —— 汉字前2 / 纯数字前2 /
			// 纯英文首字母缩写 / 否则空→回退群组图标)。
			text = avatarrender.GroupNameText(groupInfo.Name)
		}
		if groupInfo.AvatarColor != nil {
			if customStyle, ok := avatarrender.GroupStyleByIndex(*groupInfo.AvatarColor); ok {
				style = customStyle
				colorTag = "idx" + strconv.Itoa(*groupInfo.AvatarColor)
			}
		}
	}
	renderable := avatarrender.Renderable(text)

	// ETag 覆盖决定图像内容的因子：渲染模式版本 + group_no(派生色) + 实际色 + 文字。
	// 改名/改自定义文字 → text 变 → ETag 变；改自定义色 → colorTag 变 → ETag 变。
	// 渲染**视觉/取字规则**改动(像素变但因子不变，如 #486 透明四角、本次取字改版)必须
	// bump 版本段，否则已缓存旧图的客户端 If-None-Match 命中同一 ETag → 304 → 一直返旧图。
	// group-name-v4: 群名取字改为 script 感知前 2 字(原 v3 取前 4)。

	etag := avatarrender.ETag("group-icon-v3", groupNo, colorTag)
	if renderable {
		etag = avatarrender.ETag("group-name-v4", groupNo, colorTag, text)
	}
	c.Header("Content-Disposition", "inline; filename=avatar.png")
	c.Header("ETag", etag)
	c.Header("Cache-Control", "public, max-age=300, must-revalidate")
	if avatarrender.IfNoneMatch(c.GetHeader("If-None-Match"), etag) {
		c.Status(http.StatusNotModified)
		return
	}

	// 非条件 GET（disable-cache / 首屏 / 共享缓存 miss）绕过上面的 304 快路径，落到这里
	// 真渲染。成员/会话列表扇出下大量并发非条件 GET 会把 CPU 打满、饿死同机其它请求
	// （issue#480）。渲染统一走进程级共享缓存 GetOrRender（与 user 同一 LRU + 同一渲染
	// 信号量）：命中复用字节、singleflight 合并冷渲染、信号量限并发，确保一次扇出最多渲
	// 一张。缓存 key 用 CacheKey（完整原始因子，与 ETag 同因子但**非** CRC32 弱指纹），
	// 避免 32 位碰撞跨群串图。
	if renderable {
		nameKey := avatarrender.CacheKey("group-name-v4", groupNo, colorTag, text)
		imageData, genErr := avatarrender.GetOrRender(nameKey, func() ([]byte, error) {
			return avatarrender.RenderGroup(text, style, avatarrender.DefaultSize)
		})
		if genErr == nil {
			c.Data(http.StatusOK, "image/png", imageData)
			return
		}
		// 渲染失败不直接 500，记录后回退群组图标；ETag 改回 icon 模式与内容一致。
		g.Error("生成群名默认头像失败，回退群组图标", zap.Error(genErr), zap.String("group_no", groupNo))
		c.Header("ETag", avatarrender.ETag("group-icon-v3", groupNo, colorTag))
	}

	// 群名为空 / 不可渲染（如纯 emoji）/ 渲染失败 → 群组图标。
	iconKey := avatarrender.CacheKey("group-icon-v3", groupNo, colorTag)
	iconData, iconErr := avatarrender.GetOrRender(iconKey, func() ([]byte, error) {
		return avatarrender.RenderIcon(style)
	})
	if iconErr != nil {
		g.Error("生成群组图标头像失败", zap.Error(iconErr), zap.String("group_no", groupNo))
		c.Writer.WriteHeader(http.StatusInternalServerError)
		return
	}
	c.Data(http.StatusOK, "image/png", iconData)
}

func (g *Group) avatarUpload(c *wkhttp.Context) {
	loginUID := c.GetLoginUID()
	groupNo := c.Param("group_no")
	if groupNo == "" {
		respondGroupRequestInvalid(c, "group_no")
		return
	}
	_, err := g.getGroupInfo(groupNo)
	if err != nil {
		respondGroupInfoError(c, err)
		return
	}
	if c.Request.MultipartForm == nil {
		err := c.Request.ParseMultipartForm(1024 * 1024 * 20) // 20M
		if err != nil {
			g.Error("数据格式不正确！", zap.Error(err))
			respondGroupRequestInvalid(c, "")
			return
		}
	}
	file, _, err := c.Request.FormFile("file")
	if err != nil {
		// FormFile 在 file 字段缺失/为空时返回 http.ErrMissingFile —— 纯客户端
		// 错误,应为 400,而非内部存储失败。与上面 ParseMultipartForm 分支一致。
		g.Error("读取文件失败！", zap.Error(err))
		respondGroupRequestInvalid(c, "file")
		return
	}
	defer file.Close()

	isCreator, err := g.db.QueryIsGroupCreator(groupNo, loginUID)
	if err != nil {
		g.Error("查询群创建者失败！", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrGroupQueryFailed, nil, nil)
		return
	}
	if !isCreator {
		httperr.ResponseErrorL(c, errcode.ErrGroupCreatorOnly, nil, nil)
		return
	}

	avatarVersion := avatarversion.New()
	groupAvatarPath := g.ctx.GetConfig().GetGroupAvatarFilePath(groupNo, avatarVersion)
	_, err = g.fileService.UploadFile(groupAvatarPath, "image/png", "", func(w io.Writer) error {
		_, err := io.Copy(w, file)
		return err
	})
	if err != nil {
		g.Error("上传文件失败！", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrGroupStoreFailed, nil, nil)
		return
	}
	err = g.db.updateAvatar(groupAvatarPath, avatarVersion, groupNo)
	if err != nil {
		g.Error("头像修改失败！", zap.String("group_no", groupNo), zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrGroupStoreFailed, nil, nil)
		return
	}
	// 发送群头像更新命令
	err = g.ctx.SendCMD(config.MsgCMDReq{
		ChannelID:   groupNo,
		ChannelType: common.ChannelTypeGroup.Uint8(),
		CMD:         common.CMDGroupAvatarUpdate,
		Param: map[string]interface{}{
			"group_no": groupNo,
		},
	})
	if err != nil {
		g.Error("发送群头像更新命令失败！", zap.String("groupNo", groupNo), zap.Error(err))
		// The avatar object and DB version are already committed; the IM
		// notification is best-effort so clients do not retry a successful upload.
		c.ResponseOK()
		return
	}
	c.ResponseOK()
}

// 同步群成员
func (g *Group) syncMembers(c *wkhttp.Context) {
	groupNo := resolveGroupNo(c.Param("group_no"))

	if g.ctx.GetConfig().IsVisitorChannel(groupNo) {
		c.Request.URL.Path = fmt.Sprintf("/v1/hotline/visitor/channels/%s/members", groupNo)
		g.ctx.GetHttpRoute().HandleContext(c)
		return
	}

	// Verify user is a group member
	loginUID := c.GetLoginUID()
	isMember, err := g.db.ExistMember(loginUID, groupNo)
	if err != nil {
		g.Error("查询群成员关系失败", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrGroupQueryFailed, nil, nil)
		return
	}
	if !isMember {
		httperr.ResponseErrorL(c, errcode.ErrGroupViewForbidden, nil, nil)
		return
	}

	group, err := g.db.QueryWithGroupNo(groupNo)
	if err != nil {
		g.Error("查询群信息失败！", zap.Error(err), zap.String("groupNo", groupNo))
		httperr.ResponseErrorL(c, errcode.ErrGroupQueryFailed, nil, nil)
		return
	}
	if group == nil {
		g.Error("群不存在不能同步成员！", zap.String("groupNo", groupNo))
		httperr.ResponseErrorL(c, errcode.ErrGroupNotFound, nil, nil)
		return
	}
	if group.GroupType == int(GroupTypeSuper) {
		g.Error("超大群不支持同步群成员！", zap.String("groupNo", groupNo))
		httperr.ResponseErrorL(c, errcode.ErrGroupTooLargeToSync, nil, nil)
		return
	}

	limit, _ := strconv.ParseUint(c.Query("limit"), 10, 64)
	if limit <= 0 {
		limit = 100
	}
	version, _ := strconv.ParseInt(c.Query("version"), 10, 64)
	memberModels, err := g.db.SyncMembers(groupNo, version, limit)
	if err != nil {
		g.Error("同步成员信息失败！", zap.Error(err), zap.String("groupNo", groupNo))
		httperr.ResponseErrorL(c, errcode.ErrGroupQueryFailed, nil, nil)
		return
	}
	resps := make([]memberDetailResp, 0)
	for _, memberModel := range memberModels {
		resp := memberDetailResp{}
		resps = append(resps, resp.from(memberModel))
	}
	// syncMembers 在 L446 已经 QueryWithGroupNo 过一次，把 group.SpaceID 透传进去
	// 可避免 fillSpaceRelatedFields 再做一次群查询（Jerry-Xin review 优化建议）。
	g.fillSpaceRelatedFields(groupNo, group.SpaceID, resps)
	// YUJ-413 Scope B：同 membersGet / memberGet，批量回填实名字段。
	// membersync 是 Android WKSDK ChannelMember 缓存的唯一增量来源，漏下发就
	// 永远没 extraMap.realname_verified → 气泡 + 群成员列表永远不亮。
	g.fillRealnameFields(resps)
	c.Response(resps)
}

// 获取群详情
func (g *Group) groupGet(c *wkhttp.Context) {
	groupNo := c.Param("group_no")
	// if g.ctx.GetConfig().IsVisitorChannel(groupNo) { // 访客频道
	// 	c.Request.URL.Path = fmt.Sprintf("/v1/hotline/visitor/channel/%s", groupNo)
	// 	g.ctx.Server.GetRoute().HandleContext(c)
	// 	return
	// }
	uid := c.MustGet("uid").(string)

	// Verify user is a group member
	isMember, err := g.db.ExistMember(uid, groupNo)
	if err != nil {
		g.Error("查询群成员关系失败", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrGroupQueryFailed, nil, nil)
		return
	}
	if !isMember {
		httperr.ResponseErrorL(c, errcode.ErrGroupViewForbidden, nil, nil)
		return
	}

	groupResp, err := g.groupService.GetGroupDetail(groupNo, uid)
	if err != nil {
		g.Error("查询群详情失败", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrGroupQueryFailed, nil, nil)
		return
	}
	c.Response(groupResp)
}

// 获取群详情
func (g *Group) groupDetailGet(c *wkhttp.Context) {
	groupNo := c.Param("group_no")
	loginUID := c.GetLoginUID()

	groupModel, err := g.db.QueryWithGroupNo(groupNo)
	if err != nil {
		g.Error("查询群信息失败！", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrGroupQueryFailed, nil, nil)
		return
	}
	if groupModel == nil {
		httperr.ResponseErrorL(c, errcode.ErrGroupNotFound, nil, nil)
		return
	}

	// 检查用户是否是群成员
	isMember, err := g.db.ExistMember(loginUID, groupNo)
	if err != nil {
		g.Error("检查群成员失败！", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrGroupQueryFailed, nil, nil)
		return
	}
	if !isMember {
		httperr.ResponseErrorL(c, errcode.ErrGroupViewForbidden, nil, nil)
		return
	}

	memberCount, err := g.db.QueryMemberCount(groupNo)
	if err != nil {
		g.Error("查询成员数量失败！", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrGroupQueryFailed, nil, nil)
		return
	}
	// YUJ-168 / GH #1243: 外部群 H5 邀请 landing 页的信任锚点。
	// - space_name：群所属 Space 名（无 SpaceID 时为空，前端据此决定是否渲染"来自 xx"）。
	// - is_external：访问者相对群 Space 的外部性。仅在已登录且 loginUID 不是该 Space 成员时置 1。
	//   Space 查询 / 成员校验任一失败都视为非外部（0），保证展示降级而非阻塞主接口。
	spaceName, _ := spacepkg.GetSpaceName(g.ctx.DB(), groupModel.SpaceID)
	isExternal := 0
	if groupModel.SpaceID != "" && loginUID != "" {
		inSpace, checkErr := spacepkg.CheckMembership(g.ctx.DB(), groupModel.SpaceID, loginUID)
		if checkErr != nil {
			g.Warn("检查 Space 成员失败", zap.Error(checkErr), zap.String("group_no", groupNo))
		} else if !inSpace {
			isExternal = 1
		}
	}
	c.Response(groupDetailResp{}.from(groupModel, memberCount, spaceName, isExternal))
}

// list 我保存的群聊
func (g *Group) list(c *wkhttp.Context) {
	loginUID := c.MustGet("uid").(string)
	spaceID := c.Query("space_id")

	if spaceID != "" {
		// Space 模式：返回该 Space 下用户加入的所有群
		groups, err := g.db.queryGroupsWithMemberUIDAndSpaceID(loginUID, spaceID)
		if err != nil {
			g.Error("查询Space群列表失败", zap.Error(err))
			httperr.ResponseErrorL(c, errcode.ErrGroupQueryFailed, nil, nil)
			return
		}
		resps := make([]*GroupResp, 0)
		for _, model := range groups {
			groupResp := &GroupResp{}
			resp := groupResp.fromModel(model)
			// 查询成员数
			memberCount, err := g.db.QueryMemberCount(model.GroupNo)
			if err == nil {
				resp.MemberCount = int(memberCount)
			}
			resps = append(resps, resp)
		}
		c.Response(resps)
		return
	}

	models, err := g.db.querySavedGroups(loginUID)
	if err != nil {
		g.Error("查询我保存的群聊失败", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrGroupQueryFailed, nil, nil)
		return
	}
	resps := make([]*GroupResp, 0)
	for _, model := range models {
		groupResp := &GroupResp{}
		resps = append(resps, groupResp.from(model))
	}
	c.Response(resps)
}

// 创建群
func (g *Group) groupCreate(c *wkhttp.Context) {
	creator := c.MustGet("uid").(string)
	var req groupReq
	if err := c.BindJSON(&req); err != nil {
		g.Error(common.ErrData.Error(), zap.Error(err))
		respondGroupRequestInvalid(c, "")
		return
	}
	if err := req.Check(); err != nil {
		respondGroupRequestInvalid(c, "")
		return
	}
	if field, ok := req.checkAvatar(); !ok {
		respondGroupRequestInvalid(c, field)
		return
	}
	// 存清洗后的自定义头像文字（剔除不可见字符、上限 4 可见 rune），与校验口径一致，避免
	// 零宽/格式字符撑爆 avatar_text VARCHAR(16) 导致 MySQL 截断。
	req.AvatarText = avatarrender.GroupText(req.AvatarText)

	// 校验 category_id
	if req.CategoryID != "" {
		if req.SpaceID == "" {
			respondGroupRequestInvalid(c, "space_id")
			return
		}
		cat, err := g.db.QueryCategoryByID(req.CategoryID)
		if err != nil {
			g.Error("查询群聊分组失败", zap.Error(err))
			httperr.ResponseErrorL(c, errcode.ErrGroupQueryFailed, nil, nil)
			return
		}
		if cat == nil || cat.Status != 1 {
			httperr.ResponseErrorL(c, errcode.ErrGroupCategoryNotFound, nil, nil)
			return
		}
		if cat.UID != creator {
			httperr.ResponseErrorL(c, errcode.ErrGroupCategoryForbidden, nil, nil)
			return
		}
		if cat.SpaceID != req.SpaceID {
			httperr.ResponseErrorL(c, errcode.ErrGroupCategorySpaceMismatch, nil, nil)
			return
		}
	}

	count, err := g.db.querySameDayCreateCountWitUID(creator, util.Toyyyy_MM_dd(time.Now()))
	if err != nil {
		g.Error("查询用户当天建群数量失败！", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrGroupQueryFailed, nil, nil)
		return
	}
	if g.ctx.GetConfig().Group.SameDayCreateMaxCount <= count {
		httperr.ResponseErrorL(c, errcode.ErrGroupDailyCreateLimit, nil, nil)
		return
	}
	realUids := make([]string, 0)
	// 好友验证（Web 特有逻辑，Space 校验已移入 Service）
	if req.SpaceID == "" && g.ctx.GetConfig().Group.CreateGroupVerifyFriendOn {
		friends := make([]*model.FriendResp, 0)
		modules := register.GetModules(g.ctx)
		for _, m := range modules {
			if m.BussDataSource.GetFriends != nil {
				friends, err = m.BussDataSource.GetFriends(creator)
				if err != nil {
					g.Error("查询用户好友错误", zap.Error(err))
					httperr.ResponseErrorL(c, errcode.ErrGroupQueryFailed, nil, nil)
					return
				}
				break
			}
		}
		if len(friends) == 0 {
			httperr.ResponseErrorL(c, errcode.ErrGroupMemberNotFriend, nil, nil)
			return
		}
		if len(req.Members) > 0 {
			for _, uid := range req.Members {
				for _, friend := range friends {
					if uid == friend.ToUID {
						realUids = append(realUids, uid)
						break
					}
				}
			}
		}
	} else {
		realUids = req.Members
	}
	if len(realUids) == 0 {
		httperr.ResponseErrorL(c, errcode.ErrGroupMemberNotFriend, nil, nil)
		return
	}
	// 判断是否允许系统账号进入群聊
	appConfig, err := g.commonService.GetAppConfig()
	if err != nil {
		g.Error("查询应用设置错误", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrGroupQueryFailed, nil, nil)
		return
	}
	if appConfig != nil && appConfig.InviteSystemAccountJoinGroupOn == 0 {
		isContainSystemAccount := false
		for _, uid := range realUids {
			if uid == g.ctx.GetConfig().Account.FileHelperUID {
				isContainSystemAccount = true
				break
			}
		}
		if isContainSystemAccount {
			httperr.ResponseErrorL(c, errcode.ErrGroupFileHelperNotAllowed, nil, nil)
			return
		}
	}

	// 调用 Service 创建群
	createResp, err := g.groupService.CreateGroup(&CreateGroupServiceReq{
		Creator:     creator,
		Members:     realUids,
		Name:        req.Name,
		SpaceID:     req.SpaceID,
		CategoryID:  req.CategoryID,
		AvatarText:  req.AvatarText,
		AvatarColor: req.AvatarColor,
	})
	if err != nil {
		g.Error("创建群失败！", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrGroupStoreFailed, nil, nil)
		return
	}

	// 消息自动删除（Web 特有逻辑）
	creatorUser, err := g.userDB.QueryByUID(creator)
	if err == nil && creatorUser != nil && creatorUser.MsgExpireSecond > 0 {
		channelServiceObj := register.GetService(ChannelServiceName)
		var channelService chservice.IService
		if channelServiceObj != nil {
			channelService = channelServiceObj.(chservice.IService)
		}
		if channelService != nil {
			if chErr := channelService.CreateOrUpdateMsgAutoDelete(createResp.GroupNo, common.ChannelTypeGroup.Uint8(), creatorUser.MsgExpireSecond); chErr != nil {
				g.Warn("更新消息自动删除失败！", zap.Error(chErr))
			}
		}
	}

	// 查询群信息返回响应
	groupModel, err := g.db.QueryWithGroupNo(createResp.GroupNo)
	if err != nil {
		g.Error("查询群信息失败！", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrGroupQueryFailed, nil, nil)
		return
	}
	groupResp := &GroupResp{}
	resp := groupResp.from(&DetailModel{
		Model:        *groupModel,
		Receipt:      1,
		RevokeRemind: 1,
		Screenshot:   1,
	})
	// 查询成员数
	memberCount, mcErr := g.db.QueryMemberCount(createResp.GroupNo)
	if mcErr == nil {
		resp.MemberCount = int(memberCount)
	}
	c.Response(resp)
}

// 修改群信息
func (g *Group) groupUpdate(c *wkhttp.Context) {
	loginUID := c.MustGet("uid").(string)
	loginName := c.MustGet("name").(string)
	groupNo := c.Param("group_no")

	var groupMap map[string]string
	if err := c.BindJSON(&groupMap); err != nil {
		g.Error("数据格式有误！", zap.Error(err))
		respondGroupRequestInvalid(c, "")
		return
	}
	if len(groupMap) <= 0 {
		respondGroupRequestInvalid(c, "")
		return
	}
	// 查询群信息
	group, err := g.getGroupInfo(groupNo)
	if err != nil {
		respondGroupInfoError(c, err)
		return
	}
	// 查询是否是管理者
	isManager, err := g.db.QueryIsGroupManagerOrCreator(groupNo, loginUID)
	if err != nil {
		g.Error("查询是否是群管理者失败！", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrGroupQueryFailed, nil, nil)
		return
	}
	if !isManager {
		httperr.ResponseErrorL(c, errcode.ErrGroupManagerOnly, nil, nil)
		return
	}

	// invite 属性不走 Service（Service 只处理 name/notice），仍走原有逻辑
	inviteValue, hasInvite := groupMap[common.GroupAttrKeyInvite]
	nameValue, hasName := groupMap[common.GroupAttrKeyName]
	noticeValue, hasNotice := groupMap[common.GroupAttrKeyNotice]

	// 自定义群头像文字/颜色（二次弹窗保存）：先在任何 mutation 之前完成解析与校验，构造
	// avatarReq。这样非法的 avatar 字段会在 name/notice/invite 落库之前返回 400，避免
	// 「返回 400 却已部分写入群名」的非原子部分写入。avatar_text 空串清除自定义文字（回退
	// 群名），avatar_color "" / "-1" 清除自定义色（回退派生）。超限直接拒绝，不静默截断。
	avatarTextValue, hasAvatarText := groupMap[attrKeyAvatarText]
	avatarColorValue, hasAvatarColor := groupMap[attrKeyAvatarColor]
	var avatarReq *UpdateGroupAvatarCustomServiceReq
	if hasAvatarText || hasAvatarColor {
		avatarReq = &UpdateGroupAvatarCustomServiceReq{
			GroupNo:      groupNo,
			OperatorUID:  loginUID,
			OperatorName: loginName,
		}
		if hasAvatarText {
			if avatarrender.VisibleRuneCount(avatarTextValue) > 4 {
				respondGroupRequestInvalid(c, attrKeyAvatarText)
				return
			}
			// 存清洗后的文本（剔除不可见字符、上限 4 可见 rune），与校验口径一致，避免零宽/
			// 格式字符撑爆 avatar_text VARCHAR(16) 导致 MySQL 截断。
			cleaned := avatarrender.GroupText(avatarTextValue)
			avatarReq.AvatarText = &cleaned
		}
		if hasAvatarColor {
			ci := -1 // "" / "-1" → 清除
			if raw := strings.TrimSpace(avatarColorValue); raw != "" {
				parsed, convErr := strconv.Atoi(raw)
				if convErr != nil {
					respondGroupRequestInvalid(c, attrKeyAvatarColor)
					return
				}
				ci = parsed
			}
			if ci < -1 || ci >= avatarrender.PaletteSize() {
				respondGroupRequestInvalid(c, attrKeyAvatarColor)
				return
			}
			avatarReq.SetAvatarColor = true
			if ci >= 0 {
				avatarReq.AvatarColor = &ci
			}
		}
	}

	// 校验全部通过后再执行各项写入。
	// 如果有 name 或 notice，走 Service
	if hasName || hasNotice {
		serviceReq := &UpdateGroupInfoServiceReq{
			GroupNo:      groupNo,
			OperatorUID:  loginUID,
			OperatorName: loginName,
		}
		if hasName {
			serviceReq.Name = &nameValue
		}
		if hasNotice {
			serviceReq.Notice = &noticeValue
		}
		if err := g.groupService.UpdateGroupInfo(serviceReq); err != nil {
			g.Error("更新群信息失败！", zap.Error(err))
			httperr.ResponseErrorL(c, errcode.ErrGroupStoreFailed, nil, nil)
			return
		}
	}

	// invite 属性单独处理（保留原有事务逻辑）
	if hasInvite {
		invite, _ := strconv.ParseInt(inviteValue, 10, 64)
		group.Invite = int(invite)
		version, err := g.ctx.GenSeq(common.GroupSeqKey)
		if err != nil {
			g.Error("生成序列号失败", zap.Error(err))
			httperr.ResponseErrorL(c, errcode.ErrGroupStoreFailed, nil, nil)
			return
		}
		group.Version = version

		tx, err := g.ctx.DB().Begin()
		if err != nil {
			g.Error("开启事务失败！", zap.Error(err))
			httperr.ResponseErrorL(c, errcode.ErrGroupStoreFailed, nil, nil)
			return
		}
		defer func() {
			if err := recover(); err != nil {
				tx.Rollback()
				fmt.Fprintf(os.Stderr, "recovered panic in goroutine: %v\n%s\n", err, debug.Stack())
			}
		}()
		err = g.db.UpdateTx(group, tx)
		if err != nil {
			tx.Rollback()
			g.Error("更新群信息失败！", zap.Error(err), zap.String("group_no", group.GroupNo))
			httperr.ResponseErrorL(c, errcode.ErrGroupStoreFailed, nil, nil)
			return
		}
		eventID, err := g.ctx.EventBegin(&wkevent.Data{
			Event: event.GroupUpdate,
			Type:  wkevent.Message,
			Data: &config.MsgGroupUpdateReq{
				GroupNo:      groupNo,
				Operator:     loginUID,
				OperatorName: loginName,
				Attr:         common.GroupAttrKeyInvite,
				Data:         map[string]string{common.GroupAttrKeyInvite: inviteValue},
			},
		}, tx)
		if err != nil {
			tx.Rollback()
			g.Error("开启事件失败！", zap.Error(err))
			httperr.ResponseErrorL(c, errcode.ErrGroupStoreFailed, nil, nil)
			return
		}
		if err := tx.Commit(); err != nil {
			tx.RollbackUnlessCommitted()
			g.Error("提交事务失败！", zap.Error(err))
			httperr.ResponseErrorL(c, errcode.ErrGroupStoreFailed, nil, nil)
			return
		}
		g.ctx.EventCommit(eventID)
	}

	// 自定义群头像文字/颜色落库（avatarReq 已在 mutation 前校验通过）。
	if avatarReq != nil {
		if err := g.groupService.UpdateGroupAvatarCustom(avatarReq); err != nil {
			g.Error("更新群头像自定义失败！", zap.Error(err), zap.String("group_no", groupNo))
			httperr.ResponseErrorL(c, errcode.ErrGroupStoreFailed, nil, nil)
			return
		}
	}

	c.ResponseOK()
}

// 添加成员
func (g *Group) memberAdd(c *wkhttp.Context) {
	// 链路耗时定位（邀请成员 POST 慢排查）：handlerStart 覆盖整个 handler。
	// service 内 AddGroupMembers 的分段日志只量到 service 那一段，handler 在
	// 调 service 之前的写前校验（getGroupInfo / ExistMember / checkBotOwnership
	// / GetAppConfig / Bot Space 隔离循环）此前没人测——这里把那段补齐，
	// 复用 inviteSlowLogThresholdMS 同一慢阈值，仅慢请求落日志。
	handlerStart := time.Now()
	var (
		getGroupMs    int64
		existMemberMs int64
		botOwnerMs    int64
		appConfigMs   int64
		botSpaceMs    int64
		preChecksMs   int64
		connAcquireMs int64
	)
	traceID := c.GetString(reqid.GinKey)
	operator := c.MustGet("uid").(string)
	operatorName := c.MustGet("name").(string)
	var req memberAddReq
	if err := c.BindJSON(&req); err != nil {
		g.Error(common.ErrData.Error(), zap.Error(err))
		respondGroupRequestInvalid(c, "")
		return
	}
	if err := req.Check(); err != nil {
		respondGroupRequestInvalid(c, "")
		return
	}
	groupNo := c.Param("group_no")

	// 连接获取探针（邀请慢二级定位）：显式从连接池取一条连接并 PingContext 强制
	// 一次往返，单独计时。pre_checks 各步的 *_ms 是「取连接 + 执行」之和；本探针把
	// 「取连接」这一段单拎出来——若 conn_acquire_ms 很小但 get_group_ms 等仍大，说明
	// 慢在查询执行；若本探针本身就大，说明慢在连接获取 / 复用到被服务端掐死的连接后重连。
	var connProbeErr string
	if sqlDB := g.ctx.DB().Connection.DB; sqlDB != nil {
		tProbe := time.Now()
		conn, cErr := sqlDB.Conn(c.Request.Context())
		if cErr != nil {
			connProbeErr = cErr.Error()
		} else {
			if pErr := conn.PingContext(c.Request.Context()); pErr != nil {
				connProbeErr = pErr.Error()
			}
			_ = conn.Close() // 归还连接池
		}
		connAcquireMs = time.Since(tProbe).Milliseconds()
	}

	/**
	判断群是否存在
	**/
	tStep := time.Now()
	group, err := g.getGroupInfo(groupNo)
	getGroupMs = time.Since(tStep).Milliseconds()
	if err != nil {
		respondGroupInfoError(c, err)
		return
	}
	// 校验操作者是群成员,防止任意用户向任意群添加成员(issue#1018)
	tStep = time.Now()
	isMember, err := g.db.ExistMember(operator, groupNo)
	existMemberMs = time.Since(tStep).Milliseconds()
	if err != nil {
		g.Error("查询群成员关系失败", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrGroupQueryFailed, nil, nil)
		return
	}
	if !isMember {
		httperr.ResponseErrorL(c, errcode.ErrGroupNotMember, nil, nil)
		return
	}

	// Bot Ownership 校验（YUJ-46 / issue #1181）：只有 bot 的 creator
	// 本人能把该 bot 拉入任何群。前端 UI 做过过滤，但后端接口之前完全未
	// 校验，导致任意群成员可把任意 bot 拉进任意群。策略先保守：
	// 群主 / 群管理员同样不能跨 creator 邀请 bot；系统 bot / 公共 bot 暂无
	// 白名单，一律走 creator 路径。参见 checkBotOwnership 函数注释。
	// 注意：该检查故意放在系统账号 / Invite 模式等策略检查之前，以最小化
	// 攻击面——非授权方对 bot 的探测请求应尽早被拒绝。
	tStep = time.Now()
	botErr := checkBotOwnership(g.ctx.DB(), operator, req.Members)
	botOwnerMs = time.Since(tStep).Milliseconds()
	if botErr != nil {
		if errors.Is(botErr, ErrBotOwnershipDenied) {
			httperr.ResponseErrorL(c, errcode.ErrGroupBotOwnershipDenied, nil, nil)
			return
		}
		g.Error("检查 Bot 归属失败", zap.Error(botErr))
		httperr.ResponseErrorL(c, errcode.ErrGroupQueryFailed, nil, nil)
		return
	}

	// 判断是否允许系统账号进入群聊
	tStep = time.Now()
	appConfig, err := g.commonService.GetAppConfig()
	appConfigMs = time.Since(tStep).Milliseconds()
	if err != nil {
		g.Error("查询应用设置错误", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrGroupQueryFailed, nil, nil)
		return
	}
	if appConfig != nil && appConfig.InviteSystemAccountJoinGroupOn == 0 {
		isContainSystemAccount := false
		for _, uid := range req.Members {
			if uid == g.ctx.GetConfig().Account.FileHelperUID {
				isContainSystemAccount = true
				break
			}
		}
		if isContainSystemAccount {
			httperr.ResponseErrorL(c, errcode.ErrGroupFileHelperNotAllowed, nil, nil)
			return
		}
	}
	/**
	判断群是否开启了邀请模式 如果开启了 再判断邀请的人是否是群主或管理员 如果不是则不允许直接添加群成员
	**/
	if group.Invite == 1 {
		creatorOrManager, err := g.db.QueryIsGroupManagerOrCreator(groupNo, operator)
		if err != nil {
			g.Error("查询是否是创建者和管理者失败！", zap.Error(err))
			httperr.ResponseErrorL(c, errcode.ErrGroupQueryFailed, nil, nil)
			return
		}
		if !creatorOrManager {
			httperr.ResponseErrorL(c, errcode.ErrGroupInviteModeCannotAdd, nil, nil)
			return
		}
	}

	// Bot Space 隔离检查：如果群属于某个 Space，Bot 必须在邀请人的有效 Space 中
	// （内部成员：群的 Space；外部成员：来源 Space）
	tStep = time.Now()
	if group.SpaceID != "" {
		inviterSpaceID := group.SpaceID
		operatorMember, opErr := g.db.QueryMemberWithUID(operator, groupNo)
		if opErr != nil {
			g.Error("查询操作者群成员失败", zap.Error(opErr))
			httperr.ResponseErrorL(c, errcode.ErrGroupQueryFailed, nil, nil)
			return
		}
		if operatorMember != nil && operatorMember.IsExternal == 1 && operatorMember.SourceSpaceID != "" {
			inviterSpaceID = operatorMember.SourceSpaceID
		}
		for _, memberUID := range req.Members {
			var isBot int
			err = g.ctx.DB().SelectBySql("SELECT COALESCE((SELECT robot FROM `user` WHERE uid=? LIMIT 1), 0)", memberUID).LoadOne(&isBot)
			if err != nil {
				g.Error("查询用户robot状态失败", zap.Error(err), zap.String("memberUID", memberUID))
				httperr.ResponseErrorL(c, errcode.ErrGroupQueryFailed, nil, nil)
				return
			}
			if isBot == 1 {
				inSpace, checkErr := spacepkg.CheckMembership(g.ctx.DB(), inviterSpaceID, memberUID)
				if checkErr != nil {
					g.Error("检查Bot Space成员失败", zap.Error(checkErr))
					httperr.ResponseErrorL(c, errcode.ErrGroupQueryFailed, nil, nil)
					return
				}
				if !inSpace {
					httperr.ResponseErrorL(c, errcode.ErrGroupBotNotInSpace, nil, nil)
					return
				}
			}
		}
	}

	botSpaceMs = time.Since(tStep).Milliseconds()

	// 至此为 handler 在调 service 之前的全部写前校验耗时。
	preChecksMs = time.Since(handlerStart).Milliseconds()

	// 调用 Service 添加群成员
	_, err = g.groupService.AddGroupMembers(&AddGroupMembersServiceReq{
		GroupNo:      groupNo,
		Members:      req.Members,
		OperatorUID:  operator,
		OperatorName: operatorName,
		TraceID:      traceID,
	})
	if err != nil {
		// AddGroupMembers 会返回业务拒绝（如 allow_external=0 群里普通成员邀请
		// 外部用户）；这类是用户态 403，不能吞成内部错误。沿用 invite.go 的
		// 字符串判定（服务层 sentinel 抽取是 thread/user 同款 follow-up）。
		if strings.Contains(err.Error(), "禁止外部成员") {
			httperr.ResponseErrorL(c, errcode.ErrGroupExternalJoinForbidden, nil, nil)
			return
		}
		// 去重 / trim 后无有效成员（如 members 全是空白串）是 400 校验错误，
		// 不是存储失败。
		if strings.Contains(err.Error(), "no valid members") {
			respondGroupRequestInvalid(c, "members")
			return
		}
		// TOCTOU：getGroupInfo 之后群被解散 → 404，而非内部错误。
		if strings.Contains(err.Error(), "group not found or disbanded") {
			httperr.ResponseErrorL(c, errcode.ErrGroupNotFound, nil, nil)
			return
		}
		g.Error("添加群成员失败", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrGroupStoreFailed, nil, nil)
		return
	}

	// handler 级链路耗时（仅慢请求落日志）。与 GIN 访问日志的总耗时对照：
	//   GIN_total - handler_total ≈ 中间件耗时；
	//   handler_total - service total_ms ≈ handler 写前校验耗时（= pre_checks_ms）。
	// pre_checks_ms 再按 get_group / exist_member / bot_ownership / app_config /
	// bot_space 分摊，定位「埋点之外那 ~4.5s」到底卡在哪一步。
	if handlerTotalMs := time.Since(handlerStart).Milliseconds(); handlerTotalMs >= inviteSlowLogThresholdMS {
		g.Warn("邀请成员 handler 链路耗时偏高",
			zap.String("trace_id", traceID),
			zap.String("group_no", groupNo),
			zap.String("operator", operator),
			zap.Int("requested", len(req.Members)),
			zap.Int64("pre_checks_ms", preChecksMs),
			zap.Int64("conn_acquire_ms", connAcquireMs),
			zap.String("conn_probe_err", connProbeErr),
			zap.Int64("get_group_ms", getGroupMs),
			zap.Int64("exist_member_ms", existMemberMs),
			zap.Int64("bot_ownership_ms", botOwnerMs),
			zap.Int64("app_config_ms", appConfigMs),
			zap.Int64("bot_space_ms", botSpaceMs),
			zap.Int64("handler_total_ms", handlerTotalMs),
			zap.Int64("threshold_ms", inviteSlowLogThresholdMS),
		)
	}

	c.ResponseOK()

}

func (g *Group) addMembersTx(members []string, groupNo string, operator, operatorName string, tx *dbr.Tx) (func(), error) {
	return g.addMembersTxWithSpace(members, groupNo, operator, operatorName, "", tx)
}

// addMembersTxWithSpace 是 addMembersTx 的带 inviterSpaceID 版本。
// inviterSpaceID 来源于邀请发起请求的 X-Space-ID header（YUJ-199 / GH#1265）。
// 非空时优先作为跨 Space 邀请新成员的 source_space_id；空时沿用历史兜底逻辑
// （operator 外部 → operator.SourceSpaceID；否则 → 被邀请者 home Space）。
func (g *Group) addMembersTxWithSpace(members []string, groupNo string, operator, operatorName, inviterSpaceID string, tx *dbr.Tx) (func(), error) {

	/**
	判断操作者是否在群内，如果不在群内是不允许邀请好友的
	**/
	exist, err := g.db.ExistMember(operator, groupNo)
	if err != nil {
		g.Error("查询是否存在群内失败！", zap.Error(err))
		return nil, err
	}
	if !exist {
		return nil, errors.New("群成员不存在群里，不能添加别人！")
	}

	// Bot Ownership 校验（YUJ-46 / issue #1181）：groupMemberInviteSure 等
	// 非 Service 路径也会落到 addMembersTx，必须在此处再次拦截，确保
	// "只有 bot 的 creator 能把 bot 加入群" 这条规则无论走哪个入口都成立。
	// 此处的 operator 是原始邀请发起者（invite.inviter），与 memberAdd
	// handler 中的 operator 语义一致。
	if botErr := checkBotOwnership(g.ctx.DB(), operator, members); botErr != nil {
		if errors.Is(botErr, ErrBotOwnershipDenied) {
			return nil, ErrBotOwnershipDenied
		}
		g.Error("检查 Bot 归属失败", zap.Error(botErr))
		return nil, errors.New("检查 Bot 归属失败")
	}

	// 外部成员准入校验：与 Service.AddGroupMembers 保持一致。
	// 当群 allow_external=0 且操作者不是群主/管理员时，禁止把跨 Space 用户加入群。
	// 该检查覆盖邀请确认（groupMemberInviteSure）等非 Service 路径。
	groupModel, err := g.db.QueryWithGroupNo(groupNo)
	if err != nil {
		g.Error("查询群信息失败", zap.Error(err))
		return nil, errors.New("查询群信息失败")
	}
	// 跨 Space 外部成员标识：与 scanjoin / Service.AddGroupMembers 语义对齐。
	// 群属于某 Space 时，不在 Space 的成员标记 is_external=1 并写 source_space_id，
	// 让消息头 from_is_external / from_source_space_name 下发路径可正确渲染
	// 来源 tag（YUJ-53 bugfix）。source_space_id 规则：
	//   1) 操作者本身是外部成员 → 沿用其 source_space_id（同源 Space 邀请）
	//   2) 否则使用被邀请人的默认 Space
	externalMap := make(map[string]bool)
	sourceSpaceMap := make(map[string]string)
	var operatorMemberForSpace *MemberModel
	if groupModel != nil && groupModel.SpaceID != "" && groupModel.AllowExternal == 0 {
		operatorMember, opErr := g.db.QueryMemberWithUID(operator, groupNo)
		if opErr != nil {
			g.Error("查询操作者群成员失败", zap.Error(opErr))
			return nil, errors.New("查询操作者群成员失败")
		}
		operatorMemberForSpace = operatorMember
		operatorIsManager := operatorMember != nil &&
			(operatorMember.Role == MemberRoleCreator || operatorMember.Role == MemberRoleManager)
		if !operatorIsManager {
			for _, uid := range members {
				inSpace, spaceErr := spacepkg.CheckMembership(g.ctx.DB(), groupModel.SpaceID, uid)
				if spaceErr != nil {
					g.Error("检查 Space 成员失败", zap.Error(spaceErr))
					return nil, errors.New("检查成员关系失败")
				}
				if !inSpace {
					return nil, errors.New("该群已禁止外部成员加入，只有群主或管理员可邀请外部成员")
				}
			}
		}
	}
	if groupModel != nil && groupModel.SpaceID != "" {
		if operatorMemberForSpace == nil {
			operatorMemberForSpace, _ = g.db.QueryMemberWithUID(operator, groupNo)
		}
		// YUJ-201 / GH#1268：纵深防御——inviterSpaceID 来自 X-Space-ID header，
		// client 可以任意伪造。如果 operator 并不是该 Space 成员，就不能把它
		// 写进 source_space_id（否则外部成员会被挂到 operator 看不见的 Space）。
		// 校验失败降级为空串，让下面 switch 走 operator/home 兜底，同时 Warn 记录。
		if inviterSpaceID != "" {
			inSpace, membershipErr := spacepkg.CheckMembership(g.ctx.DB(), inviterSpaceID, operator)
			if membershipErr != nil {
				g.Error("邀请确认 X-Space-ID 成员校验失败",
					zap.String("uid", operator),
					zap.String("spaceId", inviterSpaceID),
					zap.Error(membershipErr))
				inviterSpaceID = ""
			} else if !inSpace {
				g.Warn("addmembers X-Space-ID not member, ignoring",
					zap.String("uid", operator),
					zap.String("spaceId", inviterSpaceID))
				inviterSpaceID = ""
			}
		}
		for _, uid := range members {
			inSpace, spaceErr := spacepkg.CheckMembership(g.ctx.DB(), groupModel.SpaceID, uid)
			if spaceErr != nil {
				g.Error("检查 Space 成员失败", zap.Error(spaceErr))
				return nil, errors.New("检查成员关系失败")
			}
			if inSpace {
				continue
			}
			externalMap[uid] = true
			// YUJ-199 / GH#1265：邀请确认路径同样要尊重邀请发起者**当前视角**
			// 所在的 Space（X-Space-ID header）。否则 operator 不是外部成员时
			// 会 fallback 到被邀请者 uid 的 home Space，外部成员被错误地挂到
			// 被邀请者自己的主 Space，而不是邀请发起的那个 Space。
			// 优先级：
			//   1. inviterSpaceID（caller 从 X-Space-ID header 读出）
			//   2. operator 自身外部成员的 SourceSpaceID
			//   3. 被邀请者的 home Space（兜底，保持历史行为）
			// source_space_id 规则与 Service.AddGroupMembers 保持一致。
			switch {
			case inviterSpaceID != "":
				sourceSpaceMap[uid] = inviterSpaceID
			case operatorMemberForSpace != nil && operatorMemberForSpace.IsExternal == 1 && operatorMemberForSpace.SourceSpaceID != "":
				sourceSpaceMap[uid] = operatorMemberForSpace.SourceSpaceID
			default:
				sourceSpaceMap[uid] = spacemod.GetUserDefaultSpaceID(g.ctx, uid)
			}
		}
	}

	/**
	 获取到真实有效的成员信息
	**/
	tempNewMembers := util.RemoveRepeatedElement(members)
	// 查询用户是否已注销
	userList, err := g.userDB.QueryByUIDs(tempNewMembers)
	if err != nil {
		g.Error("查询添加成员信息错误", zap.Error(err))
		return nil, errors.New("查询添加成员信息错误")
	}
	newMembers := make([]string, 0)
	unableAddMemberVos := make([]*config.UserBaseVo, 0)
	if len(userList) > 0 {
		for _, u := range userList {
			if u.IsDestroy == user.IsDestroyDone {
				unableAddMemberVos = append(unableAddMemberVos, &config.UserBaseVo{
					UID:  u.UID,
					Name: u.Name,
				})
			} else {
				newMembers = append(newMembers, u.UID)
			}
		}
	}
	// 如果添加的成员全都已注销则不执行添加到群逻辑
	if len(unableAddMemberVos) == len(tempNewMembers) {
		g.Error("添加用户已注销无法加入群聊", zap.Error(err))
		return nil, errors.New("添加用户已注销无法加入群聊")
	}

	existMembers, err := g.db.QueryMembersWithUids(newMembers, groupNo)
	if err != nil {
		g.Error("查询已在群内存在的成员失败！", zap.Error(err))
		return nil, errors.New("查询已在群内存在的成员失败！")
	}
	// 查询群内黑名单成员
	blacklist, err := g.db.QueryMembersWithStatus(groupNo, int(common.GroupMemberStatusBlacklist))
	if err != nil {
		g.Error("查询群黑名单成员错误", zap.Error(err))
		return nil, errors.New("查询群黑名单成员错误")
	}
	realMembers := make([]string, 0, len(newMembers)) // 真正要添加的群成员
	for _, memberUID := range newMembers {
		exist := false
		for _, existMember := range existMembers {
			if memberUID == existMember.UID {
				exist = true
				break
			}
		}
		if len(blacklist) > 0 {
			for _, blacklistMember := range blacklist {
				if memberUID == blacklistMember.UID {
					exist = true
					break
				}
			}
		}
		if !exist {
			realMembers = append(realMembers, memberUID)
		}
	}
	if len(realMembers) == 0 {
		g.Error("添加的成员已在群内或在群黑名单内", zap.Error(err))
		return nil, errors.New("添加的成员已在群内或在群黑名单内")
	}
	realMemberModels, err := g.userDB.QueryByUIDs(realMembers)
	if err != nil {
		g.Error("查询成员用户信息失败！", zap.Error(err))
		return nil, errors.New("查询成员用户信息失败！")
	}
	/**
	 将成员信息存到数据库
	**/
	userBaseVos := make([]*config.UserBaseVo, 0, len(realMembers))
	hasNewExternal := false
	for _, realMember := range realMemberModels {
		version, err := g.ctx.GenSeq(common.GroupMemberSeqKey)
		if err != nil {
			g.Error("GenSeq failed", zap.Error(err))
			return nil, err
		}

		userBaseVos = append(userBaseVos, &config.UserBaseVo{
			UID:  realMember.UID,
			Name: realMember.Name,
		})
		existDelete, err := g.db.ExistMemberDelete(realMember.UID, groupNo)
		if err != nil {
			g.Error("查询是否存在删除成员失败！", zap.Error(err))
			return nil, errors.New("查询是否存在删除成员失败！")
		}
		// 跨 Space 外部成员：写入 is_external=1 和 source_space_id（YUJ-53）。
		isExt := 0
		srcSpaceID := ""
		if externalMap[realMember.UID] {
			isExt = 1
			srcSpaceID = sourceSpaceMap[realMember.UID]
		}
		newMember := &MemberModel{
			GroupNo:       groupNo,
			InviteUID:     operator,
			UID:           realMember.UID,
			Vercode:       fmt.Sprintf("%s@%d", util.GenerUUID(), common.GroupMember),
			Version:       version,
			Status:        int(common.GroupMemberStatusNormal),
			Robot:         realMember.Robot,
			IsExternal:    isExt,
			SourceSpaceID: srcSpaceID,
		}
		if existDelete {
			err = g.db.recoverMemberTx(newMember, tx)
		} else {
			err = g.db.InsertMemberTx(newMember, tx)
		}
		if err != nil {
			g.Error("添加群成员失败！", zap.Error(err))
			return nil, errors.New("添加群成员失败！")
		}
		// is_external_group 只反映人类外部成员：bot 即便 is_external=1 也不应
		// flip 群标记（与 DELETE 路径 robot=0 过滤对称）。
		if isExt == 1 && realMember.Robot == 0 {
			hasNewExternal = true
		}
	}

	// 首次出现外部人类成员时，在事务内将群标记为外部群。
	markedExternal := false
	if hasNewExternal && groupModel != nil && groupModel.IsExternalGroup == 0 {
		if updateErr := g.db.UpdateIsExternalGroupTx(groupNo, 1, tx); updateErr != nil {
			g.Error("更新 is_external_group 失败", zap.Error(updateErr), zap.String("group_no", groupNo))
			return nil, errors.New("更新 is_external_group 失败")
		}
		markedExternal = true
	}

	/**
	发布群成员添加事件
		**/
	eventID, err := g.ctx.EventBegin(&wkevent.Data{
		Event: event.GroupMemberAdd,
		Type:  wkevent.Message,
		Data: &config.MsgGroupMemberAddReq{
			GroupNo:      groupNo,
			Operator:     operator,
			OperatorName: operatorName,
			Members:      userBaseVos,
		},
	}, tx)
	if err != nil {
		g.Error("开启事件失败！", zap.Error(err))
		return nil, errors.New("开启事件失败！")
	}
	var unableAddDestroyAccount int64 = 0
	if len(unableAddMemberVos) > 0 {
		// 发布无法添加到群聊用户
		unableAddDestroyAccount, err = g.ctx.EventBegin(&wkevent.Data{
			Event: event.GroupUnableAddDestroyAccount,
			Type:  wkevent.Message,
			Data: &config.MsgGroupCreateReq{
				GroupNo: groupNo,
				Members: unableAddMemberVos,
			},
		}, tx)
		if err != nil {
			g.Error("开启无法添加到群聊事件失败！", zap.Error(err))
			return nil, errors.New("开启无法添加到群聊事件失败！")
		}
	}
	// 调用IM的添加订阅者
	err = g.ctx.IMAddSubscriber(&config.SubscriberAddReq{
		ChannelID:   groupNo,
		ChannelType: common.ChannelTypeGroup.Uint8(),
		Subscribers: realMembers,
	})
	if err != nil {
		g.Error("调用IM的订阅接口失败！", zap.Error(err))
		return nil, errors.New("调用IM的订阅接口失败！")
	}

	// 检查新增成员中是否有Bot用户，推送 bot_joined_group 事件
	botMembers := make([]*user.Model, 0)
	for _, realMember := range realMemberModels {
		if realMember.Robot == 1 {
			botMembers = append(botMembers, realMember)
		}
	}
	if len(botMembers) > 0 {
		go g.notifyBotJoinedGroup(botMembers, groupNo, operator, operatorName)
	}

	return func() {
		// 提交事件
		g.ctx.EventCommit(eventID)
		if unableAddDestroyAccount != 0 {
			g.ctx.EventCommit(unableAddDestroyAccount)
		}
		// 群首次被 flip 为外部群：通知端上刷新群资料，与 scanjoin /
		// Service.AddGroupMembers 保持一致。
		if markedExternal {
			g.ctx.SendChannelUpdateToGroup(groupNo)
		}
	}, nil
}

func (g *Group) addMembers(members []string, groupNo string, operator, operatorName string) error {
	tx, err := g.ctx.DB().Begin()
	if err != nil {
		g.Error("开启事务失败！", zap.Error(err))
		return errors.New("开启事务失败！")
	}
	defer func() {
		if err := recover(); err != nil {
			tx.Rollback()
			fmt.Fprintf(os.Stderr, "recovered panic in goroutine: %v\n%s\n", err, debug.Stack())
		}
	}()
	commitCallback, err := g.addMembersTx(members, groupNo, operator, operatorName, tx)
	if err != nil {
		tx.RollbackUnlessCommitted()
		return err
	}
	if err := tx.Commit(); err != nil {
		tx.Rollback()
		g.Error("提交事务失败！", zap.Error(err))
		return errors.New("提交事务失败！")
	}
	if commitCallback != nil {
		commitCallback()
	}

	// 同步新成员到群内所有子区的 IM 订阅（允许发消息）
	g.addUsersToGroupThreads(groupNo, members)

	return nil
}

// notifyBotJoinedGroup 向Bot的事件队列推送 bot_joined_group 事件
func (g *Group) notifyBotJoinedGroup(botMembers []*user.Model, groupNo, operator, operatorName string) {
	for _, botMember := range botMembers {
		robotID := botMember.UID
		seq, err := g.ctx.GenSeq(fmt.Sprintf("%s%s", common.RobotEventSeqKey, robotID))
		if err != nil {
			g.Warn("GenSeq failed for bot", zap.String("robotID", robotID), zap.Error(err))
			continue
		}
		eventData := map[string]interface{}{
			"event_id":   seq,
			"event_type": "bot_joined_group",
			"event_data": map[string]interface{}{
				"group_no":      groupNo,
				"operator":      operator,
				"operator_name": operatorName,
			},
			"expire": time.Now().Add(time.Hour * 24).Unix(),
		}
		key := fmt.Sprintf("robotEvent:%s", robotID)
		err = g.ctx.GetRedisConn().ZAdd(key, float64(seq), util.ToJson(eventData))
		if err != nil {
			g.Error("推送bot_joined_group事件失败！", zap.Error(err), zap.String("robotID", robotID), zap.String("groupNo", groupNo))
			continue
		}
		g.Info("已推送bot_joined_group事件", zap.String("robotID", robotID), zap.String("groupNo", groupNo))
	}
}

// 添加管理员
func (g *Group) managerAdd(c *wkhttp.Context) {
	loginUID := c.MustGet("uid").(string)
	var memberUIDs []string
	if err := c.BindJSON(&memberUIDs); err != nil {
		g.Error("数据格式有误！", zap.Error(err))
		respondGroupRequestInvalid(c, "")
		return
	}
	if len(memberUIDs) <= 0 {
		respondGroupRequestInvalid(c, "members")
		return
	}
	for _, memberUID := range memberUIDs {
		if memberUID == loginUID {
			httperr.ResponseErrorL(c, errcode.ErrGroupCannotTargetSelf, nil, nil)
			return
		}
	}
	groupNo := c.Param("group_no")
	isCreator, err := g.db.QueryIsGroupCreator(groupNo, loginUID)
	if err != nil {
		g.Error("查询是否是创建者失败！", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrGroupQueryFailed, nil, nil)
		return
	}
	if !isCreator {
		httperr.ResponseErrorL(c, errcode.ErrGroupCreatorOnly, nil, nil)
		return
	}

	groupModel, err := g.getGroupInfo(groupNo)
	if err != nil {
		respondGroupInfoError(c, err)
		return
	}

	version, err := g.ctx.GenSeq(common.GroupMemberSeqKey)
	if err != nil {
		g.Error("生成序列号失败", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrGroupStoreFailed, nil, nil)
		return
	}

	// Verify all target UIDs are current group members before promoting.
	// 同时拦截外部成员 (is_external=1)：外部成员不得被提拔为管理员，
	// 否则会获得 groupForbidden / memberRemove / managerAdd/Remove /
	// blacklist / transferGrouper 等 8 项敏感操作权限
	// (YUJ-231 / GH#1289，ReviewBot YUJ-230 P1)。
	for _, uid := range memberUIDs {
		targetMember, err := g.db.QueryMemberWithUID(uid, groupNo)
		if err != nil {
			g.Error("查询群成员关系失败", zap.String("uid", uid), zap.Error(err))
			httperr.ResponseErrorL(c, errcode.ErrGroupQueryFailed, nil, nil)
			return
		}
		if targetMember == nil {
			httperr.ResponseErrorL(c, errcode.ErrGroupMemberNotInGroup, nil, nil)
			return
		}
		if targetMember.IsExternal == 1 {
			g.Warn("拒绝将外部成员提拔为管理员",
				zap.String("group_no", groupNo),
				zap.String("target_uid", uid),
				zap.String("operator", loginUID))
			httperr.ResponseErrorL(c, errcode.ErrGroupExternalCannotBeAdmin, nil, nil)
			return
		}
	}

	err = g.db.UpdateMembersToManager(groupNo, memberUIDs, version)
	if err != nil {
		g.Error("更新成员为管理员失败！", zap.Any("memberUIDs", memberUIDs), zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrGroupStoreFailed, nil, nil)
		return
	}

	if groupModel.Forbidden == 1 { // 如果是禁言状态，则重置管理员白名单
		err = g.setIMWhitelistForGroupManager(groupModel.GroupNo)
		if err != nil {
			httperr.ResponseErrorL(c, errcode.ErrGroupStoreFailed, nil, nil)
			g.Error("设置白名单失败！", zap.Error(err))
			return
		}
	}
	if groupModel.GroupType == int(GroupTypeCommon) {
		err = g.ctx.SendCMD(config.MsgCMDReq{
			ChannelID:   groupNo,
			ChannelType: common.ChannelTypeGroup.Uint8(),
			CMD:         common.CMDGroupMemberUpdate,
			Param: map[string]interface{}{
				"group_no": groupNo,
			},
		})
		if err != nil {
			g.Error("发送命令消息失败！", zap.Error(err))
			httperr.ResponseErrorL(c, errcode.ErrGroupNotifyFailed, nil, nil)
			return
		}
	} else {
		for _, uid := range memberUIDs {
			err = g.ctx.SendCMD(config.MsgCMDReq{
				ChannelID:   groupNo,
				ChannelType: common.ChannelTypeGroup.Uint8(),
				CMD:         common.CMDGroupMemberUpdate,
				Param: map[string]interface{}{
					"group_no": groupNo,
					"uid":      uid,
				},
			})
			if err != nil {
				g.Error("发送命令消息失败！", zap.Error(err))
				httperr.ResponseErrorL(c, errcode.ErrGroupNotifyFailed, nil, nil)
				return
			}
		}
	}
	c.ResponseOK()
}

// 移除管理员
func (g *Group) managerRemove(c *wkhttp.Context) {
	loginUID := c.MustGet("uid").(string)
	var memberUIDs []string
	if err := c.BindJSON(&memberUIDs); err != nil {
		g.Error("数据格式有误！", zap.Error(err))
		respondGroupRequestInvalid(c, "")
		return
	}
	if len(memberUIDs) <= 0 {
		respondGroupRequestInvalid(c, "members")
		return
	}
	for _, memberUID := range memberUIDs {
		if memberUID == loginUID {
			httperr.ResponseErrorL(c, errcode.ErrGroupCannotTargetSelf, nil, nil)
			return
		}
	}
	groupNo := c.Param("group_no")

	isCreator, err := g.db.QueryIsGroupCreator(groupNo, loginUID)
	if err != nil {
		g.Error("查询是否是创建者失败！", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrGroupQueryFailed, nil, nil)
		return
	}
	if !isCreator {
		httperr.ResponseErrorL(c, errcode.ErrGroupCreatorOnly, nil, nil)
		return
	}

	groupModel, err := g.getGroupInfo(groupNo)
	if err != nil {
		respondGroupInfoError(c, err)
		return
	}

	version, err := g.ctx.GenSeq(common.GroupMemberSeqKey)
	if err != nil {
		g.Error("生成序列号失败", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrGroupStoreFailed, nil, nil)
		return
	}

	err = g.db.UpdateManagersToMember(groupNo, memberUIDs, version)
	if err != nil {
		g.Error("更新成员为管理员失败！", zap.Any("memberUIDs", memberUIDs), zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrGroupStoreFailed, nil, nil)
		return
	}

	if groupModel.Forbidden == 1 { // 如果是禁言状态，则重置管理员白名单
		err = g.setIMWhitelistForGroupManager(groupModel.GroupNo)
		if err != nil {
			httperr.ResponseErrorL(c, errcode.ErrGroupStoreFailed, nil, nil)
			g.Error("设置白名单失败！", zap.Error(err))
			return
		}
	}
	if groupModel.GroupType == int(GroupTypeCommon) {

		err = g.ctx.SendCMD(config.MsgCMDReq{
			ChannelID:   groupNo,
			ChannelType: common.ChannelTypeGroup.Uint8(),
			CMD:         common.CMDGroupMemberUpdate,
			Param: map[string]interface{}{
				"group_no": groupNo,
			},
		})
		if err != nil {
			g.Error("发送命令消息失败！", zap.Error(err))
			httperr.ResponseErrorL(c, errcode.ErrGroupNotifyFailed, nil, nil)
			return
		}
	} else {
		for _, uid := range memberUIDs {
			err = g.ctx.SendCMD(config.MsgCMDReq{
				ChannelID:   groupNo,
				ChannelType: common.ChannelTypeGroup.Uint8(),
				CMD:         common.CMDGroupMemberUpdate,
				Param: map[string]interface{}{
					"group_no": groupNo,
					"uid":      uid,
				},
			})
			if err != nil {
				g.Error("发送命令消息失败！", zap.Error(err))
				httperr.ResponseErrorL(c, errcode.ErrGroupNotifyFailed, nil, nil)
				return
			}
		}
	}
	c.ResponseOK()
}

// 群全员禁言
func (g *Group) groupForbidden(c *wkhttp.Context) {
	loginUID := c.MustGet("uid").(string)
	loginName := c.MustGet("name").(string)
	groupNo := c.Param("group_no")
	on := c.Param("on")
	isCreatorOrManager, err := g.db.QueryIsGroupManagerOrCreator(groupNo, loginUID)
	if err != nil {
		g.Error("查询是否是创建者失败！", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrGroupQueryFailed, nil, nil)
		return
	}
	if !isCreatorOrManager {
		httperr.ResponseErrorL(c, errcode.ErrGroupCreatorOrManagerOnly, nil, nil)
		return
	}
	groupModel, err := g.getGroupInfo(groupNo)
	if err != nil {
		respondGroupInfoError(c, err)
		return
	}
	forbidden, _ := strconv.ParseInt(on, 10, 64)
	groupModel.Forbidden = int(forbidden)

	whitelistUIDs := make([]string, 0)
	if forbidden == 1 {
		managerOrCreaterUIDs, err := g.db.QueryGroupManagerOrCreatorUIDS(groupNo)
		if err != nil {
			g.Error("查询管理者们的uid失败！", zap.Error(err))
			httperr.ResponseErrorL(c, errcode.ErrGroupQueryFailed, nil, nil)
			return
		}
		whitelistUIDs = managerOrCreaterUIDs
	}
	// 重置白名单
	err = g.resetIMWhitelist(whitelistUIDs, groupNo)

	if err != nil {
		g.Error("设置禁言失败！", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrGroupStoreFailed, nil, nil)
		return
	}

	tx, err := g.ctx.DB().Begin()
	if err != nil {
		g.Error("开启事务失败！", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrGroupStoreFailed, nil, nil)
		return
	}
	defer func() {
		if err := recover(); err != nil {
			tx.RollbackUnlessCommitted()
			fmt.Fprintf(os.Stderr, "recovered panic in goroutine: %v\n%s\n", err, debug.Stack())
		}
	}()

	err = g.db.UpdateTx(groupModel, tx)
	if err != nil {
		tx.Rollback()
		g.Error("更新群信息失败！", zap.Error(err), zap.String("group_no", groupModel.GroupNo))
		httperr.ResponseErrorL(c, errcode.ErrGroupStoreFailed, nil, nil)
		return
	}
	// 发布群信息更新事件
	eventID, err := g.ctx.EventBegin(&wkevent.Data{
		Event: event.GroupUpdate,
		Type:  wkevent.Message,
		Data: &config.MsgGroupUpdateReq{
			GroupNo:      groupNo,
			Operator:     loginUID,
			OperatorName: loginName,
			Attr:         common.GroupAttrKeyForbidden,
			Data: map[string]string{
				common.GroupAttrKeyForbidden: on,
			},
		},
	}, tx)
	if err != nil {
		tx.Rollback()
		g.Error("开启群更新事件失败！", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrGroupStoreFailed, nil, nil)
		return
	}
	if err := tx.Commit(); err != nil {
		tx.RollbackUnlessCommitted()
		g.Error("提交事务失败！", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrGroupStoreFailed, nil, nil)
		return
	}
	g.ctx.EventCommit(eventID)

	c.ResponseOK()
}

// 设置群管理员（包含创建者）列表作为群白名单
func (g *Group) setIMWhitelistForGroupManager(groupNo string) error {
	managerOrCreaterUIDs, err := g.db.QueryGroupManagerOrCreatorUIDS(groupNo)
	if err != nil {
		return err
	}
	return g.resetIMWhitelist(managerOrCreaterUIDs, groupNo)
}

// 重新设置群管理的白名单
func (g *Group) resetIMWhitelist(whitelist []string, groupNo string) error {
	// 群全员禁言
	err := g.ctx.IMWhitelistSet(config.ChannelWhitelistReq{
		ChannelReq: config.ChannelReq{
			ChannelID:   groupNo,
			ChannelType: common.ChannelTypeGroup.Uint8(),
		},
		UIDs: whitelist,
	})
	if err != nil {
		g.Error("设置白名单失败！", zap.Error(err))
		return err
	}
	return nil

}

// 获取群二维码信息
func (g *Group) groupQRCode(c *wkhttp.Context) {
	loginUID := c.MustGet("uid").(string)
	groupNo := c.Param("group_no")
	_, err := g.getGroupInfo(groupNo)
	if err != nil {
		respondGroupInfoError(c, err)
		return
	}
	exist, err := g.db.ExistMember(loginUID, groupNo)
	if err != nil {
		g.Error("查询是否存在群内失败！", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrGroupQueryFailed, nil, nil)
		return
	}
	if !exist {
		httperr.ResponseErrorL(c, errcode.ErrGroupQRCodeMemberOnly, nil, nil)
		return
	}

	uuid := util.GenerUUID()
	err = g.ctx.GetRedisConn().SetAndExpire(fmt.Sprintf("%s%s", common.QRCodeCachePrefix, uuid), util.ToJson(common.NewQRCodeModel(common.QRCodeTypeGroup, map[string]interface{}{
		"group_no":  groupNo,
		"generator": loginUID, // 生成者
	})), time.Hour*24*7)
	if err != nil {
		g.Error("设置缓存失败！", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrGroupStoreFailed, nil, nil)
		return
	}
	baseURL := g.ctx.GetConfig().External.BaseURL
	c.Response(gin.H{
		"day":    7,
		"qrcode": fmt.Sprintf("%s/%s", baseURL, strings.ReplaceAll(g.ctx.GetConfig().QRCodeInfoURL, ":code", uuid)),
		// invite_url 是浏览器友好的公开落地页（YUJ-31），App 仍走 qrcode 字段。
		// Web "复制邀请链接" 按钮（YUJ-30）应当使用此字段。
		"invite_url": fmt.Sprintf("%s/v1/group/invite?code=%s", baseURL, uuid),
		"expire":     time.Now().Add(time.Hour * 24 * 7).Format("01月02日"),
	})

}

// 加入群
func (g *Group) groupScanJoin(c *wkhttp.Context) {
	loginUID := c.GetLoginUID()
	if loginUID == "" {
		respondGroupNotLoggedIn(c)
		return
	}
	// GH #1319 / Direction A：零 Space 用户禁止入群。
	// 放在 loginUID 校验之后、auth_code / 群资料查询之前，避免写出脏
	// group_member{is_external=1, source_space_id=""} 记录（下游
	// space_filter 会把该记录在任何 Space 视角隐藏，造成消息到账但 UI 看不见）。
	// 三端（Web / iOS / Android）收到该 status 后应拉起 SpaceGate/JoinSpacePage，
	// Gate 完成后用当前 auth_code 重试本接口。auth_code 30min TTL 足够走一趟 Gate。
	if spacemod.GetUserDefaultSpaceID(g.ctx, loginUID) == "" {
		c.Response(gin.H{
			"status": groupInviteStatusNeedSpace,
			"msg":    needSpaceMsg,
		})
		return
	}
	authCode := c.Query("auth_code")
	groupNo := c.Param("group_no")
	if groupNo == "" {
		respondGroupRequestInvalid(c, "group_no")
		return
	}
	group, err := g.getGroupInfo(groupNo)
	if err != nil {
		respondGroupInfoError(c, err)
		return
	}
	if group.Invite == 1 {
		httperr.ResponseErrorL(c, errcode.ErrGroupInviteModeCannotJoin, nil, nil)
		return
	}
	authInfo, err := g.ctx.GetRedisConn().GetString(fmt.Sprintf("%s%s", common.AuthCodeCachePrefix, authCode))
	if err != nil {
		g.Error("获取认证信息数据失败！", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrGroupQueryFailed, nil, nil)
		return
	}
	if authInfo == "" {
		httperr.ResponseErrorL(c, errcode.ErrGroupAuthCodeInvalid, nil, nil)
		return
	}
	var authMap map[string]interface{}
	err = util.ReadJsonByByte([]byte(authInfo), &authMap)
	if err != nil {
		g.Error("解码认证信息的JSON数据失败！", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrGroupStoreFailed, nil, nil)
		return
	}
	authType, ok := authMap["type"].(string)
	if !ok {
		httperr.ResponseErrorL(c, errcode.ErrGroupAuthCodeInvalid, nil, nil)
		return
	}
	if authType != string(common.AuthCodeTypeJoinGroup) {
		httperr.ResponseErrorL(c, errcode.ErrGroupAuthCodeInvalid, nil, nil)
		return
	}
	authGroupNo, ok := authMap["group_no"].(string)
	if !ok {
		httperr.ResponseErrorL(c, errcode.ErrGroupAuthCodeInvalid, nil, nil)
		return
	}
	if authGroupNo != groupNo {
		httperr.ResponseErrorL(c, errcode.ErrGroupAuthCodeInvalid, nil, nil)
		return
	}
	generator, ok := authMap["generator"].(string)
	if !ok {
		httperr.ResponseErrorL(c, errcode.ErrGroupAuthCodeInvalid, nil, nil)
		return
	}
	if strings.TrimSpace(generator) == "" {
		httperr.ResponseErrorL(c, errcode.ErrGroupAuthCodeInvalid, nil, nil)
		return
	}
	scaner, ok := authMap["scaner"].(string)
	if !ok {
		httperr.ResponseErrorL(c, errcode.ErrGroupAuthCodeInvalid, nil, nil)
		return
	}
	if strings.TrimSpace(scaner) == "" {
		httperr.ResponseErrorL(c, errcode.ErrGroupAuthCodeInvalid, nil, nil)
		return
	}
	if scaner != loginUID {
		httperr.ResponseErrorL(c, errcode.ErrGroupAuthCodeUserMismatch, nil, nil)
		return
	}
	existMember, err := g.db.ExistMember(scaner, groupNo)
	if err != nil {
		g.Error("查询是否存在群内时失败！", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrGroupQueryFailed, nil, nil)
		return
	}
	if existMember {
		httperr.ResponseErrorL(c, errcode.ErrGroupAlreadyMember, nil, nil)
		return
	}
	// 查询生成二维码信息
	generatorInfo, err := g.userDB.QueryByUID(generator)
	if err != nil {
		g.Error("获取生成二维码的用户信息失败！", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrGroupQueryFailed, nil, nil)
		return
	}
	if generatorInfo == nil {
		httperr.ResponseErrorL(c, errcode.ErrGroupAuthCodeInvalid, nil, nil)
		return
	}
	// 查询扫码者用户信息
	scanerInfo, err := g.userDB.QueryByUID(scaner)
	if err != nil {
		g.Error("查询扫码者用户信息失败！", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrGroupQueryFailed, nil, nil)
		return
	}
	if scanerInfo == nil {
		httperr.ResponseErrorL(c, errcode.ErrGroupAuthCodeInvalid, nil, nil)
		return
	}

	version, err := g.ctx.GenSeq(common.GroupMemberSeqKey)
	if err != nil {
		g.Error("生成序列号失败", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrGroupStoreFailed, nil, nil)
		return
	}

	// 外部成员检测：群属于某个 Space 且扫码者不在该 Space 时，标记为外部成员
	isExternal := 0
	sourceSpaceID := ""
	if group.SpaceID != "" {
		inSpace, checkErr := spacepkg.CheckMembership(g.ctx.DB(), group.SpaceID, scaner)
		if checkErr != nil {
			g.Error("检查 Space 成员失败", zap.Error(checkErr))
			httperr.ResponseErrorL(c, errcode.ErrGroupQueryFailed, nil, nil)
			return
		}
		if !inSpace {
			// 当群禁止外部成员加入时，拒绝跨 Space 扫码入群
			if group.AllowExternal == 0 {
				httperr.ResponseErrorL(c, errcode.ErrGroupExternalJoinForbidden, nil, nil)
				return
			}
			isExternal = 1
			// YUJ-199 / GH#1265：source_space_id 必须反映扫码者**当前视角**
			// 所在的 Space（三端拦截器注入的 X-Space-ID header），而不是
			// scaner 的 home Space。否则用户 A 在测试空间建群，B 在华山派
			// 扫码入群后，群会错落在 B 的 home（ExampleCorp）视图。
			// 三端（Android/iOS/Web React）的 header 拦截器（YUJ-88/GH#1038/EP3）
			// 早就把 X-Space-ID 注入在每个请求上，这里只需读取 + 兜底 home。
			//
			// YUJ-201 / GH#1268：纵深防御——client 可以任意伪造 X-Space-ID
			// header，如果 scaner 并不是那个 Space 的成员，就不能用它作为
			// source_space_id（否则把群错落到 scaner 看不见的外部 Space）。
			// 校验失败时降级成空串走 home 兜底，并 Warn 记一笔方便排查。
			if headerSpaceID := strings.TrimSpace(c.GetHeader("X-Space-ID")); headerSpaceID != "" {
				sourceSpaceID = headerSpaceID
				inSpace, membershipErr := spacepkg.CheckMembership(g.ctx.DB(), headerSpaceID, scaner)
				if membershipErr != nil {
					g.Error("扫码入群 X-Space-ID 成员校验失败",
						zap.String("uid", scaner),
						zap.String("spaceId", headerSpaceID),
						zap.Error(membershipErr))
					sourceSpaceID = "" // 降级到 home Space，不阻断主流程
				} else if !inSpace {
					g.Warn("scanjoin X-Space-ID not member, ignoring",
						zap.String("uid", scaner),
						zap.String("spaceId", headerSpaceID))
					sourceSpaceID = "" // 降级，不写脏数据
				}
			}
			if sourceSpaceID == "" {
				sourceSpaceID = spacemod.GetUserDefaultSpaceID(g.ctx, scaner)
			}
		}
	}

	memberModel := &MemberModel{
		GroupNo:   groupNo,
		UID:       scaner,
		Role:      MemberRoleCommon,
		Version:   version,
		Status:    int(common.GroupMemberStatusNormal),
		InviteUID: generator,
		Vercode:   fmt.Sprintf("%s@%d", util.GenerUUID(), common.GroupMember),
		// 保留 scaner 的 robot 标记，与其它入群路径保持一致，
		// 让 DELETE 路径的 QueryExternalMemberCountTx(robot=0) 能正确排除 bot。
		Robot:         scanerInfo.Robot,
		IsExternal:    isExternal,
		SourceSpaceID: sourceSpaceID,
	}

	tx, err := g.db.session.Begin()
	if err != nil {
		g.Error("开启事务失败！", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrGroupStoreFailed, nil, nil)
		return
	}
	defer func() {
		if err := recover(); err != nil {
			tx.RollbackUnlessCommitted()
			fmt.Fprintf(os.Stderr, "recovered panic in goroutine: %v\n%s\n", err, debug.Stack())
		}
	}()
	eventID, err := g.ctx.EventBegin(&wkevent.Data{
		Event: event.GroupMemberScanJoin,
		Type:  wkevent.Message,
		Data: MsgGroupMemberScanJoinExt{
			MsgGroupMemberScanJoin: config.MsgGroupMemberScanJoin{
				GroupNo:       groupNo,
				Generator:     generatorInfo.UID,
				GeneratorName: generatorInfo.Name,
				Scaner:        scanerInfo.UID,
				ScanerName:    scanerInfo.Name,
			},
			IsExternal: isExternal,
		},
	}, tx)
	if err != nil {
		tx.Rollback()
		g.Error("开启事件事务失败！", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrGroupStoreFailed, nil, nil)
		return
	}
	existDelete, err := g.db.ExistMemberDelete(scaner, groupNo)
	if err != nil {
		tx.Rollback()
		g.Error("查询是否存在删除成员失败！", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrGroupQueryFailed, nil, nil)
		return
	}
	if existDelete {
		err = g.db.recoverMemberTx(memberModel, tx)
	} else {
		err = g.db.InsertMemberTx(memberModel, tx)
	}
	if err != nil {
		tx.Rollback()
		g.Error("添加群成员失败！", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrGroupStoreFailed, nil, nil)
		return
	}

	// 首个外部成员加入时在同一事务内将群标记为外部群，确保成员/群标记一致提交。
	// is_external_group 语义只反映人类外部成员：bot 扫码入群（即便 is_external=1）
	// 不应 flip 群成外部群，保持与 DELETE 路径 QueryExternalMemberCountTx(robot=0)
	// 以及批量 ADD 路径 (service.go) 的语义对称。
	// 详见 YUJ-48 / Mininglamp-OSS/octo-server#1184。
	markedExternal := false
	if isExternal == 1 && scanerInfo.Robot == 0 && group.IsExternalGroup == 0 {
		if updateErr := g.db.UpdateIsExternalGroupTx(groupNo, 1, tx); updateErr != nil {
			tx.Rollback()
			g.Error("更新 is_external_group 失败", zap.Error(updateErr), zap.String("group_no", groupNo))
			httperr.ResponseErrorL(c, errcode.ErrGroupStoreFailed, nil, nil)
			return
		}
		markedExternal = true
	}

	if err := tx.Commit(); err != nil {
		tx.Rollback()
		g.Error("提交事务失败！", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrGroupStoreFailed, nil, nil)
		return
	}

	if markedExternal {
		g.ctx.SendChannelUpdateToGroup(groupNo)
	}

	// 调用IM的添加订阅者（在事务提交后执行，确保数据一致性）
	err = g.ctx.IMAddSubscriber(&config.SubscriberAddReq{
		ChannelID:   groupNo,
		ChannelType: common.ChannelTypeGroup.Uint8(),
		Subscribers: []string{scaner},
	})
	if err != nil {
		// IM 调用失败时记录日志，但不影响已提交的数据库事务
		// 后续可通过数据同步机制修复 IM 订阅状态
		g.Error("调用IM的订阅接口失败！", zap.Error(err), zap.String("group_no", groupNo), zap.String("scaner", scaner))
		httperr.ResponseErrorL(c, errcode.ErrGroupNotifyFailed, nil, nil)
		return
	}

	// 同步新成员到群内所有子区的 IM 订阅（允许发消息）
	g.addUsersToGroupThreads(groupNo, []string{scaner})

	g.ctx.EventCommit(eventID)

	// YUJ-170 / dmwork-web#1100：scanjoin 成功响应直接回带群所属 Space 信息。
	// 替换原 ResponseOK() 的空载，为 H5 join_group.html 提供 crossSpace 判定数据，
	// 避免再次调 /detail（非成员首次会 403；成员二次调虽 200 但多了一次往返）。
	// - space_id：群所属 Space ID，H5 与 localStorage.currentSpaceId 比对判断是否跨 Space
	// - space_name：展示用（"位于 xxx 空间"），私群为空串
	// - group_no / group_name：冗余便于 H5 写 sessionStorage payload 无需依赖全局变量
	// 私群（SpaceID=""）/ GetSpaceName 失败 → 空串降级，H5 据空串跳过 notice 写入。
	spaceName, spaceNameErr := spacepkg.GetSpaceName(g.ctx.DB(), group.SpaceID)
	if spaceNameErr != nil {
		g.Warn("scanjoin 成功但查询 Space 名称失败（降级为空串，不阻塞入群）",
			zap.String("space_id", group.SpaceID),
			zap.String("group_no", groupNo),
			zap.Error(spaceNameErr))
		spaceName = ""
	}
	c.Response(gin.H{
		"status":     http.StatusOK,
		"group_no":   groupNo,
		"group_name": group.Name,
		"space_id":   group.SpaceID,
		"space_name": spaceName,
	})
}

// 群主转让
func (g *Group) transferGrouper(c *wkhttp.Context) {
	loginUID := c.MustGet("uid").(string)
	loginName := c.MustGet("name").(string)
	toUID := c.Param("to_uid")
	groupNo := c.Param("group_no")

	/**
	查询转让者用户信息
	**/
	toUser, err := g.userDB.QueryByUID(toUID)
	if err != nil {
		g.Error("查询转让用户失败！", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrGroupQueryFailed, nil, nil)
		return
	}
	if toUser == nil || toUser.IsDestroy == user.IsDestroyDone {
		httperr.ResponseErrorL(c, errcode.ErrGroupTransferTargetNotFound, nil, nil)
		return
	}

	/**
	判断转让的用户是否在群内,只有在群内才能转让
	**/
	// exist, err := g.db.ExistMember(toUID, groupNo)
	// if err != nil {
	// 	g.Error("查询是否存在成员失败！", zap.Error(err))
	// 	httperr.ResponseErrorL(c, errcode.ErrGroupQueryFailed, nil, nil)
	// 	return
	// }
	// if !exist {
	// 	httperr.ResponseErrorL(c, errcode.ErrGroupMemberNotInGroup, nil, nil)
	// 	return
	// }
	toMember, err := g.db.QueryMemberWithUID(toUID, groupNo)
	if err != nil {
		g.Error("查询是否存在成员失败！", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrGroupQueryFailed, nil, nil)
		return
	}
	if toMember == nil {
		httperr.ResponseErrorL(c, errcode.ErrGroupMemberNotInGroup, nil, nil)
		return
	}
	// 拦截将群主转让给外部成员 (is_external=1)：群主拥有全部敏感操作权限，
	// 绝不能落到外部成员手上 (YUJ-231 / GH#1289)。
	if toMember.IsExternal == 1 {
		g.Warn("拒绝将群主转让给外部成员",
			zap.String("group_no", groupNo),
			zap.String("to_uid", toUID),
			zap.String("operator", loginUID))
		httperr.ResponseErrorL(c, errcode.ErrGroupExternalCannotBeOwner, nil, nil)
		return
	}
	forbiddenExpirTime := toMember.ForbiddenExpirTime
	/**
	判断当前请求转让的用户是否是群主，只有群主才能把群主的位置转让给别人
	**/
	isCreator, err := g.db.QueryIsGroupCreator(groupNo, loginUID)
	if err != nil {
		g.Error("查询是否是群主失败！", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrGroupQueryFailed, nil, nil)
		return
	}
	if !isCreator {
		httperr.ResponseErrorL(c, errcode.ErrGroupCreatorOnly, nil, nil)
		return
	}

	groupModel, err := g.getGroupInfo(groupNo)
	if err != nil {
		respondGroupInfoError(c, err)
		return
	}

	version, err := g.ctx.GenSeq(common.GroupMemberSeqKey)
	if err != nil {
		g.Error("生成序列号失败", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrGroupStoreFailed, nil, nil)
		return
	}
	/**
	修改群主为普通成员，修改转让用户为群主
	**/
	tx, err := g.db.session.Begin()
	if err != nil {
		g.Error("开启事务失败！", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrGroupStoreFailed, nil, nil)
		return
	}
	defer func() {
		if err := recover(); err != nil {
			tx.RollbackUnlessCommitted()
			fmt.Fprintf(os.Stderr, "recovered panic in goroutine: %v\n%s\n", err, debug.Stack())
		}
	}()
	eventID, err := g.ctx.EventBegin(&wkevent.Data{
		Event: event.GroupMemberTransferGrouper,
		Type:  wkevent.Message,
		Data: config.MsgGroupTransferGrouper{
			GroupNo:        groupNo,
			OldGrouper:     loginUID,
			OldGrouperName: loginName,
			NewGrouper:     toUID,
			NewGrouperName: toUser.Name,
		},
	}, tx)
	if err != nil {
		tx.Rollback()
		g.Error("开启事件失败！", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrGroupStoreFailed, nil, nil)
		return
	}
	err = g.db.UpdateMemberRoleTx(groupNo, loginUID, MemberRoleCommon, version, tx)
	if err != nil {
		tx.Rollback()
		g.Error("更新成普通成员失败！", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrGroupStoreFailed, nil, nil)
		return
	}
	err = g.db.UpdateMemberRoleTx(groupNo, toUID, MemberRoleCreator, version, tx)
	if err != nil {
		tx.Rollback()
		g.Error("更新成创建者失败！", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrGroupStoreFailed, nil, nil)
		return
	}
	// 修改普通成员禁言时长
	err = g.db.updateMemberForbiddenExpirTimeTx(groupNo, toUID, 0, version, tx)
	if err != nil {
		tx.Rollback()
		g.Error("修改成员禁言时长失败！", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrGroupStoreFailed, nil, nil)
		return
	}
	if err := tx.Commit(); err != nil {
		tx.Rollback()
		g.Error("提交事务失败！", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrGroupStoreFailed, nil, nil)
		return
	}
	g.ctx.EventCommit(eventID)

	if groupModel.Forbidden == 1 { // 如果是禁言状态，则重置管理员白名单
		err = g.setIMWhitelistForGroupManager(groupModel.GroupNo)
		if err != nil {
			httperr.ResponseErrorL(c, errcode.ErrGroupStoreFailed, nil, nil)
			g.Error("设置白名单失败！", zap.Error(err))
			return
		}
	}
	if forbiddenExpirTime > 0 {
		toUIDs := make([]string, 0)
		toUIDs = append(toUIDs, toUID)
		err = g.ctx.IMBlacklistRemove(config.ChannelBlacklistReq{
			ChannelReq: config.ChannelReq{
				ChannelID:   groupNo,
				ChannelType: common.ChannelTypeGroup.Uint8(),
			},
			UIDs: toUIDs,
		})
		if err != nil {
			httperr.ResponseErrorL(c, errcode.ErrGroupStoreFailed, nil, nil)
			g.Error("新群主添加白名单失败！", zap.Error(err))
			return
		}
	}

	c.ResponseOK()

}

// 修改群里群成员信息
func (g *Group) memberUpdate(c *wkhttp.Context) {
	loginUID := c.MustGet("uid").(string)
	memberUID := c.Param("uid")
	groupNo := c.Param("group_no")
	var memberUpdateMap map[string]interface{}
	if err := c.BindJSON(&memberUpdateMap); err != nil {
		g.Error("数据格式有误！", zap.Error(err))
		respondGroupRequestInvalid(c, "")
		return
	}
	_, err := g.getGroupInfo(groupNo)
	if err != nil {
		respondGroupInfoError(c, err)
		return
	}
	isManager, err := g.db.QueryIsGroupManagerOrCreator(groupNo, loginUID)
	if err != nil {
		g.Error("查询是否是群管理者失败！", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrGroupQueryFailed, nil, nil)
		return
	}
	if !isManager && loginUID != memberUID {
		g.Error("只有管理员才能修改其他人的成员信息！")
		httperr.ResponseErrorL(c, errcode.ErrGroupManagerOnly, nil, nil)
		return
	}
	memberModel, err := g.db.QueryMemberWithUID(memberUID, groupNo)
	if err != nil {
		g.Error("查询成员信息失败！", zap.Error(err), zap.String("groupNo", groupNo), zap.String("memberUID", memberUID))
		httperr.ResponseErrorL(c, errcode.ErrGroupQueryFailed, nil, nil)
		return
	}
	if memberModel == nil {
		httperr.ResponseErrorL(c, errcode.ErrGroupMemberNotInGroup, nil, nil)
		return
	}
	for key, value := range memberUpdateMap {
		switch key {
		case "remark":
			remark, ok := value.(string)
			if !ok {
				respondGroupRequestInvalid(c, "remark")
				return
			}
			memberModel.Remark = remark
		}
	}
	genSeqVal, err := g.ctx.GenSeq(common.GroupMemberSeqKey)
	if err != nil {
		g.Error("生成序列号失败", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrGroupStoreFailed, nil, nil)
		return
	}
	memberModel.Version = genSeqVal
	err = g.db.UpdateMember(memberModel)
	if err != nil {
		g.Error("更新群成员信息失败！", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrGroupStoreFailed, nil, nil)
		return
	}
	err = g.ctx.SendCMD(config.MsgCMDReq{
		ChannelID:   groupNo,
		ChannelType: common.ChannelTypeGroup.Uint8(),
		CMD:         common.CMDGroupMemberUpdate,
		Param: map[string]interface{}{
			"group_no": groupNo,
			"uid":      memberUID,
		},
	})
	if err != nil {
		g.Error("发送命令消息失败！", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrGroupNotifyFailed, nil, nil)
		return
	}

	c.ResponseOK()
}

// 移除群成员
func (g *Group) memberRemove(c *wkhttp.Context) {
	operator := c.GetLoginUID()
	operatorName := c.GetLoginName()
	var req memberRemoveReq
	if err := c.BindJSON(&req); err != nil {
		g.Error(common.ErrData.Error(), zap.Error(err))
		respondGroupRequestInvalid(c, "")
		return
	}
	if err := req.Check(); err != nil {
		respondGroupRequestInvalid(c, "")
		return
	}
	groupNo := c.Param("group_no")
	req.Members = util.RemoveRepeatedElement(req.Members)

	// 判断群是否存在
	_, err := g.getGroupInfo(groupNo)
	if err != nil {
		respondGroupInfoError(c, err)
		return
	}
	var loginMember *MemberModel
	// 查询操作者身份
	// 这里要兼容后台管理系统的删除操作
	if c.CheckLoginRole() != nil {
		loginMember, err = g.db.QueryMemberWithUID(operator, groupNo)
		if err != nil {
			g.Error("查询操作者群成员信息错误", zap.Error(err))
			httperr.ResponseErrorL(c, errcode.ErrGroupQueryFailed, nil, nil)
			return
		}
		if loginMember == nil {
			httperr.ResponseErrorL(c, errcode.ErrGroupNotMember, nil, nil)
			return
		}
		if loginMember.Role != int(common.GroupMemberRoleCreater) && loginMember.Role != int(common.GroupMemberRoleManager) {
			httperr.ResponseErrorL(c, errcode.ErrGroupMemberCannotRemove, nil, nil)
			return
		}
	}
	// 验证删除者是否包含自己
	for _, uid := range req.Members {
		if uid == operator {
			httperr.ResponseErrorL(c, errcode.ErrGroupCannotTargetSelf, nil, nil)
			return
		}
	}
	// Web 特有的权限检查：管理员不能删管理员/群主
	if loginMember != nil {
		deleteMembers, err := g.db.QueryMembersWithUids(req.Members, groupNo)
		if err != nil {
			g.Error("查询被删除的群成员信息错误", zap.Error(err))
			httperr.ResponseErrorL(c, errcode.ErrGroupQueryFailed, nil, nil)
			return
		}
		if len(deleteMembers) == 0 {
			httperr.ResponseErrorL(c, errcode.ErrGroupMemberNotInGroup, nil, nil)
			return
		}
		for _, member := range deleteMembers {
			if loginMember.Role == int(common.GroupMemberRoleManager) {
				if member.Role == int(common.GroupMemberRoleManager) {
					httperr.ResponseErrorL(c, errcode.ErrGroupCannotRemoveAdmin, nil, nil)
					return
				}
				if member.Role == int(common.GroupMemberRoleCreater) {
					httperr.ResponseErrorL(c, errcode.ErrGroupCannotRemoveOwner, nil, nil)
					return
				}
			}
		}
	}

	// 调用 Service 移除群成员
	_, err = g.groupService.RemoveGroupMembers(&RemoveGroupMembersServiceReq{
		GroupNo:      groupNo,
		Members:      req.Members,
		OperatorUID:  operator,
		OperatorName: operatorName,
	})
	if err != nil {
		// 后台管理路径会跳过普通成员的目标预校验；若删除的 UID 全不在群内，
		// RemoveGroupMembers 返回业务错误，应是 404 而非存储失败。
		if strings.Contains(err.Error(), "none of the members are in this group") {
			httperr.ResponseErrorL(c, errcode.ErrGroupMemberNotInGroup, nil, nil)
			return
		}
		// TOCTOU：getGroupInfo 之后群被解散 → 404，而非内部错误。
		if strings.Contains(err.Error(), "group not found or disbanded") {
			httperr.ResponseErrorL(c, errcode.ErrGroupNotFound, nil, nil)
			return
		}
		g.Error("移除群成员失败", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrGroupStoreFailed, nil, nil)
		return
	}

	c.ResponseOK()
}

// 修改群设置
func (g *Group) groupSettingUpdate(c *wkhttp.Context) {
	loginUID := c.MustGet("uid").(string) // 登录用户
	loginName := c.GetLoginName()
	groupNo := c.Param("group_no")

	var resultMap map[string]interface{}
	if err := c.BindJSON(&resultMap); err != nil {
		g.Error("数据格式有误！", zap.Error(err))
		respondGroupRequestInvalid(c, "")
		return
	}
	if len(resultMap) == 0 {
		c.ResponseOK()
		return
	}
	_, err := g.getGroupInfo(groupNo)
	if err != nil {
		respondGroupInfoError(c, err)
		return
	}
	getSettingFnc := func() (*Setting, bool, error) {
		setting, err := g.settingDB.QuerySetting(groupNo, loginUID)
		if err != nil {
			g.Error("查询群设置信息失败！", zap.Error(err))
			return nil, false, err
		}
		insert := false // 是否是插入操作
		version, err := g.ctx.GenSeq(common.GroupSettingSeqKey)
		if err != nil {
			return nil, false, err
		}
		if setting == nil { // 不存在设置信息
			insert = true
			setting = newDefaultSetting()
			setting.GroupNo = groupNo
			setting.UID = loginUID
			setting.Version = version
		} else {
			setting.Version = version
		}
		return setting, insert, nil
	}

	getGroupFnc := func() (*Model, error) {
		group, err := g.db.QueryWithGroupNo(groupNo)
		if err != nil {
			g.Error("查询群信息失败", zap.Error(err))
			return nil, err
		}
		if group == nil {
			// 缺失 / 已解散群 → 404,复用 getGroupInfo 的 not-found sentinel,
			// 由 respondGroupInfoError 分流,而非塌缩成内部查询失败。
			return nil, errGroupInfoNotFound
		}
		return group, nil
	}

	for key, value := range resultMap {
		settingActionFnc := settingActionMap[key]
		if settingActionFnc != nil {
			setting, newSetting, err := getSettingFnc()
			if err != nil {
				g.Error("获取设置信息失败！", zap.Error(err))
				httperr.ResponseErrorL(c, errcode.ErrGroupStoreFailed, nil, nil)
				return
			}
			ctx := &settingContext{
				loginUID:     loginUID,
				loginName:    c.GetLoginName(),
				groupSetting: setting,
				newSetting:   newSetting,
				g:            g,
			}
			err = settingActionFnc(ctx, value)
			if err != nil {
				// 错类型的设置值是 400 校验错误,不是存储失败。
				if errors.Is(err, errSettingInvalidValueType) {
					respondGroupRequestInvalid(c, key)
					return
				}
				g.Error("修改群设置信息错误", zap.Error(err))
				httperr.ResponseErrorL(c, errcode.ErrGroupStoreFailed, nil, nil)
				return
			}
			continue
		}
		groupUpdateActionFnc := groupUpdateActionMap[key]
		if groupUpdateActionFnc != nil {
			group, err := getGroupFnc()
			if err != nil {
				// 群不存在 → 404,查询失败 → 500;getGroupFnc 已记录 DB 错误。
				respondGroupInfoError(c, err)
				return
			}
			ctx := &groupUpdateContext{
				loginUID:   loginUID,
				loginName:  loginName,
				groupModel: group,
				g:          g,
			}
			err = groupUpdateActionFnc(ctx, value)
			if err != nil {
				// 非管理员/群主 → 403;错类型 / allow_external 越界 → 400 校验错误。
				// 仅真正的 DB / 事务 / 事件失败才落到 Internal=true 的 store_failed。
				if errors.Is(err, errGroupUpdateForbidden) {
					httperr.ResponseErrorL(c, errcode.ErrGroupCreatorOrManagerOnly, nil, nil)
					return
				}
				if errors.Is(err, errSettingInvalidValueType) || errors.Is(err, errSettingAllowExternalRange) || errors.Is(err, errSettingAllowNoMentionRange) {
					respondGroupRequestInvalid(c, key)
					return
				}
				g.Error("修改群设置信息错误", zap.Error(err))
				httperr.ResponseErrorL(c, errcode.ErrGroupStoreFailed, nil, nil)
				return
			}
			continue
		}
	}

	c.ResponseOK()
}

// 退出群聊
func (g *Group) groupExit(c *wkhttp.Context) {
	loginUID := c.MustGet("uid").(string)
	groupNo := c.Param("group_no")
	groupInfo, err := g.getGroupInfo(groupNo)
	if err != nil {
		// 不存在 / 已解散群 → 404；查询失败 → 500。getGroupInfo 已记录 DB 错误。
		respondGroupInfoError(c, err)
		return
	}
	// 调用IM的移除订阅者
	err = g.ctx.IMRemoveSubscriber(&config.SubscriberRemoveReq{
		ChannelID:   groupNo,
		ChannelType: common.ChannelTypeGroup.Uint8(),
		Subscribers: []string{loginUID},
	})
	if err != nil {
		g.Error("移除订阅者失败！", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrGroupNotifyFailed, nil, nil)
		return
	}
	loginMember, err := g.db.QueryMemberWithUID(loginUID, groupNo)
	if err != nil {
		g.Error("查询是否存在群成员失败！", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrGroupQueryFailed, nil, nil)
		return
	}
	if loginMember == nil {
		httperr.ResponseErrorL(c, errcode.ErrGroupMemberNotInGroup, nil, nil)
		return
	}
	// 查询群的管理员和群主
	adminAndCreatorUIDS, err := g.db.QueryGroupManagerOrCreatorUIDS(groupNo)
	if err != nil {
		g.Error("查询群管理员失败！", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrGroupQueryFailed, nil, nil)
		return
	}
	visiblesUids := make([]string, 0)
	if len(adminAndCreatorUIDS) > 0 {
		for _, uid := range adminAndCreatorUIDS {
			if uid != loginUID {
				visiblesUids = append(visiblesUids, uid)
				break
			}
		}
	}

	/**
	如果退出的人是群主，则选择第二个入群的人作为群主。
	**/
	var newGrouper *MemberModel // 新群主
	if loginMember.Role == MemberRoleCreator {
		// 查询第二老成员。#354：排除退群群主名下的 bot——它们会在下方被级联带走，
		// 不能被选为新群主（否则新群主在同一事务内即被删除）。
		newGrouper, err = g.db.QuerySecondOldestMemberExcludingBotsOf(groupNo, loginUID)
		if err != nil {
			g.Error("查询第二元老成员失败！", zap.Error(err))
			httperr.ResponseErrorL(c, errcode.ErrGroupQueryFailed, nil, nil)
			return
		}
	}
	/**
	如果退出的人是普通成员，则直接删除就行
	**/
	version, err := g.ctx.GenSeq(common.GroupMemberSeqKey)
	if err != nil {
		g.Error("生成序列号失败", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrGroupStoreFailed, nil, nil)
		return
	}

	tx, err := g.db.session.Begin()
	if err != nil {
		g.Error("开启事务失败！", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrGroupStoreFailed, nil, nil)
		return
	}
	defer func() {
		if err := recover(); err != nil {
			tx.RollbackUnlessCommitted()
			fmt.Fprintf(os.Stderr, "recovered panic in goroutine: %v\n%s\n", err, debug.Stack())
		}
	}()
	eventID, err := g.ctx.EventBegin(&wkevent.Data{
		Event: event.ConversationDelete,
		Type:  wkevent.CMD,
		Data: &config.DeleteConversationReq{
			ChannelID:   groupNo,
			ChannelType: common.ChannelTypeGroup.Uint8(),
			UID:         loginUID,
		},
	}, tx)
	if err != nil {
		tx.Rollback()
		g.Error("开启事件事务失败！", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrGroupStoreFailed, nil, nil)
		return
	}
	if newGrouper != nil {
		err = g.db.UpdateMemberRoleTx(groupNo, newGrouper.UID, MemberRoleCreator, version, tx)
		if err != nil {
			tx.Rollback()
			g.Error("更换新的群主失败！", zap.Error(err))
			httperr.ResponseErrorL(c, errcode.ErrGroupStoreFailed, nil, nil)
			return
		}
	}
	err = g.db.DeleteMemberTx(groupNo, loginUID, version, tx)
	if err != nil {
		tx.Rollback()
		g.Error("删除群成员失败！", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrGroupStoreFailed, nil, nil)
		return
	}

	// D-2 · 级联带走 inviter 拉入的 bot（YUJ-49 / Mininglamp-OSS/octo-server#1186）。
	// #354 产品决策：bot 永远跟随其主人，无角色例外——群主退群（角色已由上方
	// newGrouper 完成转让）同样级联带走自己名下的 bot，和普通成员一致。
	var cascadedBotUsers []*user.Model
	cascadedUIDs, cerr := cascadeRemoveBotsInvitedByUIDTx(g.db, g.ctx, groupNo, loginUID, tx)
	if cerr != nil {
		tx.Rollback()
		g.Error("级联移除 bot 成员失败", zap.Error(cerr))
		httperr.ResponseErrorL(c, errcode.ErrGroupStoreFailed, nil, nil)
		return
	}
	for _, botUID := range cascadedUIDs {
		botUser, _ := g.userDB.QueryByUID(botUID)
		cascadedBotUsers = append(cascadedBotUsers, botUser)
	}

	// 若退群者是外部成员且当前群是外部群，检查是否需要恢复普通群
	resetExternalGroup := false
	if loginMember.IsExternal == 1 && groupInfo.IsExternalGroup == 1 {
		externalCount, countErr := g.db.QueryExternalMemberCountTx(groupNo, tx)
		if countErr != nil {
			g.Error("查询外部成员数量失败", zap.Error(countErr))
		} else if externalCount == 0 {
			if updateErr := g.db.UpdateIsExternalGroupTx(groupNo, 0, tx); updateErr != nil {
				tx.Rollback()
				g.Error("更新 is_external_group 失败", zap.Error(updateErr))
				httperr.ResponseErrorL(c, errcode.ErrGroupStoreFailed, nil, nil)
				return
			}
			resetExternalGroup = true
		}
	}

	groupSetting, err := g.settingDB.querySettingWithTx(groupNo, loginUID, tx)
	if err != nil {
		tx.Rollback()
		g.Error("查询用户群设置错误", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrGroupQueryFailed, nil, nil)
		return
	}
	if groupSetting != nil && groupSetting.Save == 1 {
		// 清除保存设置
		groupSetting.Save = 0
		err = g.settingDB.UpdateSettingWithTx(groupSetting, tx)
		if err != nil {
			tx.Rollback()
			g.Error("修改群设置信息错误", zap.Error(err))
			httperr.ResponseErrorL(c, errcode.ErrGroupStoreFailed, nil, nil)
			return
		}
	}
	if err := tx.Commit(); err != nil {
		tx.RollbackUnlessCommitted()
		g.Error("提交事务失败！", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrGroupStoreFailed, nil, nil)
		return
	}
	g.ctx.EventCommit(eventID)

	// 外部群标记发生变化时，通知成员刷新频道信息
	if resetExternalGroup {
		g.ctx.SendChannelUpdateToGroup(groupNo)
	}

	// 移除用户在该群所有子区的成员身份和置顶
	g.removeUserFromGroupThreads(groupNo, loginUID, groupInfo.SpaceID)
	// 发送群成员更新命令
	err = g.ctx.SendCMD(config.MsgCMDReq{
		ChannelID:   groupNo,
		ChannelType: common.ChannelTypeGroup.Uint8(),
		CMD:         common.CMDGroupMemberUpdate,
		Param: map[string]interface{}{
			"group_no": groupNo,
			"uid":      loginUID,
		},
	})
	if err != nil {
		g.Error("发送群更新命令失败！", zap.Error(err), zap.String("groupNo", groupNo))
		httperr.ResponseErrorL(c, errcode.ErrGroupNotifyFailed, nil, nil)
		return
	}
	var showName = loginMember.Remark
	if showName == "" {
		showName = c.GetLoginName()
	}
	if groupInfo.Status != GroupStatusDisband && len(visiblesUids) > 0 {
		// 发送群成员退出群聊消息
		err = g.ctx.SendGroupExit(groupNo, loginUID, showName, visiblesUids)
		if err != nil {
			g.Error("发送成员退出群聊错误", zap.Error(err))
		}
	}

	// D-2 · bot 级联移除后发系统 Tip，保持透明度（#1186 / YUJ-49）。
	// IM 订阅也一并摘除，避免 bot 继续收到群消息。
	if len(cascadedBotUsers) > 0 && groupInfo.Status != GroupStatusDisband {
		botUIDs := make([]string, 0, len(cascadedBotUsers))
		for _, bu := range cascadedBotUsers {
			if bu == nil {
				continue
			}
			botUIDs = append(botUIDs, bu.UID)
		}
		if len(botUIDs) > 0 {
			if err := g.ctx.IMRemoveSubscriber(&config.SubscriberRemoveReq{
				ChannelID:   groupNo,
				ChannelType: common.ChannelTypeGroup.Uint8(),
				Subscribers: botUIDs,
			}); err != nil {
				g.Error("级联移除 bot 的 IM 订阅失败", zap.Error(err))
			}
		}
		if err := sendBotCascadeRemovedTip(g.ctx, groupNo, showName, "离开了", cascadedBotUsers); err != nil {
			g.Error("发送 bot 级联移除 Tip 失败", zap.Error(err))
		}
		// YUJ-52 / Mininglamp-OSS/octo-server#1189 · 级联退群的 bot 也必须清理 thread_member
		// 和 Space 维度的 pinned，对齐 service.go kick 路径（service.go:1483-1488）。
		// 顺序放在 tip 之后，与 kick 路径一致。
		for _, bu := range cascadedBotUsers {
			if bu == nil {
				continue
			}
			g.removeUserFromGroupThreads(groupNo, bu.UID, groupInfo.SpaceID)
			user.RemovePinnedForUserInSpace(bu.UID, groupInfo.SpaceID, groupNo, common.ChannelTypeGroup.Uint8())
			conversation_ext.RemoveConvExtForUserInSpace(bu.UID, groupInfo.SpaceID, groupNo, common.ChannelTypeGroup.Uint8())
		}
	}
	// 清理用户在该群的置顶（按 Space 隔离）
	user.RemovePinnedForUserInSpace(loginUID, groupInfo.SpaceID, groupNo, common.ChannelTypeGroup.Uint8())
	conversation_ext.RemoveConvExtForUserInSpace(loginUID, groupInfo.SpaceID, groupNo, common.ChannelTypeGroup.Uint8())
	c.ResponseOK()

}

// removeUserFromGroupThreads 移除用户在某群下所有子区的成员记录、IM 订阅和置顶。
// 委托给包级 removeUserFromGroupThreadsCleanup（见 thread_cleanup.go，Issue #27），
// 保留方法签名以最小化调用方改动。
func (g *Group) removeUserFromGroupThreads(groupNo, uid, spaceID string) {
	removeUserFromGroupThreadsCleanup(g.ctx, g.Log, groupNo, uid, spaceID)
}

// addUsersToGroupThreads 新成员入群时，将其加入该群所有子区的 IM 订阅（允许发消息）
func (g *Group) addUsersToGroupThreads(groupNo string, uids []string) {
	if len(uids) == 0 {
		return
	}

	// 查询该群的所有活跃子区
	type threadInfo struct {
		ShortID string `db:"short_id"`
	}
	var threads []threadInfo
	_, err := g.db.session.Select("short_id").
		From("thread").
		Where("group_no=? AND status!=3", groupNo). // status=3 是已删除
		Load(&threads)
	if err != nil {
		g.Error("查询群子区失败", zap.Error(err), zap.String("groupNo", groupNo))
		return
	}
	if len(threads) == 0 {
		return
	}

	// 将新成员加入所有子区的 IM 订阅
	for _, t := range threads {
		// 子区 channelID 格式: {groupNo}____{shortID} (与 thread.BuildChannelID 一致)
		channelID := groupNo + "____" + t.ShortID
		if addErr := g.ctx.IMAddSubscriber(&config.SubscriberAddReq{
			ChannelID:   channelID,
			ChannelType: common.ChannelTypeCommunityTopic.Uint8(),
			Subscribers: uids,
		}); addErr != nil {
			g.Error("添加子区IM订阅者失败", zap.Error(addErr), zap.String("channelID", channelID), zap.Strings("uids", uids))
		}
	}
}

// 添加或移除黑名单
func (g *Group) blacklist(c *wkhttp.Context) {
	loginUID := c.MustGet("uid").(string)
	groupNo := c.Param("group_no")
	action := c.Param("action")
	var req blacklistReq
	if err := c.BindJSON(&req); err != nil {
		g.Error(common.ErrData.Error(), zap.Error(err))
		respondGroupRequestInvalid(c, "")
		return
	}
	if len(req.Uids) == 0 {
		respondGroupRequestInvalid(c, "members")
		return
	}
	if groupNo == "" {
		respondGroupRequestInvalid(c, "group_no")
		return
	}
	if action == "" {
		respondGroupRequestInvalid(c, "action")
		return
	}
	group, err := g.db.QueryDetailWithGroupNo(groupNo, loginUID)
	if err != nil {
		g.Error("查询群详情错误", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrGroupQueryFailed, nil, nil)
		return
	}
	if group == nil || group.Status == GroupStatusDisband {
		g.Error("群不存在", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrGroupNotFound, nil, nil)
		return
	}
	// 查询是否是管理者
	isManager, err := g.db.QueryIsGroupManagerOrCreator(groupNo, loginUID)
	if err != nil {
		g.Error("查询是否是群管理者失败！", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrGroupQueryFailed, nil, nil)
		return
	}
	if !isManager {
		httperr.ResponseErrorL(c, errcode.ErrGroupManagerOnly, nil, nil)
		return
	}
	// #354 · Bot 跟人走：拉黑/解除拉黑级联到目标用户名下在群的 bot
	// （robot.creator_uid 命中）。旧行为只动用户本人，其 bot 仍 status=Normal，
	// 被拉黑用户可经自己的 bot 旁路读群/子区内容，绕过 ExistMemberActive
	// 加固线（#343/#345）。解除拉黑走同一扩展，对称恢复。
	targetUIDs, err := expandBlacklistTargetsWithOwnedBots(g.db, groupNo, req.Uids)
	if err != nil {
		g.Error("查询拉黑级联 bot 失败", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrGroupQueryFailed, nil, nil)
		return
	}
	status := 0
	if action == "add" {
		status = int(common.GroupMemberStatusBlacklist)
	} else {
		status = int(common.GroupMemberStatusNormal)
	}

	version, err := g.ctx.GenSeq(common.GroupMemberSeqKey)
	if err != nil {
		g.Error("生成序列号失败", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrGroupStoreFailed, nil, nil)
		return
	}
	err = g.db.updateMembersStatus(version, groupNo, status, targetUIDs)
	if err != nil {
		g.Error("添加或移除群成员黑名单错误", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrGroupStoreFailed, nil, nil)
		return
	}
	if status == int(common.GroupMemberStatusBlacklist) {
		err = g.setGroupBlacklist(groupNo, targetUIDs, status == int(common.GroupMemberStatusBlacklist))
		if err != nil {
			g.Error("添加IM黑名单错误", zap.Error(err))
			httperr.ResponseErrorL(c, errcode.ErrGroupStoreFailed, nil, nil)
			return
		}
		// YUJ-4185 P0-1：拉黑 add 分支必须主动摘除每个被拉黑 uid 的子区 + 父群 IM 订阅。
		// 仅靠 setGroupBlacklist(IMBlacklistAdd) 只挡“发送”，不断“接收”——被拉黑者仍
		// 通过 WuKongIM WS 收子区/父群实时消息（越权读 P0）。子区订阅复用 #27/#332 统一
		// helper；父群订阅显式 IMRemoveSubscriber。best-effort：失败只记日志、不回滚拉黑。
		// #354：级联拉黑的 bot 同样摘除订阅（targetUIDs 已含 bot）。
		for _, uid := range targetUIDs {
			g.removeUserFromGroupThreads(groupNo, uid, group.SpaceID)
		}
		if rmErr := g.ctx.IMRemoveSubscriber(&config.SubscriberRemoveReq{
			ChannelID:   groupNo,
			ChannelType: common.ChannelTypeGroup.Uint8(),
			Subscribers: targetUIDs,
		}); rmErr != nil {
			g.Error("拉黑摘除父群IM订阅失败", zap.Error(rmErr), zap.String("groupNo", groupNo))
		}
	} else {
		members, err := g.db.QueryMembersWithUids(targetUIDs, groupNo)
		if err != nil {
			g.Error("查询移除黑名单成员错误", zap.Error(err))
			httperr.ResponseErrorL(c, errcode.ErrGroupQueryFailed, nil, nil)
			return
		}
		if len(members) == 0 {
			httperr.ResponseErrorL(c, errcode.ErrGroupMemberNotInGroup, nil, nil)
			return
		}
		removeUIDs := make([]string, 0)
		for _, member := range members {
			if member.ForbiddenExpirTime == 0 {
				removeUIDs = append(removeUIDs, member.UID)
			}
		}
		if len(removeUIDs) > 0 {
			err = g.setGroupBlacklist(groupNo, removeUIDs, false)
			if err != nil {
				g.Error("移除IM黑名单错误", zap.Error(err))
				httperr.ResponseErrorL(c, errcode.ErrGroupStoreFailed, nil, nil)
				return
			}
			// YUJ-4185 P0-1：解除拉黑必须对称恢复订阅（add 分支摘掉了父群+子区订阅）。
			// updateMembersStatus 已把 status 改回 Normal，此处把这些成员重新挂回父群和
			// 群内所有非删除子区的 IM 订阅，参考入群 addUsersToGroupThreads。
			// best-effort：失败只记日志，不回滚解除黑名单。
			if addErr := g.ctx.IMAddSubscriber(&config.SubscriberAddReq{
				ChannelID:   groupNo,
				ChannelType: common.ChannelTypeGroup.Uint8(),
				Subscribers: removeUIDs,
			}); addErr != nil {
				g.Error("解除拉黑恢复父群IM订阅失败", zap.Error(addErr), zap.String("groupNo", groupNo))
			}
			g.addUsersToGroupThreads(groupNo, removeUIDs)
		}
	}
	if group.GroupType == int(GroupTypeCommon) {
		// 发送群成员更新命令
		err = g.ctx.SendCMD(config.MsgCMDReq{
			ChannelID:   groupNo,
			ChannelType: common.ChannelTypeGroup.Uint8(),
			CMD:         common.CMDGroupMemberUpdate,
			Param: map[string]interface{}{
				"group_no": groupNo,
			},
		})
		if err != nil {
			g.Error("发送更新群成员消息错误", zap.Error(err))
			httperr.ResponseErrorL(c, errcode.ErrGroupNotifyFailed, nil, nil)
			return
		}
	} else {
		for _, uid := range targetUIDs {
			// 发送群成员更新命令
			err = g.ctx.SendCMD(config.MsgCMDReq{
				ChannelID:   groupNo,
				ChannelType: common.ChannelTypeGroup.Uint8(),
				CMD:         common.CMDGroupMemberUpdate,
				Param: map[string]interface{}{
					"group_no": groupNo,
					"uid":      uid,
				},
			})
			if err != nil {
				g.Error("发送更新群成员消息错误", zap.Error(err))
				httperr.ResponseErrorL(c, errcode.ErrGroupNotifyFailed, nil, nil)
				return
			}
		}
	}
	c.ResponseOK()
}

// 禁言时长列表
func (g *Group) forbiddenTimesList(c *wkhttp.Context) {
	type forbiddenTime struct {
		Text string `json:"text"`
		Key  int    `json:"key"`
	}
	list := []*forbiddenTime{
		{
			Text: "1分钟",
			Key:  1,
		},
		{
			Text: "10分钟",
			Key:  2,
		},
		{
			Text: "1小时",
			Key:  3,
		},
		{
			Text: "1天",
			Key:  4,
		},
		{
			Text: "1周",
			Key:  5,
		},
		{
			Text: "1个月",
			Key:  6,
		},
	}
	c.Response(list)
}

// 禁言某个群成员
func (g *Group) forbiddenWithGroupMember(c *wkhttp.Context) {
	type forbiddenWithGroupMemberReq struct {
		MemberUID string `json:"member_uid"`
		Action    int    `json:"action"` // 0.解禁1.禁言
		Key       int    `json:"key"`
	}
	var req forbiddenWithGroupMemberReq
	if err := c.BindJSON(&req); err != nil {
		g.Error("数据格式有误！", zap.Error(err))
		respondGroupRequestInvalid(c, "")
		return
	}
	loginUID := c.GetLoginUID()
	groupNo := c.Param("group_no")
	if groupNo == "" {
		respondGroupRequestInvalid(c, "group_no")
		return
	}
	if req.MemberUID == "" {
		respondGroupRequestInvalid(c, "member_uid")
		return
	}

	if req.Action != 0 && req.Action != 1 {
		respondGroupRequestInvalid(c, "action")
		return
	}
	group, err := g.getGroupInfo(groupNo)
	if err != nil {
		respondGroupInfoError(c, err)
		return
	}
	loginGroupMember, err := g.db.QueryMemberWithUID(loginUID, group.GroupNo)
	if err != nil {
		g.Error("查询登录用户群内信息错误", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrGroupQueryFailed, nil, nil)
		return
	}
	if loginGroupMember == nil {
		httperr.ResponseErrorL(c, errcode.ErrGroupNotMember, nil, nil)
		return
	}
	member, err := g.db.QueryMemberWithUID(req.MemberUID, group.GroupNo)
	if err != nil {
		g.Error("查询成员信息错误", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrGroupQueryFailed, nil, nil)
		return
	}
	if member == nil {
		httperr.ResponseErrorL(c, errcode.ErrGroupMemberNotInGroup, nil, nil)
		return
	}
	if loginGroupMember.Role == MemberRoleCommon || member.Role == MemberRoleCreator || loginGroupMember.Role == member.Role {
		respondGroupForbidden(c)
		return
	}
	genSeqVal, err := g.ctx.GenSeq(common.GroupMemberSeqKey)
	if err != nil {
		g.Error("生成序列号失败", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrGroupStoreFailed, nil, nil)
		return
	}
	member.Version = genSeqVal
	if req.Action == 0 {
		// 解禁
		member.ForbiddenExpirTime = 0
		err := g.db.UpdateMember(member)
		if err != nil {
			g.Error("解除用户禁言错误", zap.Error(err))
			httperr.ResponseErrorL(c, errcode.ErrGroupStoreFailed, nil, nil)
			return
		}
	} else {
		expirationTime := time.Now().Unix()
		switch req.Key {
		case 1:
			expirationTime += 60
		case 2:
			expirationTime += 60 * 10
		case 3:
			expirationTime += 60 * 60
		case 4:
			expirationTime += 60 * 60 * 24
		case 5:
			expirationTime += 60 * 60 * 24 * 7
		case 6:
			expirationTime += 60 * 60 * 24 * 30
		default:
			expirationTime = 0
		}
		if expirationTime == 0 {
			respondGroupRequestInvalid(c, "key")
			return
		}
		member.ForbiddenExpirTime = expirationTime
		err = g.db.UpdateMember(member)
		if err != nil {
			g.Error("禁言用户错误", zap.Error(err))
			httperr.ResponseErrorL(c, errcode.ErrGroupStoreFailed, nil, nil)
			return
		}
	}

	// 加入talk黑名单
	uids := make([]string, 0)
	uids = append(uids, req.MemberUID)
	err = g.setGroupBlacklist(groupNo, uids, req.Action == 1)
	if err != nil {
		httperr.ResponseErrorL(c, errcode.ErrGroupStoreFailed, nil, nil)
		return
	}
	err = g.ctx.SendCMD(config.MsgCMDReq{
		ChannelID:   groupNo,
		ChannelType: common.ChannelTypeGroup.Uint8(),
		CMD:         common.CMDGroupMemberUpdate,
		Param: map[string]interface{}{
			"group_no": groupNo,
			"uid":      req.MemberUID,
		},
	})
	if err != nil {
		g.Error("发送命令消息失败！", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrGroupNotifyFailed, nil, nil)
		return
	}
	c.ResponseOK()
}

func (g *Group) CheckForbiddenLoop() {
	var limit int64 = 100
	var errSleep = time.Second * 1
	var noDataSleep = time.Second * 15
	for {
		models, err := g.db.queryForbiddenExpirationTimeMembers(limit)
		if err != nil {
			g.Warn("查询禁言成员信息错误", zap.Error(err))
			time.Sleep(errSleep) // 错误后退避重试
			continue
		}
		if len(models) <= 0 {
			time.Sleep(noDataSleep) // 无数据时降低轮询频率
			continue
		}
		for _, model := range models {
			genSeqVal, err := g.ctx.GenSeq(common.GroupMemberSeqKey)
			if err != nil {
				g.Error("GenSeq failed", zap.Error(err))
				continue
			}
			model.Version = genSeqVal
			model.ForbiddenExpirTime = 0
			err = g.db.UpdateMember(model)
			if err != nil {
				g.Warn("更新禁言成员新消息错误", zap.Error(err))
				continue
			}
			uids := []string{model.UID}
			if model.Status != int(common.GroupMemberStatusBlacklist) {
				err = g.setGroupBlacklist(model.GroupNo, uids, false)
				if err != nil {
					g.Warn("更新禁言成员新消息错误", zap.Error(err))
					continue
				}
			}
			err = g.ctx.SendCMD(config.MsgCMDReq{
				ChannelID:   model.GroupNo,
				ChannelType: common.ChannelTypeGroup.Uint8(),
				CMD:         common.CMDGroupMemberUpdate,
				Param: map[string]interface{}{
					"group_no": model.GroupNo,
					"uid":      model.UID,
				},
			})
			if err != nil {
				g.Error("发送命令消息失败！", zap.Error(err))
				continue
			}
		}
	}
}

// 设置talk黑名单
func (g *Group) setGroupBlacklist(groupNo string, uids []string, isAdd bool) error {
	var err error
	if isAdd {
		err = g.ctx.IMBlacklistAdd(config.ChannelBlacklistReq{
			ChannelReq: config.ChannelReq{
				ChannelID:   groupNo,
				ChannelType: common.ChannelTypeGroup.Uint8(),
			}, UIDs: uids})
	} else {
		err = g.ctx.IMBlacklistRemove(config.ChannelBlacklistReq{
			ChannelReq: config.ChannelReq{
				ChannelID:   groupNo,
				ChannelType: common.ChannelTypeGroup.Uint8(),
			}, UIDs: uids})
	}
	if err != nil {
		g.Error("设置群黑名单错误", zap.Error(err))
		return err
	}
	return nil
}

// 获取群资料
func (g *Group) getGroupInfo(groupNo string) (*Model, error) {
	group, err := g.db.QueryWithGroupNo(groupNo)
	if err != nil {
		g.Error("查询群资料错误", zap.Error(err))
		return nil, errGroupInfoQueryFailed
	}
	if group == nil || group.Status == GroupStatusDisband {
		return nil, errGroupInfoNotFound
	}
	return group, nil
}

// resolveGroupNo extracts the parent group number from a thread channel ID
// (format: "groupNo____shortId") or returns the input unchanged for regular groups.
func resolveGroupNo(groupNo string) string {
	// mirrors thread.ChannelIDSeparator (modules/thread/const.go)
	const threadSeparator = "____"
	if idx := strings.Index(groupNo, threadSeparator); idx > 0 {
		return groupNo[:idx]
	}
	return groupNo
}

// getGroupMdMaxSize is a convenience alias for GetGroupMdMaxSize (service layer)
func getGroupMdMaxSize() int {
	return GetGroupMdMaxSize()
}

// groupMdGet returns GROUP.md content for a group
func (g *Group) groupMdGet(c *wkhttp.Context) {
	groupNo := c.Param("group_no")
	loginUID := c.GetLoginUID()

	isMember, err := g.db.ExistMember(loginUID, groupNo)
	if err != nil {
		g.Error("check group member failed", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrGroupQueryFailed, nil, nil)
		return
	}
	if !isMember {
		httperr.ResponseErrorL(c, errcode.ErrGroupViewForbidden, nil, nil)
		return
	}

	result, err := g.db.QueryGroupMd(groupNo)
	if err != nil {
		g.Error("query GROUP.md failed", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrGroupQueryFailed, nil, nil)
		return
	}
	if result == nil {
		c.Response(groupMdResp{
			Content:   "",
			Version:   0,
			UpdatedAt: nil,
			UpdatedBy: "",
		})
		return
	}
	c.Response(groupMdResp{
		Content:   result.Content,
		Version:   result.Version,
		UpdatedAt: result.UpdatedAt,
		UpdatedBy: result.UpdatedBy,
	})
}

// groupMdUpdate creates or updates GROUP.md content
func (g *Group) groupMdUpdate(c *wkhttp.Context) {
	groupNo := c.Param("group_no")
	loginUID := c.GetLoginUID()

	isManagerOrCreator, err := g.db.QueryIsGroupManagerOrCreator(groupNo, loginUID)
	if err != nil {
		g.Error("check permission failed", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrGroupQueryFailed, nil, nil)
		return
	}
	if !isManagerOrCreator {
		httperr.ResponseErrorL(c, errcode.ErrGroupCreatorOrManagerOnly, nil, nil)
		return
	}

	var req struct {
		Content string `json:"content"`
	}
	if err := c.BindJSON(&req); err != nil {
		respondGroupRequestInvalid(c, "")
		return
	}

	maxSize := getGroupMdMaxSize()
	if len(req.Content) > maxSize {
		respondGroupMdContentTooLarge(c, maxSize)
		return
	}

	newVersion, err := g.db.UpdateGroupMd(groupNo, req.Content, loginUID)
	if err != nil {
		g.Error("update GROUP.md failed", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrGroupStoreFailed, nil, nil)
		return
	}

	// Async send notification
	go func() {
		defer func() {
			if r := recover(); r != nil {
				g.Error("sendGroupMdNotification panic", zap.Any("recover", r))
			}
		}()
		g.sendGroupMdNotification(groupNo, loginUID, newVersion, "group_md_updated", "GROUP.md updated")
	}()

	c.Response(map[string]interface{}{
		"version": newVersion,
	})
}

// groupMdDelete deletes GROUP.md content
func (g *Group) groupMdDelete(c *wkhttp.Context) {
	groupNo := c.Param("group_no")
	loginUID := c.GetLoginUID()

	isManagerOrCreator, err := g.db.QueryIsGroupManagerOrCreator(groupNo, loginUID)
	if err != nil {
		g.Error("check permission failed", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrGroupQueryFailed, nil, nil)
		return
	}
	if !isManagerOrCreator {
		httperr.ResponseErrorL(c, errcode.ErrGroupCreatorOrManagerOnly, nil, nil)
		return
	}

	newVersion, err := g.db.DeleteGroupMd(groupNo)
	if err != nil {
		g.Error("delete GROUP.md failed", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrGroupStoreFailed, nil, nil)
		return
	}

	// Async send notification
	go func() {
		defer func() {
			if r := recover(); r != nil {
				g.Error("sendGroupMdNotification panic", zap.Any("recover", r))
			}
		}()
		g.sendGroupMdNotification(groupNo, loginUID, newVersion, "group_md_deleted", "GROUP.md deleted")
	}()

	c.ResponseOK()
}

// botAdminSet sets a bot member as bot_admin
func (g *Group) botAdminSet(c *wkhttp.Context) {
	groupNo := c.Param("group_no")
	targetUID := c.Param("uid")
	loginUID := c.GetLoginUID()

	isManagerOrCreator, err := g.db.QueryIsGroupManagerOrCreator(groupNo, loginUID)
	if err != nil {
		g.Error("check permission failed", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrGroupQueryFailed, nil, nil)
		return
	}
	if !isManagerOrCreator {
		httperr.ResponseErrorL(c, errcode.ErrGroupCreatorOrManagerOnly, nil, nil)
		return
	}

	// Verify target is a robot member
	member, err := g.db.QueryMemberWithUID(targetUID, groupNo)
	if err != nil {
		g.Error("query member failed", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrGroupQueryFailed, nil, nil)
		return
	}
	if member == nil {
		httperr.ResponseErrorL(c, errcode.ErrGroupMemberNotInGroup, nil, nil)
		return
	}
	if member.Robot != 1 {
		httperr.ResponseErrorL(c, errcode.ErrGroupTargetNotBot, nil, nil)
		return
	}

	version, err := g.ctx.GenSeq(common.GroupMemberSeqKey)
	if err != nil {
		g.Error("GenSeq failed", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrGroupStoreFailed, nil, nil)
		return
	}

	err = g.db.UpdateBotAdmin(groupNo, targetUID, 1, version)
	if err != nil {
		g.Error("set bot admin failed", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrGroupStoreFailed, nil, nil)
		return
	}
	c.ResponseOK()
}

// botAdminRemove removes bot_admin from a bot member
func (g *Group) botAdminRemove(c *wkhttp.Context) {
	groupNo := c.Param("group_no")
	targetUID := c.Param("uid")
	loginUID := c.GetLoginUID()

	isManagerOrCreator, err := g.db.QueryIsGroupManagerOrCreator(groupNo, loginUID)
	if err != nil {
		g.Error("check permission failed", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrGroupQueryFailed, nil, nil)
		return
	}
	if !isManagerOrCreator {
		httperr.ResponseErrorL(c, errcode.ErrGroupCreatorOrManagerOnly, nil, nil)
		return
	}

	// Verify target member exists in group
	member, err := g.db.QueryMemberWithUID(targetUID, groupNo)
	if err != nil {
		g.Error("query member failed", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrGroupQueryFailed, nil, nil)
		return
	}
	if member == nil {
		httperr.ResponseErrorL(c, errcode.ErrGroupMemberNotInGroup, nil, nil)
		return
	}

	version, err := g.ctx.GenSeq(common.GroupMemberSeqKey)
	if err != nil {
		g.Error("GenSeq failed", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrGroupStoreFailed, nil, nil)
		return
	}

	err = g.db.UpdateBotAdmin(groupNo, targetUID, 0, version)
	if err != nil {
		g.Error("remove bot admin failed", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrGroupStoreFailed, nil, nil)
		return
	}
	c.ResponseOK()
}

// sendGroupMdNotification sends GROUP.md event notification to the group
func (g *Group) sendGroupMdNotification(groupNo string, updatedBy string, version int64, eventType string, contentText string) {
	botUIDs, err := g.db.QueryBotMemberUIDs(groupNo)
	if err != nil {
		g.Error("query bot member UIDs failed", zap.Error(err))
		return
	}

	payload := map[string]interface{}{
		"type":    common.Text,
		"content": contentText,
		"event": map[string]interface{}{
			"type":       eventType,
			"version":    version,
			"updated_by": updatedBy,
		},
	}
	if len(botUIDs) > 0 {
		payload["mention"] = map[string]interface{}{
			"uids": botUIDs,
		}
	}

	err = g.ctx.SendMessage(&config.MsgSendReq{
		Header: config.MsgHeader{
			RedDot: 0,
		},
		ChannelID:   groupNo,
		ChannelType: common.ChannelTypeGroup.Uint8(),
		FromUID:     updatedBy,
		Payload:     []byte(util.ToJson(payload)),
	})
	if err != nil {
		g.Error("send GROUP.md notification failed", zap.Error(err))
	}
}

// ---------- vo ----------

type groupMdResp struct {
	Content   string     `json:"content"`
	Version   int64      `json:"version"`
	UpdatedAt *time.Time `json:"updated_at"`
	UpdatedBy string     `json:"updated_by"`
}

type groupDetailResp struct {
	GroupNo     string `json:"group_no"`  // 群编号
	Name        string `json:"name"`      // 群名称
	Notice      string `json:"notice"`    // 群公告
	Forbidden   int    `json:"forbidden"` // 是否全员禁言
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
	MemberCount int64  `json:"member_count"` // 成员数量
	Version     int64  `json:"version"`      // 群数据版本
	// YUJ-168 / GH #1243: 外部群 H5 邀请 landing 页需要的信任锚点字段。
	// SpaceName 始终下发（即使访问者未登录），只要群挂在某 Space 下；
	// IsExternal 仅在访问者已登录且不是该 Space 成员时为 1，
	// 未登录或同 Space 成员访问时为 0 —— 前端据此决定是否渲染"外部"徽标。
	SpaceName  string `json:"space_name"`  // 群所属 Space 名称（空字符串表示无 Space）
	IsExternal int    `json:"is_external"` // 访问者视角：0=内部/未登录，1=跨 Space 外部访问者
}

func (g groupDetailResp) from(model *Model, memberCount int64, spaceName string, isExternal int) groupDetailResp {
	return groupDetailResp{
		GroupNo:     model.GroupNo,
		Name:        model.Name,
		Notice:      model.Notice,
		Version:     model.Version,
		Forbidden:   model.Forbidden,
		MemberCount: memberCount,
		CreatedAt:   model.CreatedAt.String(),
		UpdatedAt:   model.UpdatedAt.String(),
		SpaceName:   spaceName,
		IsExternal:  isExternal,
	}
}

// 成员详情model
type memberDetailResp struct {
	ID                 uint64 `json:"id"`
	UID                string `json:"uid"`                  // 成员uid
	GroupNo            string `json:"group_no"`             // 群唯一编号
	Name               string `json:"name"`                 // 群成员名称
	Remark             string `json:"remark"`               // 成员备注
	Role               int    `json:"role"`                 // 成员角色
	Version            int64  `json:"version"`              // 版本号
	IsDeleted          int    `json:"is_deleted"`           // 是否删除
	Status             int    `json:"status"`               //成员状态0:正常，2:黑名单
	Vercode            string `json:"vercode"`              // 验证码
	InviteUID          string `json:"invite_uid"`           // 邀请人
	Robot              int    `json:"robot"`                // 机器人
	ForbiddenExpirTime int64  `json:"forbidden_expir_time"` // 禁言时长
	BotAdmin           int    `json:"bot_admin"`            // Bot管理员
	IsExternal         int    `json:"is_external"`          // 是否外部成员
	SourceSpaceID      string `json:"source_space_id"`      // 来源 Space ID
	SourceSpaceName    string `json:"source_space_name"`    // 来源 Space 名称
	// HomeSpaceID / HomeSpaceName 是对齐企微"相对当前 Space 外部"语义的视图字段（YUJ-63 / #1208）。
	// 后端保持 IsExternal / SourceSpaceID / SourceSpaceName 的绝对语义不变，仅用于后端逻辑；
	// 前端在渲染"外部"徽标时应改为比较 home_space_id 与当前查看 Space。
	// 规则：
	//   外部成员 (is_external == 1) → home_space_id = source_space_id
	//   内部成员                     → home_space_id = group.space_id
	HomeSpaceID   string `json:"home_space_id"`   // 成员归属 Space ID（供前端相对视角渲染）
	HomeSpaceName string `json:"home_space_name"` // 成员归属 Space 名称
	// OCTO 实名认证（YUJ-413 Scope B）。根因报告（YUJ-411）发现 Android 气泡 +
	// 群成员列表两条渲染路径都依赖此处 JSON 下发 —— Web 通过 friend/sync 的
	// UserDetailResp 已经有这三字段，Android/iOS 走 WKSDK ChannelMember.extraMap
	// 缓存路径，对应的 /v1/groups/:group_no/members + /membersync 必须同名同型。
	// 未实名用户：realname_verified=false，其它字段 omitempty 省略。
	RealnameVerified   bool   `json:"realname_verified"`
	RealName           string `json:"real_name,omitempty"`
	RealnameVerifiedAt int64  `json:"realname_verified_at,omitempty"`
	CreatedAt          string `json:"created_at"`
	UpdatedAt          string `json:"updated_at"`
}

func (r memberDetailResp) from(model *MemberDetailModel) memberDetailResp {
	return memberDetailResp{
		ID:        uint64(model.Id),
		UID:       model.UID,
		GroupNo:   model.GroupNo,
		Name:      model.Name,
		Remark:    model.Remark,
		Role:      model.Role,
		Version:   model.Version,
		IsDeleted: model.IsDeleted,
		Status:    model.Status,
		// Vercode:            model.Vercode,
		InviteUID:          model.InviteUID,
		Robot:              model.Robot,
		ForbiddenExpirTime: model.ForbiddenExpirTime,
		BotAdmin:           model.BotAdmin,
		IsExternal:         model.IsExternal,
		SourceSpaceID:      model.SourceSpaceID,
		CreatedAt:          model.CreatedAt.String(),
		UpdatedAt:          model.UpdatedAt.String(),
	}
}

// fillSpaceRelatedFields 在一次 space 批量查询中同时填充：
//   - source_space_name （原语义：外部成员的来源 Space 名称）
//   - home_space_id / home_space_name （YUJ-63 / #1208：相对当前 Space 视图字段）
//
// 计算规则（保留原语义，前端在渲染"外部"徽标时改为比较 home_space_id）：
//
//	if member.IsExternal == 1 {
//	    home_space_id   = member.SourceSpaceID
//	    home_space_name = lookup(SourceSpaceID)
//	} else {
//	    home_space_id   = group.SpaceID
//	    home_space_name = lookup(group.SpaceID)
//	}
//
// groupSpaceID 由调用方预先传入可避免重复的 group 表查询（Jerry-Xin review #1209 优化建议）。
// 如果传空串且存在内部成员，函数会用 groupNo 兜底查一次 group 表；
// 调用方若已经查过 group（如 syncMembers），应把 group.SpaceID 显式透传以省掉这次查询。
//
// SQL 开销上限：
//  1. 最多 1 次 group 表查询（仅在 groupSpaceID=="" 且存在内部成员时的兜底）
//  2. 最多 1 次 space 表 WHERE IN 批量查询（合并所有 source_space_id ∪ group_space_id）
//
// fillRealnameFields 批量回填 memberDetailResp 的实名字段（YUJ-413 Scope B）。
//
// 背景：根因报告 YUJ-411 指出 Android 气泡 + 群成员列表 + WKSDK ChannelMember
// 缓存全瞎，是因为 /v1/groups/:group_no/members 和 /membersync 的 response
// 没有序列化 realname_verified / real_name / realname_verified_at。Web 通过
// friend/sync 的 UserDetailResp 已有这三字段，Android/iOS 走 ChannelMember
// extraMap 缓存路径只看 members / membersync，本 handler 不补就永远读不到。
//
// 实现：单次 `user_verification WHERE user_id IN (?)` 批量查，映射到每个 resp。
// 与 modules/user/service.go::GetUserDetails 的批量回填同模式，零 N+1。
//
// 失败处理：仅 log，不阻断主响应链路 —— 实名是增强信息，不能因实名表查询抖动
// 让群成员列表不可用。
func (g *Group) fillRealnameFields(resps []memberDetailResp) {
	if len(resps) == 0 {
		return
	}
	uids := make([]string, 0, len(resps))
	seen := make(map[string]struct{}, len(resps))
	for _, r := range resps {
		if r.UID == "" {
			continue
		}
		if _, ok := seen[r.UID]; ok {
			continue
		}
		seen[r.UID] = struct{}{}
		uids = append(uids, r.UID)
	}
	if len(uids) == 0 {
		return
	}
	infoMap, err := user.QueryVerificationsByUIDs(g.ctx, uids)
	if err != nil {
		g.Warn("批量查询群成员实名认证记录失败（YUJ-413）",
			zap.Error(err), zap.Int("uid_count", len(uids)))
		return
	}
	if len(infoMap) == 0 {
		return
	}
	// 注意：resps 是值切片，必须按 index 回写，不能 range copy。
	for i := range resps {
		info, ok := infoMap[resps[i].UID]
		if !ok || info == nil {
			continue
		}
		resps[i].RealnameVerified = true
		resps[i].RealName = info.RealName
		resps[i].RealnameVerifiedAt = info.RealnameVerifiedAt
	}
}

// 这个上限与 resps 长度无关，保持原先 fillSourceSpaceNames 的无 N+1 属性。
// 函数不会改写 is_external / source_space_id 字段。
func (g *Group) fillSpaceRelatedFields(groupNo, groupSpaceID string, resps []memberDetailResp) {
	if len(resps) == 0 {
		return
	}
	// 只在存在内部成员时才需要群自身 space_id，纯外部群直接走来源 Space 快径。
	hasInternal := false
	for _, r := range resps {
		if r.IsExternal != 1 {
			hasInternal = true
			break
		}
	}
	// 兜底：调用方未传 groupSpaceID 时才落一次 group 表查询。
	if hasInternal && groupSpaceID == "" && groupNo != "" {
		grp, err := g.db.QueryWithGroupNo(groupNo)
		if err != nil {
			g.Warn("查询群 Space 失败", zap.Error(err), zap.String("group_no", groupNo))
		} else if grp != nil {
			groupSpaceID = grp.SpaceID
		}
	}

	// 第 1 步：先定好每个 resp 的 home_space_id，并把所有需要反查名称的 space_id 收入集合。
	idSet := make(map[string]struct{})
	for i := range resps {
		if resps[i].IsExternal == 1 {
			if resps[i].SourceSpaceID != "" {
				resps[i].HomeSpaceID = resps[i].SourceSpaceID
				idSet[resps[i].SourceSpaceID] = struct{}{}
			}
			continue
		}
		if groupSpaceID != "" {
			resps[i].HomeSpaceID = groupSpaceID
			idSet[groupSpaceID] = struct{}{}
		}
	}
	if len(idSet) == 0 {
		return
	}

	// 第 2 步：一次 space 表 WHERE IN 批量查询，拿到所有 id→name 的映射。
	ids := make([]string, 0, len(idSet))
	for id := range idSet {
		ids = append(ids, id)
	}
	var rows []struct {
		SpaceID string `db:"space_id"`
		Name    string `db:"name"`
	}
	_, err := g.ctx.DB().Select("space_id", "name").From("space").
		Where("space_id IN ?", ids).Load(&rows)
	if err != nil {
		g.Warn("查询 Space 名称失败", zap.Error(err))
		return
	}
	nameMap := make(map[string]string, len(rows))
	for _, r := range rows {
		nameMap[r.SpaceID] = r.Name
	}

	// 第 3 步：同一张映射同时回填 source_space_name（原语义）和 home_space_name（新视图字段）。
	for i := range resps {
		if resps[i].IsExternal == 1 && resps[i].SourceSpaceID != "" {
			resps[i].SourceSpaceName = nameMap[resps[i].SourceSpaceID]
		}
		if resps[i].HomeSpaceID != "" {
			resps[i].HomeSpaceName = nameMap[resps[i].HomeSpaceID]
		}
	}
}

type groupReq struct {
	Name        string   `json:"name"`         // 群名
	Members     []string `json:"members"`      // 成员uid
	SpaceID     string   `json:"space_id"`     // Space ID（可选）
	CategoryID  string   `json:"category_id"`  // 群聊分组 ID（可选，需配合 space_id 使用）
	AvatarText  string   `json:"avatar_text"`  // 自定义群头像文字（可选，最多 4 个中文/英文字符；空=用群名派生）
	AvatarColor *int     `json:"avatar_color"` // 自定义群头像色板下标（可选，[0,palette)；不传=按 group_no 派生）
}

func (g groupReq) Check() error {
	if len(g.Members) <= 0 {
		return errors.New("群成员不能为空！")
	}
	return nil
}

// checkAvatar 校验二次弹窗的自定义头像参数，返回越界字段名（供 Details.field）。
// ok=true 表示合法。两者均为可选：avatar_text 空、avatar_color 不传即“未自定义”，
// 渲染时分别回退到群名前 2 字 / ColorForSeed(group_no)。不静默截断，超限直接拒绝。
//
// 哨兵约定（创建 vs 改群刻意不对称）：创建无既有值可清除，故 avatar_color 仅接受
// [0,palette)（-1 在此被拒）；改群额外接受 "-1" / "" 表示清除自定义色回退派生。
// 客户端把已清除状态回灌到创建/克隆流程时需注意：传 -1 会被创建接口拒绝，应改为不传。
func (g groupReq) checkAvatar() (field string, ok bool) {
	if avatarrender.VisibleRuneCount(g.AvatarText) > 4 {
		return "avatar_text", false
	}
	if g.AvatarColor != nil && (*g.AvatarColor < 0 || *g.AvatarColor >= avatarrender.PaletteSize()) {
		return "avatar_color", false
	}
	return "", true
}

// 添加或移除黑名单
type blacklistReq struct {
	Uids []string `json:"uids"` //成员uid
}
type memberAddReq struct {
	Members []string `json:"members"` // 成员uid
}

func (m memberAddReq) Check() error {
	if len(m.Members) <= 0 {
		return errors.New("群成员不能为空！")
	}
	return nil
}

type memberRemoveReq struct {
	Members []string `json:"members"` // 成员uid
}

func (m memberRemoveReq) Check() error {
	if len(m.Members) <= 0 {
		return errors.New("群成员不能为空！")
	}
	return nil
}

// 公开邀请落地页 status 枚举（H5 与 App 共用语义）：
//   - joinable         群存在且可直接入群
//   - invite_required  群开启邀请确认（invite=1），需在 App 内由管理员审批
//   - expired          邀请码不存在或已过期
//   - not_found        群不存在或已解散
//   - external_blocked 群属于某 Space 且 allow_external=0，不允许外部成员加入。
//     扫码鉴权的最终裁决仍由 groupScanJoin 完成，这里只是让 H5 提前给出
//     明确提示、隐藏「加入群聊」按钮，避免用户点了才看到错误。
//   - need_space       登录用户尚未加入任何 Space（产品决策 Direction A /
//     GH #1319）。零 Space 用户不允许通过邀请链接 / 扫码 / 深链入群，
//     避免 group_member{is_external=1, source_space_id=""} 脏数据 +
//     下游 space_filter 隐藏导致的"消息到账但 UI 看不见"问题。
//     三端收到该 status 后应拉起 SpaceGate/JoinSpacePage，Gate 完成后重试入群。
const (
	groupInviteStatusJoinable        = "joinable"
	groupInviteStatusInviteRequired  = "invite_required"
	groupInviteStatusExpired         = "expired"
	groupInviteStatusNotFound        = "not_found"
	groupInviteStatusExternalBlocked = "external_blocked"
	groupInviteStatusNeedSpace       = "need_space"
)

// needSpaceMsg 是所有入群路径在「零 Space 用户」分支下返回给前端的标准文案。
// 统一常量避免 H5 / React / iOS / Android 四端解析漂移。
const needSpaceMsg = "请先加入一个 Space 后再入群"

// groupInvitePage 返回邀请落地页 H5（无需认证，注入 API_BASE_URL）。
// 进群操作仍走 App 内 groupScanJoin，公开页面只展示脱敏预览。
func (g *Group) groupInvitePage(c *wkhttp.Context) {
	htmlBytes, err := os.ReadFile("./assets/web/group_invite.html")
	if err != nil {
		g.Error("加载群邀请落地页失败", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrGroupStoreFailed, nil, nil)
		return
	}
	safeBaseURL := strconv.Quote(g.ctx.GetConfig().External.BaseURL)
	html := strings.Replace(string(htmlBytes), `"{{API_BASE_URL}}"`, safeBaseURL, 1)
	// 注入的 BaseURL 与部署强相关；邀请链接本身也不应被搜索引擎索引或 CDN 缓存。
	c.Header("Cache-Control", "no-store")
	c.Header("X-Robots-Tag", "noindex, nofollow")
	c.Data(http.StatusOK, "text/html; charset=utf-8", []byte(html))
}

// groupInviteDetail 返回邀请码对应的群预览信息（公开接口，per-IP 限流）。
// 仅返回脱敏字段（群名 / 头像路径 / 成员数 / status）；Space 与 allow_external
// 等鉴权延后到 App 内 groupScanJoin 执行。
func (g *Group) groupInviteDetail(c *wkhttp.Context) {
	code := strings.TrimSpace(c.Query("code"))
	if code == "" {
		respondGroupRequestInvalid(c, "code")
		return
	}

	// 1. code 不在 Redis -> expired
	qrcodeContent, err := g.ctx.GetRedisConn().GetString(fmt.Sprintf("%s%s", common.QRCodeCachePrefix, code))
	if err != nil {
		g.Error("获取邀请码缓存失败", zap.Error(err), zap.String("code", code))
		httperr.ResponseErrorL(c, errcode.ErrGroupQueryFailed, nil, nil)
		return
	}
	if qrcodeContent == "" {
		c.Response(gin.H{"status": groupInviteStatusExpired})
		return
	}

	var qrCodeModel common.QRCodeModel
	if err := util.ReadJsonByByte([]byte(qrcodeContent), &qrCodeModel); err != nil {
		g.Error("解析邀请码缓存失败", zap.Error(err), zap.String("code", code))
		c.Response(gin.H{"status": groupInviteStatusExpired})
		return
	}
	if qrCodeModel.Type != common.QRCodeTypeGroup {
		c.Response(gin.H{"status": groupInviteStatusExpired})
		return
	}
	groupNo, _ := qrCodeModel.Data["group_no"].(string)
	if groupNo == "" {
		c.Response(gin.H{"status": groupInviteStatusExpired})
		return
	}

	// 2. 群不存在或已解散 -> not_found
	groupModel, err := g.db.QueryWithGroupNo(groupNo)
	if err != nil {
		g.Error("查询群资料失败", zap.Error(err), zap.String("group_no", groupNo))
		httperr.ResponseErrorL(c, errcode.ErrGroupQueryFailed, nil, nil)
		return
	}
	if groupModel == nil || groupModel.Status == GroupStatusDisband {
		c.Response(gin.H{"status": groupInviteStatusNotFound})
		return
	}

	memberCount, err := g.db.QueryMemberCount(groupNo)
	if err != nil {
		g.Error("查询群成员数失败", zap.Error(err), zap.String("group_no", groupNo))
		httperr.ResponseErrorL(c, errcode.ErrGroupQueryFailed, nil, nil)
		return
	}

	// YUJ-39: 可选鉴权。本端点不挂 AuthMiddleware（面向公开 H5 落地页），
	// 但如果请求带了合法 token，我们也要把登录态吃下来，让 Space 内部成员
	// 看到 joinable 而不是 external_blocked（否则 PR#1174 对 authorize 的
	// 放行修复在 UI 层被 detail 的 external_blocked 按钮隐藏所抵消，
	// 同 Space 成员根本看不到「加入群聊」按钮）。
	// 无 token / token 无效 / 解析失败 → loginUID="" → 行为严格对齐既有公开
	// 预览路径，不破坏未登录访问者的 external_blocked 硬拦截语义。
	var loginUID string
	if token := strings.TrimSpace(c.GetHeader("token")); token != "" {
		raw, cacheErr := g.ctx.Cache().Get(g.ctx.GetConfig().Cache.TokenCachePrefix + token)
		if cacheErr == nil && strings.TrimSpace(raw) != "" {
			// AuthMiddleware 的 token value 由 pkg/auth 收口编解码：v2 JSON 或
			// 灰度期残留的 "uid@name[@role]"。auth.Decode 返回 ErrInvalidToken
			// 时安全降级为未登录访问者，沿用 external_blocked 硬拦截语义。
			if info, decodeErr := auth.Decode(raw); decodeErr == nil {
				loginUID = info.UID
			}
		}
	}

	// 只对真实挂 Space 的群做一次成员校验；校验失败降级为 !inSameSpace，
	// 等价于"跨 Space 访问者"，保持外部拦截语义的安全默认值。
	inSameSpace := false
	if groupModel.SpaceID != "" && loginUID != "" {
		inSpace, checkErr := spacepkg.CheckMembership(g.ctx.DB(), groupModel.SpaceID, loginUID)
		if checkErr != nil {
			g.Warn("检查 Space 成员失败", zap.Error(checkErr), zap.String("group_no", groupNo))
		} else {
			inSameSpace = inSpace
		}
	}

	// 3/4. invite=1 -> invite_required；否则 joinable
	// 5. 群属于某 Space 且 allow_external=0 且访问者不是该 Space 成员
	//    -> external_blocked（优先级高于 joinable / invite_required，
	//    因为这是更强的硬拦截：外部用户根本不可能入群，让 H5 落地页直接给出
	//    明确提示，避免点了「加入群聊」再被 groupScanJoin 拒绝）。
	//    同 Space 成员保留 joinable/invite_required 状态，和 groupInviteAuthorize
	//    的放行路径对齐（PR#1174 / YUJ-38 fix）。
	// 6. GH #1319 / Direction A：登录态 + 零 Space 用户 -> need_space。
	//    优先级**高于** external_blocked，因为零 Space 是账号级问题（加
	//    Space 后重试即可），比群级 external_blocked 更根本、更可操作；
	//    前端看到 need_space 应直接引导加 Space 而不是显示「该群不允许外部」。
	//    未登录访问者（loginUID==""）不触发，保持公共预览体验。
	status := groupInviteStatusJoinable
	if groupModel.Invite == 1 {
		status = groupInviteStatusInviteRequired
	}
	if groupModel.SpaceID != "" && groupModel.AllowExternal == 0 && !inSameSpace {
		status = groupInviteStatusExternalBlocked
	}
	if loginUID != "" && spacemod.GetUserDefaultSpaceID(g.ctx, loginUID) == "" {
		status = groupInviteStatusNeedSpace
	}

	// YUJ-168 / GH #1243: 为公开 H5 landing 提供信任锚点字段。
	// - space_name 始终下发（前端判断非空才渲染"来自 xx"）。
	// - is_external：0=未登录 / 同 Space 成员（内部视角），1=跨 Space 登录者。
	// YUJ-211 / GH #1277: 额外下发 space_id（公共群 / 查询失败时降级为空串），
	// 让 H5 落地页能显示「📁 属于 "XX" Space」归属行，解决扫码加跨 Space 群
	// 后用户找不到群挂在哪个 Space 的问题。空串时前端跳过渲染，保持向后兼容
	// （旧客户端忽略未知字段不会报错）。
	spaceName, _ := spacepkg.GetSpaceName(g.ctx.DB(), groupModel.SpaceID)
	isExternal := 0
	if groupModel.SpaceID != "" && loginUID != "" && !inSameSpace {
		isExternal = 1
	}

	c.Response(gin.H{
		"status":       status,
		"group_no":     groupNo,
		"group_name":   groupModel.Name,
		"avatar":       fmt.Sprintf("groups/%s/avatar", groupNo),
		"member_count": memberCount,
		"space_id":     groupModel.SpaceID,
		"space_name":   spaceName,
		"is_external":  isExternal,
	})
}

// groupInviteAuthorize 把公开邀请码（二维码 UUID）换成当前登录用户的入群 auth_code。
// 这是 Web H5 公开落地页「加入群聊」按钮的前置步骤：
//
//  1. 落地页通过 GET /v1/group/invite/detail?code=xxx 拿到脱敏预览
//  2. 已登录用户点击「加入群聊」→ POST /v1/group/invite/authorize?code=xxx
//     本端口在 Redis 里生成一条和扫码预检等价的 auth_code 记录
//  3. 前端拿到 auth_code 后直接调用 GET /v1/groups/:group_no/scanjoin?auth_code=xxx
//     完成入群（包含外部成员识别 / allow_external / invite 审批等完整鉴权链路）
//
// 注意：本接口本身只生成 auth_code，不真正入群；所有业务规则（是否在群、外部成员
// 是否允许、是否邀请审批）都交给 groupScanJoin，避免双份鉴权漂移。
//
// 但以下三类情况会在此端点提前短路，避免让 H5 用户看到一条令人困惑的错误
// 或无谓往 Redis 写 30min TTL 的 auth_code（对齐 qrcode/api.go handleJoinGroup 的预检）。
// 短路优先级**严格**如下（顺序必须与下方代码一致，不要随手调换）：
//  1. 群属于某 Space 且 allow_external=0 且扫码者不是 Space 成员：返回 {status: "external_blocked", group_no}
//     （同 Space 成员继续走正常流程，避免误杀）
//  2. 已经在群内：返回 {already_member: true, group_no}
//     （必须排在 invite 之前：开启邀请审批的群里，已是群成员的用户扫码应走「已在群内」而不是被 400 拦截）
//  3. invite=1：返回 HTTP 400「邀请模式」错误（与 groupScanJoin 拒绝文案一致）
func (g *Group) groupInviteAuthorize(c *wkhttp.Context) {
	loginUID := c.GetLoginUID()
	if loginUID == "" {
		respondGroupNotLoggedIn(c)
		return
	}
	// GH #1319 / Direction A：零 Space 用户禁止入群。
	// 放在 loginUID 校验之后、所有 Redis / DB 查询之前，避免写入脏 auth_code
	// 以及后续 groupScanJoin 落出 group_member{is_external=1, source_space_id=""} 记录。
	// 优先级**必须**高于 external_blocked / already_member / invite，因为这是用户侧的
	// 账号状态问题（加完 Space 后重试即可），而不是群本身的属性问题。
	if spacemod.GetUserDefaultSpaceID(g.ctx, loginUID) == "" {
		c.Response(gin.H{
			"status": groupInviteStatusNeedSpace,
			"msg":    needSpaceMsg,
		})
		return
	}
	code := strings.TrimSpace(c.Query("code"))
	if code == "" {
		respondGroupRequestInvalid(c, "code")
		return
	}

	qrcodeContent, err := g.ctx.GetRedisConn().GetString(fmt.Sprintf("%s%s", common.QRCodeCachePrefix, code))
	if err != nil {
		g.Error("获取邀请码缓存失败", zap.Error(err), zap.String("code", code))
		httperr.ResponseErrorL(c, errcode.ErrGroupQueryFailed, nil, nil)
		return
	}
	if qrcodeContent == "" {
		httperr.ResponseErrorL(c, errcode.ErrGroupInviteExpired, nil, nil)
		return
	}

	var qrCodeModel common.QRCodeModel
	if err := util.ReadJsonByByte([]byte(qrcodeContent), &qrCodeModel); err != nil {
		g.Error("解析邀请码缓存失败", zap.Error(err), zap.String("code", code))
		httperr.ResponseErrorL(c, errcode.ErrGroupInviteExpired, nil, nil)
		return
	}
	if qrCodeModel.Type != common.QRCodeTypeGroup {
		httperr.ResponseErrorL(c, errcode.ErrGroupInviteExpired, nil, nil)
		return
	}
	groupNo, _ := qrCodeModel.Data["group_no"].(string)
	if groupNo == "" {
		httperr.ResponseErrorL(c, errcode.ErrGroupInviteExpired, nil, nil)
		return
	}
	generator, _ := qrCodeModel.Data["generator"].(string)
	if strings.TrimSpace(generator) == "" {
		httperr.ResponseErrorL(c, errcode.ErrGroupInviteExpired, nil, nil)
		return
	}

	groupModel, err := g.db.QueryWithGroupNo(groupNo)
	if err != nil {
		g.Error("查询群资料失败", zap.Error(err), zap.String("group_no", groupNo))
		httperr.ResponseErrorL(c, errcode.ErrGroupQueryFailed, nil, nil)
		return
	}
	if groupModel == nil || groupModel.Status == GroupStatusDisband {
		httperr.ResponseErrorL(c, errcode.ErrGroupNotFound, nil, nil)
		return
	}
	// 群属于某 Space 且 allow_external=0：提前拦截，和 handleJoinGroup 的预检保持一致。
	// 注意：external_blocked 判定必须放在 invite 判定之前，与 groupInviteDetail 的优先级对齐
	// （detail 那边也是 external_blocked 赋值覆盖 invite_required）。
	// 同 Space 成员即使群禁止外部、开启邀请审批，也应继续走正常 authorize 流程，
	// 由 groupScanJoin 负责最终的邀请模式判定。
	if groupModel.SpaceID != "" && groupModel.AllowExternal == 0 {
		inSpace, checkErr := spacepkg.CheckMembership(g.ctx.DB(), groupModel.SpaceID, loginUID)
		if checkErr != nil {
			g.Error("检查 Space 成员失败", zap.Error(checkErr), zap.String("group_no", groupNo))
			httperr.ResponseErrorL(c, errcode.ErrGroupQueryFailed, nil, nil)
			return
		}
		if !inSpace {
			// 跨 Space 用户：H5 拦截，与 detail/qrcode 预检行为一致。
			c.Response(gin.H{
				"status":   groupInviteStatusExternalBlocked,
				"group_no": groupNo,
			})
			return
		}
		// 同 Space 成员：继续走正常流程（可能落到 already_member / invite_required / 直接生成 auth_code）。
	}
	// 已经在群内：与 qrcode/api.go handleJoinGroup 对齐，返回 already_member=true，
	// 不生成 auth_code（否则 scanjoin 只会回「已经在群内」，并白占一条 Redis TTL）。
	// H5 收到后展示「你已在此群内」并给跳转入口。
	// 注意：already_member 判定必须放在 invite 判定之前 —— 开启邀请审批的群，
	// 已是群成员的用户扫码仍应走「已在群内」分支，而不是被 400「邀请模式」拦截。
	existMember, err := g.db.ExistMember(loginUID, groupNo)
	if err != nil {
		g.Error("查询群成员失败", zap.Error(err), zap.String("group_no", groupNo))
		httperr.ResponseErrorL(c, errcode.ErrGroupQueryFailed, nil, nil)
		return
	}
	if existMember {
		c.Response(gin.H{
			"group_no":       groupNo,
			"already_member": true,
		})
		return
	}
	if groupModel.Invite == 1 {
		// 与 groupScanJoin 保持一致：开启邀请审批的群不支持直接扫码入群，
		// 也不在 H5 落地页生成 auth_code（避免后续 scanjoin 失败时的语义含糊）。
		httperr.ResponseErrorL(c, errcode.ErrGroupInviteModeCannotJoin, nil, nil)
		return
	}

	authCode := util.GenerUUID()
	err = g.ctx.GetRedisConn().SetAndExpire(fmt.Sprintf("%s%s", common.AuthCodeCachePrefix, authCode), util.ToJson(map[string]interface{}{
		"group_no":  groupNo,
		"generator": generator,
		"scaner":    loginUID,
		"type":      common.AuthCodeTypeJoinGroup,
	}), time.Minute*30)
	if err != nil {
		g.Error("生成入群授权码失败", zap.Error(err), zap.String("group_no", groupNo))
		httperr.ResponseErrorL(c, errcode.ErrGroupStoreFailed, nil, nil)
		return
	}
	c.Response(gin.H{
		"group_no":  groupNo,
		"auth_code": authCode,
	})
}
