package webhook

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-server/modules/user"
	"github.com/Mininglamp-OSS/octo-server/pkg/pushcache"
	"github.com/Mininglamp-OSS/octo-server/pkg/space"
	"go.uber.org/zap"
)

// resolvePushChannelID 计算推送 payload 中 channel_id 的最终取值。
// 对 person 频道，WuKongIM 传入的 ChannelID 是发送者视角下的对端（即接收者自身 UID），
// 客户端需要用 "对端 UID" 定位会话，因此需要把 peer 改写成 FromUID，同时保留 space 前缀。
// 群聊 / 子区在所有成员视角下 channel_id 一致，原样返回。
func resolvePushChannelID(channelType uint8, channelID, fromUID string) string {
	if channelType != common.ChannelTypePerson.Uint8() || fromUID == "" {
		return channelID
	}
	spaceID, _ := space.ParseChannelID(channelID)
	return space.BuildChannelID(spaceID, fromUID)
}

// EventMsgOffline 离线消息
const EventMsgOffline = "msg.offline"

// EventOnlineStatus 在线状态
const EventOnlineStatus = "user.onlinestatus"

// EventMsgNotify 消息通知 (所有消息)
const EventMsgNotify = "msg.notify"

const (
	nameCachePrefix string = "name:"
)

type PayloadInfo struct {
	Title   string
	Content string
	Badge   int

	// ------ 以下是rtc推送需要 ------
	IsVideoCall bool   // 是否是rtc消息
	FromUID     string // rtc消息需要
	CallType    common.RTCCallType
	Operation   string

	SpaceID string // 推送所属 Space

	// 路由字段：客户端点击通知跳转到对应会话所需
	ChannelID   string
	ChannelType uint8
	MessageSeq  uint32
}

func (p *PayloadInfo) toPayload() Payload {
	var payload Payload
	var basePayload = BasePayload{
		title:   p.Title,
		content: p.Content,
		badge:   p.Badge,
	}
	if p.IsVideoCall {
		payload = &BaseRTCPayload{
			BasePayload: basePayload,
			fromUID:     p.FromUID,
			operation:   p.Operation,
			callType:    p.CallType,
		}
	} else {
		payload = &basePayload
	}
	return payload
}

// ParsePushInfo 解析推送信息 获得title,content,badge
func ParsePushInfo(msgResp msgOfflineNotify, ctx *config.Context, toUser *user.Resp) (*PayloadInfo, error) {
	toUID := toUser.UID
	fromName, err := getFromName(msgResp, ctx)
	if err != nil {
		return nil, err
	}

	// 红点
	badge, err := getUserBadge(toUID, ctx)
	if err != nil {
		log.Warn("获取用户红点失败", zap.Error(err), zap.String("uid", toUID))
	}

	payloadInfo := &PayloadInfo{
		Badge: badge,
	}

	content, err := getMessageAlert(msgResp, toUser, ctx)
	if err != nil {
		return nil, err
	}

	if msgResp.ChannelType == common.ChannelTypePerson.Uint8() {
		payloadInfo.Title = fromName
	} else if msgResp.ChannelType == common.ChannelTypeCommunityTopic.Uint8() {
		threadTitle, err := getThreadPushTitle(msgResp, ctx)
		if err != nil {
			log.Error("获取子区推送标题失败", zap.Error(err), zap.String("channel_id", msgResp.ChannelID))
			return nil, err
		}
		payloadInfo.Title = threadTitle
		// 子区消息的外部发件人同样需要 @SpaceName 外部标识
		// （YUJ-172 / octo-server#1251）。子区所在群号从 ChannelID 前缀解析，
		// 解析失败直接降级为不加后缀。
		var threadGroupNo string
		if groupNo, _, ok := parseThreadChannelID(msgResp.ChannelID); ok {
			threadGroupNo = groupNo
		}
		content = formatGroupPushBody(ctx, threadGroupNo, msgResp.FromUID, fromName, content)
	} else {
		var groupName string
		groupName, err = getCachedGroupName(msgResp.ChannelID, ctx)
		if err != nil {
			log.Error("获取群名失败！", zap.Error(err), zap.String("group_no", msgResp.ChannelID))
			return nil, err
		}
		payloadInfo.Title = groupName
		content = formatGroupPushBody(ctx, msgResp.ChannelID, msgResp.FromUID, fromName, content)
	}
	payloadInfo.Content = content
	payloadInfo.SpaceID = msgResp.SpaceID
	payloadInfo.ChannelID = resolvePushChannelID(msgResp.ChannelType, msgResp.ChannelID, msgResp.FromUID)
	payloadInfo.ChannelType = msgResp.ChannelType
	payloadInfo.MessageSeq = msgResp.MessageSeq

	return payloadInfo, nil
}

func getFromName(msgResp msgOfflineNotify, ctx *config.Context) (string, error) {
	fromName, err := getAndCacheShowNameForFromUID(msgResp, ctx)
	if err != nil {
		log.Error("获取fromUID对应的名称失败！", zap.Error(err))
		return "", err
	}
	return fromName, nil
}

func getMessageAlert(msg msgOfflineNotify, toUser *user.Resp, ctx *config.Context) (string, error) {
	setting := config.SettingFromUint8(msg.Setting)
	if msg.PayloadMap == nil || setting.Signal || !ctx.GetConfig().Push.ContentDetailOn || toUser.MsgShowDetail == 0 {
		if msg.PayloadMap != nil && msg.PayloadMap["cmd"] != nil {
			return "您收到新的来电", nil
		}
		return "您有一条新的消息", nil
	}

	var alert string
	var contentTypeInt64 int64
	if num, ok := msg.PayloadMap["type"].(json.Number); ok {
		contentTypeInt64, _ = num.Int64()
	}
	contentType := common.ContentType(contentTypeInt64)
	switch contentType {
	case common.Text:
		if content, ok := msg.PayloadMap["content"].(string); ok {
			alert = content
		}
	case common.Image:
		alert = "[图片]"
	case common.GIF:
		alert = "[GIF]"
	case common.Voice:
		alert = "[语音]"
	case common.Video:
		alert = "[视频]"
	case common.Card:
		alert = "[名片]"
	case common.File:
		alert = "[文件]"
	case common.Location:
		alert = "[位置]"
	case common.VectorSticker:
		alert = "[动画表情]"
	case common.EmojiSticker:
		alert = "[emoji表情]"
	case common.MultipleForward:
		alert = "[聊天记录]"
	case common.RichText:
		// 图文混排 RichText(=14)：推送正文取 server 已生成的权威 plain（含 [图片]
		// 占位）。GetRichTextDisplayText 优先用 payload.plain，缺失时现场遍历
		// content 兜底，再不行回退到 GetDisplayText(=「富文本消息」)。这里走的是
		// 「存储后展示路径」，payload 已在派发出口经 EnsurePlain 用 content 重算
		// 覆盖端上 plain，故信任 plain 是正确且高效的。
		alert = common.GetRichTextDisplayText(msg.Payload)
	}
	return alert, nil
}

var (
	webhookDB     *DB
	webhookDBOnce sync.Once
)

// getWebhookDB returns the singleton webhookDB instance, initializing it thread-safely on first call.
func getWebhookDB(ctx *config.Context) *DB {
	webhookDBOnce.Do(func() {
		webhookDB = NewDB(ctx.DB())
	})
	return webhookDB
}

// 获取和缓存发送者的显示名称
// fallbackSyntheticSenderName 在 GetThirdName 取不到常用名（name 为空）时，尝试解析合成
// 发送者的展示名。典型场景：incoming webhook 的虚拟发送者（iwh_xxx）在 user 表里没有行，
// 直查 user 表得到空名，推送就没有发件人名字。这里复用 user 模块基于 BussDataSource 的
// 解析器(不在推送层硬编码 iwh_ 前缀)。
//
// 返回 cacheable 表示结果是否可写入缓存：解析发生瞬时故障（datasource 报错）时返回原值
// 且 cacheable=false，让下一次推送重试，避免把空名冻结到 TTL（最长 NameCacheExpire）。
// 解析成功（含"无人处理"返回空名，如已删除 webhook）则 cacheable=true。失败不影响推送。
func fallbackSyntheticSenderName(ctx *config.Context, fromUID, name string) (resolved string, cacheable bool) {
	if name != "" {
		return name, true
	}
	resolved, err := user.ResolveWebhookDisplayName(ctx, fromUID)
	if err != nil {
		log.Warn("解析合成发送者名失败", zap.String("from_uid", fromUID), zap.Error(err))
		return name, false
	}
	return resolved, true
}

// repairEmptyCachedName 处理"缓存命中但 name 为空"的存量场景：修复前已对某合成发送者
// （如 iwh_）缓存过空名的条目，命中后会直接返回空名、绕过 miss 分支的兜底，导致存量
// iwh_ 发送者在旧缓存 TTL 到期前仍显示空发件人名。这里在命中时同样补一次兜底解析，并
// 就地修复缓存的 name 字段（只改 name，不动 remark/name_in_group，也不重置 TTL），根治
// 存量、无需等旧 TTL。解析失败或无人处理则保持空名（下次再试），不影响本次推送。
func repairEmptyCachedName(ctx *config.Context, key, fromUID, name string) string {
	if name != "" {
		return name
	}
	resolved, cacheable := fallbackSyntheticSenderName(ctx, fromUID, name)
	if cacheable && resolved != "" {
		if err := ctx.GetRedisConn().Hset(key, "name", resolved); err != nil {
			log.Warn("修复发送者名缓存失败", zap.String("key", key), zap.Error(err))
		}
	}
	return resolved
}

func getAndCacheShowNameForFromUID(msgResp msgOfflineNotify, ctx *config.Context) (string, error) {
	db := getWebhookDB(ctx)

	var name, // 发送者常用名
		remark, // 接收者对发送者的备注
		nameInGroup string // 如果是群聊则发送者在群里的备注
	if msgResp.ChannelType == common.ChannelTypePerson.Uint8() {
		key := fmt.Sprintf("%s%s-%s", nameCachePrefix, msgResp.FromUID, msgResp.ToUID)
		nameMap, err := ctx.GetRedisConn().Hgetall(key)
		if err != nil {
			log.Error("从缓存中获取名字失败！", zap.Error(err))
			return "", err
		}
		if len(nameMap) > 0 { // 存在缓存，直接取出
			name = nameMap["name"]
			remark = nameMap["remark"]
			name = repairEmptyCachedName(ctx, key, msgResp.FromUID, name)
		} else { // 不存在缓存，从DB获取，然后再缓存
			name, remark, _, err = db.GetThirdName(msgResp.FromUID, msgResp.ToUID, "")
			if err != nil {
				return "", err
			}
			var cacheable bool
			name, cacheable = fallbackSyntheticSenderName(ctx, msgResp.FromUID, name)
			if cacheable {
				err = ctx.GetRedisConn().Hmset(key, "name", name, "remark", remark)
				if err != nil {
					log.Error("缓存名字失败！", zap.Error(err))
					return "", err
				}
				err = ctx.GetRedisConn().Expire(key, ctx.GetConfig().Cache.NameCacheExpire)
				if err != nil {
					log.Error("设置过期时间失败！", zap.String("key", key), zap.Error(err))
					return "", err
				}
			}
		}
	} else {
		key := fmt.Sprintf("%s%s-%s@%s", nameCachePrefix, msgResp.FromUID, msgResp.ToUID, msgResp.ChannelID)
		nameMap, err := ctx.GetRedisConn().Hgetall(key)
		if err != nil {
			log.Error("从缓存中获取名字失败！", zap.Error(err))
			return "", err
		}
		if len(nameMap) > 0 { // 存在缓存，直接取出
			name = nameMap["name"]
			remark = nameMap["remark"]
			nameInGroup = nameMap["name_in_group"]
			name = repairEmptyCachedName(ctx, key, msgResp.FromUID, name)
		} else { // 不存在缓存，从DB获取，然后再缓存
			name, remark, nameInGroup, err = db.GetThirdName(msgResp.FromUID, msgResp.ToUID, msgResp.ChannelID)
			if err != nil {
				return "", err
			}
			var cacheable bool
			name, cacheable = fallbackSyntheticSenderName(ctx, msgResp.FromUID, name)
			if cacheable {
				err = ctx.GetRedisConn().Hmset(key, "name", name, "remark", remark, "name_in_group", nameInGroup)
				if err != nil {
					log.Error("缓存名字失败！", zap.Error(err))
					return "", err
				}
				err = ctx.GetRedisConn().Expire(key, ctx.GetConfig().Cache.NameCacheExpire)
				if err != nil {
					log.Error("设置过期时间失败！", zap.String("key", key), zap.Error(err))
					return "", err
				}
			}
		}

	}
	if remark != "" { // 优先返回备注
		return remark, nil
	}
	if nameInGroup != "" {
		return nameInGroup, nil
	}
	return name, nil
}

// getCachedGroupName 读取群名（用作推送标题），命中缓存直接返回，否则回源 DB 并写入
// 带 TTL 的缓存。缓存 key 由 pushcache 统一定义，群改名时由 modules/group 主动失效，
// 避免推送标题在 TTL 到期前一直沿用旧名。
func getCachedGroupName(groupNo string, ctx *config.Context) (string, error) {
	key := pushcache.GroupNameKey(groupNo)
	groupName, err := ctx.GetRedisConn().GetString(key)
	if err != nil {
		return "", err
	}
	if groupName != "" {
		return groupName, nil
	}

	groupName, err = getWebhookDB(ctx).GetGroupName(groupNo)
	if err != nil {
		return "", err
	}
	if err = ctx.GetRedisConn().Set(key, groupName); err != nil {
		return "", err
	}
	// Expire 失败不影响本次取名：群名已成功取到（并已写入缓存，只是少了 TTL）。失败仅告警、
	// 继续返回，避免一次瞬时 Redis 错误把整条推送打挂（群推送和子区推送共用本函数）。
	if err = ctx.GetRedisConn().Expire(key, ctx.GetConfig().Cache.NameCacheExpire); err != nil {
		log.Warn("设置群名过期时间失败", zap.String("key", key), zap.Error(err))
	}
	return groupName, nil
}

// parseThreadChannelID 解析子区 ChannelID，格式：groupNo____shortID
func parseThreadChannelID(channelID string) (groupNo, shortID string, ok bool) {
	parts := strings.SplitN(channelID, "____", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// BuildThreadTitle 构建子区推送标题，格式：#子区名,群名
func BuildThreadTitle(channelID, threadName, groupName string) string {
	if threadName != "" && groupName != "" {
		return fmt.Sprintf("#%s,%s", threadName, groupName)
	} else if threadName != "" {
		return fmt.Sprintf("#%s", threadName)
	} else if groupName != "" {
		return groupName
	}
	return channelID
}

// getThreadPushTitle 组装子区推送标题，格式：#子区名,群名。
//
// 子区名和群名分别独立缓存（见 getCachedThreadName / getCachedGroupName），标题在读取时
// 现场拼接，而不是把拼好的整串冻结进缓存。这样群改名时只需失效 groupName: 单个 key，群推送
// 标题和该群下所有子区推送标题就会一起刷新，无需遍历子区、也不依赖 Redis KEYS 扫描。
func getThreadPushTitle(msgResp msgOfflineNotify, ctx *config.Context) (string, error) {
	groupNo, shortID, ok := parseThreadChannelID(msgResp.ChannelID)
	if !ok {
		return msgResp.ChannelID, nil
	}

	threadName, err := getCachedThreadName(msgResp.ChannelID, groupNo, shortID, ctx)
	if err != nil {
		return "", err
	}
	groupName, err := getCachedGroupName(groupNo, ctx)
	if err != nil {
		return "", fmt.Errorf("failed to get group name: %w", err)
	}

	return BuildThreadTitle(msgResp.ChannelID, threadName, groupName), nil
}

// getCachedThreadName 读取子区名，命中缓存直接返回，否则回源 DB 并写入带 TTL 的缓存。
// 缓存 key 由 pushcache 统一定义，子区改名时由 modules/thread 主动失效。
func getCachedThreadName(channelID, groupNo, shortID string, ctx *config.Context) (string, error) {
	key := pushcache.ThreadNameKey(channelID)
	threadName, err := ctx.GetRedisConn().GetString(key)
	if err != nil {
		return "", err
	}
	if threadName != "" {
		return threadName, nil
	}

	threadName, err = getWebhookDB(ctx).GetThreadName(groupNo, shortID)
	if err != nil {
		return "", fmt.Errorf("failed to get thread name: %w", err)
	}
	if err = ctx.GetRedisConn().Set(key, threadName); err != nil {
		return "", err
	}
	if err = ctx.GetRedisConn().Expire(key, ctx.GetConfig().Cache.NameCacheExpire); err != nil {
		log.Warn("设置子区名过期时间失败", zap.String("key", key), zap.Error(err))
	}

	return threadName, nil
}

func getUserBadge(uid string, ctx *config.Context) (int, error) {
	badge, err := ctx.GetRedisConn().Hincrby(common.UserDeviceBadgePrefix, uid, 1)
	if err != nil {
		log.Error("获取红点数失败！", zap.Error(err))
		return 0, err
	}
	return int(badge), nil
}

// formatGroupPushBody 组装群/子区推送 body：
//   - 同 Space 发件人：`<发件人>：<content>` （与历史行为一致，无回归）
//   - 跨 Space 发件人：`<发件人> @<space_name>：<content>` （YUJ-172 / octo-server#1251）
//
// 任何数据缺失/依赖错误都降级为同 Space 路径，永不阻塞推送。
func formatGroupPushBody(ctx *config.Context, groupNo, fromUID, fromName, content string) string {
	suffix := resolveSenderSpaceLabel(getExternalMarkerCache(ctx), groupNo, fromUID)
	if suffix == "" {
		return fmt.Sprintf("%s：%s", fromName, content)
	}
	return fmt.Sprintf("%s @%s：%s", fromName, suffix, content)
}
