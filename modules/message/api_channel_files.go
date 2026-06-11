package message

import (
	"encoding/json"
	"errors"
	"net/url"
	"path/filepath"
	"strings"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/modules/thread"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	"go.uber.org/zap"
)

type fileCategory string

const (
	fileCategoryAll      fileCategory = "all"
	fileCategoryDocument fileCategory = "document"
	fileCategoryImage    fileCategory = "image"
	fileCategoryVideo    fileCategory = "video"
	fileCategoryArchive  fileCategory = "archive"
	fileCategoryCode     fileCategory = "code"
	fileCategoryOther    fileCategory = "other"
)

var documentExts = map[string]bool{
	".doc": true, ".docx": true, ".xls": true, ".xlsx": true,
	".ppt": true, ".pptx": true, ".pdf": true, ".txt": true,
	".csv": true, ".rtf": true, ".md": true, ".odt": true, ".ods": true,
}

var archiveExts = map[string]bool{
	".zip": true, ".rar": true, ".7z": true, ".tar": true, ".gz": true,
	".bz2": true, ".xz": true, ".tgz": true,
}

var codeExts = map[string]bool{
	".json": true, ".xml": true, ".yaml": true, ".yml": true,
	".html": true, ".js": true, ".ts": true, ".py": true,
	".go": true, ".java": true, ".css": true, ".sh": true,
	".sql": true, ".rb": true, ".rs": true, ".c": true,
	".cpp": true, ".h": true, ".swift": true, ".kt": true,
	".proto": true, ".toml": true, ".ini": true, ".conf": true,
}

func categoryFromFilename(name string) fileCategory {
	ext := strings.ToLower(filepath.Ext(name))
	if ext == "" {
		return fileCategoryOther
	}
	if documentExts[ext] {
		return fileCategoryDocument
	}
	if archiveExts[ext] {
		return fileCategoryArchive
	}
	if codeExts[ext] {
		return fileCategoryCode
	}
	return fileCategoryOther
}

func payloadTypesForCategory(cat fileCategory) []int {
	switch cat {
	case fileCategoryImage:
		return []int{common.Image.Int(), common.GIF.Int()}
	case fileCategoryVideo:
		return []int{common.Video.Int()}
	case fileCategoryDocument, fileCategoryArchive, fileCategoryCode:
		return []int{common.File.Int()}
	default:
		return []int{common.Image.Int(), common.GIF.Int(), common.Video.Int(), common.File.Int()}
	}
}

func needsExtFilter(cat fileCategory) bool {
	return cat == fileCategoryDocument || cat == fileCategoryArchive || cat == fileCategoryCode
}

type channelFilesReq struct {
	ChannelID   string `json:"channel_id"`
	ChannelType uint8  `json:"channel_type"`
	Category    string `json:"category"`
	Keyword     string `json:"keyword"`
	Page        int    `json:"page"`
	Limit       int    `json:"limit"`
}

var validCategories = map[string]bool{
	"all": true, "document": true, "image": true,
	"video": true, "archive": true, "code": true,
}

func (r *channelFilesReq) check() error {
	if strings.TrimSpace(r.ChannelID) == "" {
		return errors.New("频道ID不能为空！")
	}
	if r.ChannelType == 0 {
		return errors.New("频道类型不能为空！")
	}
	if r.Category != "" && !validCategories[r.Category] {
		return errors.New("不支持的文件分类")
	}
	return nil
}

type channelFileResp struct {
	MessageID   int64  `json:"message_id"`
	MessageSeq  uint32 `json:"message_seq"`
	FromUID     string `json:"from_uid"`
	FromName    string `json:"from_name"`
	ChannelID   string `json:"channel_id"`
	ChannelType uint8  `json:"channel_type"`
	Category    string `json:"category"`
	Name        string `json:"name"`
	URL         string `json:"url"`
	Size        int64  `json:"size"`
	Width       int    `json:"width,omitempty"`
	Height      int    `json:"height,omitempty"`
	Duration    int    `json:"duration,omitempty"`
	Timestamp   int32  `json:"timestamp"`
}

type channelFilesResp struct {
	Total   int64              `json:"total"`
	Page    int                `json:"page"`
	Limit   int                `json:"limit"`
	HasMore bool               `json:"has_more"`
	Files   []*channelFileResp `json:"files"`
}

func (m *Message) channelFiles(c *wkhttp.Context) {
	loginUID := c.GetLoginUID()
	var req channelFilesReq
	if err := c.BindJSON(&req); err != nil {
		m.Error("数据格式有误！", zap.Error(err))
		respondMessageRequestInvalid(c, "")
		return
	}
	if err := req.check(); err != nil {
		respondMessageRequestInvalid(c, "")
		return
	}
	if req.Page <= 0 {
		req.Page = 1
	}
	if req.Limit <= 0 {
		req.Limit = 20
	}
	if req.Limit > 100 {
		req.Limit = 100
	}
	if req.Category == "" {
		req.Category = string(fileCategoryAll)
	}

	cat := fileCategory(req.Category)
	payloadTypes := payloadTypesForCategory(cat)

	// 群聊/话题校验成员身份
	if req.ChannelType == common.ChannelTypeGroup.Uint8() || req.ChannelType == common.ChannelTypeCommunityTopic.Uint8() {
		groupNo := req.ChannelID
		// isTopic 为真时走子区父群门禁，需排除黑名单(ExistMemberActive)；
		// GROUP 分支保留 ExistMember 既有语义，避免误杀正常群成员。
		isTopic := req.ChannelType == common.ChannelTypeCommunityTopic.Uint8()
		if isTopic {
			parentGroupNo, _, perr := thread.ParseChannelID(req.ChannelID)
			if perr != nil {
				respondMessageRequestInvalid(c, "channel_id")
				return
			}
			groupNo = parentGroupNo
		}
		var isMember bool
		var err error
		if isTopic {
			isMember, err = m.groupService.ExistMemberActive(groupNo, loginUID)
		} else {
			isMember, err = m.groupService.ExistMember(groupNo, loginUID)
		}
		if err != nil {
			m.Error("查询群成员关系错误", zap.Error(err))
			httperr.ResponseErrorL(c, errcode.ErrMessageQueryFailed, nil, nil)
			return
		}
		if !isMember {
			httperr.ResponseErrorL(c, errcode.ErrMessageNotGroupMember, nil, nil)
			return
		}
	} else if req.ChannelType == common.ChannelTypePerson.Uint8() {
		if req.ChannelID != loginUID {
			isFriend, err := m.userService.IsFriend(loginUID, req.ChannelID)
			if err != nil {
				m.Error("查询好友关系错误", zap.Error(err))
				httperr.ResponseErrorL(c, errcode.ErrMessageQueryFailed, nil, nil)
				return
			}
			if !isFriend {
				httperr.ResponseErrorL(c, errcode.ErrMessageNotFriend, nil, nil)
				return
			}
		}
	} else {
		respondMessageRequestInvalid(c, "channel_type")
		return
	}

	var payload map[string]interface{}
	if req.Keyword != "" {
		payload = map[string]interface{}{
			"name": req.Keyword,
		}
	}

	// channel offset 只需查一次
	channelOffsets, err := m.channelOffsetDB.queryWithUIDAndChannelIDs(loginUID, []string{req.ChannelID})
	if err != nil {
		m.Error("查询清空消息标记错误", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrMessageQueryFailed, nil, nil)
		return
	}
	var offsetSeq uint32
	if len(channelOffsets) > 0 {
		offsetSeq = channelOffsets[0].MessageSeq
	}

	const maxFetchRounds = 5
	batchSize := req.Limit
	if needsExtFilter(cat) {
		batchSize = req.Limit * 3
	}

	files := make([]*channelFileResp, 0, req.Limit)
	fromUIDSet := make(map[string]bool)
	hasMore := false
	imExhausted := true
	var totalFromIM int64
	currentPage := req.Page
	if needsExtFilter(cat) {
		currentPage = 1
	}
	skip := 0
	if needsExtFilter(cat) {
		skip = (req.Page - 1) * req.Limit
	}

	for round := 0; round < maxFetchRounds; round++ {
		msgResp, err := m.ctx.IMSearchUserMessages(&config.SearchUserMessageReq{
			UID:          loginUID,
			Payload:      payload,
			PayloadTypes: payloadTypes,
			ChannelID:    req.ChannelID,
			ChannelType:  req.ChannelType,
			Limit:        batchSize,
			Page:         currentPage,
			Highlights:   []string{"payload.name"},
		})
		if err != nil {
			m.Error("查询文件消息失败", zap.Error(err))
			httperr.ResponseErrorL(c, errcode.ErrMessageQueryFailed, nil, nil)
			return
		}
		if msgResp == nil || len(msgResp.Messages) == 0 {
			break
		}
		totalFromIM = msgResp.Total

		filtered, err := m.filterMessages(msgResp.Messages, loginUID, offsetSeq)
		if err != nil {
			m.Error("过滤消息状态失败", zap.Error(err))
			httperr.ResponseErrorL(c, errcode.ErrMessageQueryFailed, nil, nil)
			return
		}

		for _, msg := range filtered {
			contentType := msg.GetContentType()
			file := buildChannelFileResp(msg, contentType)
			if file == nil {
				continue
			}
			if needsExtFilter(cat) {
				if categoryFromFilename(file.Name) != cat {
					continue
				}
			}
			file.Category = string(categoryForContentType(contentType, file.Name))

			if skip > 0 {
				skip--
				continue
			}
			if len(files) >= req.Limit {
				// 已凑够本页且仍有匹配项，标记还有更多
				hasMore = true
				break
			}
			fromUIDSet[msg.FromUID] = true
			files = append(files, file)
		}

		imExhausted = len(msgResp.Messages) < batchSize
		if hasMore || imExhausted {
			break
		}
		if !needsExtFilter(cat) {
			break
		}
		currentPage++
	}

	if !hasMore {
		if !needsExtFilter(cat) {
			hasMore = int64(req.Page*req.Limit) < totalFromIM
		} else {
			// ext 过滤下仅当 IM 数据耗尽才能确定无更多
			hasMore = !imExhausted
		}
	}

	total := totalFromIM
	if needsExtFilter(cat) {
		total = -1
	}

	// 批量查询用户名
	fromUIDs := make([]string, 0, len(fromUIDSet))
	for uid := range fromUIDSet {
		fromUIDs = append(fromUIDs, uid)
	}
	if len(fromUIDs) > 0 {
		users, err := m.userService.GetUsers(fromUIDs)
		if err != nil {
			m.Warn("查询用户信息失败", zap.Error(err))
		} else {
			nameMap := make(map[string]string, len(users))
			for _, u := range users {
				nameMap[u.UID] = u.Name
			}
			for _, f := range files {
				f.FromName = nameMap[f.FromUID]
			}
		}
	}

	c.Response(channelFilesResp{
		Total:   total,
		Page:    req.Page,
		Limit:   req.Limit,
		HasMore: hasMore,
		Files:   files,
	})
}

func (m *Message) filterMessages(messages []*config.MessageResp, loginUID string, offsetSeq uint32) ([]*config.MessageResp, error) {
	messageIDs := make([]string, 0, len(messages))
	for _, msg := range messages {
		messageIDs = append(messageIDs, msg.MessageIDStr)
	}

	revokedExtras, err := m.messageExtraDB.queryRevokedWithMessageIDs(messageIDs)
	if err != nil {
		return nil, err
	}
	deletedExtras, err := m.messageExtraDB.queryDeletedWithMessageIDs(messageIDs)
	if err != nil {
		return nil, err
	}
	userDeletedExtras, err := m.messageUserExtraDB.queryDeletedWithMessageIDsAndUID(messageIDs, loginUID)
	if err != nil {
		return nil, err
	}

	revokedMap := make(map[string]bool, len(revokedExtras))
	for _, e := range revokedExtras {
		revokedMap[e.MessageID] = true
	}
	deletedMap := make(map[string]bool, len(deletedExtras))
	for _, e := range deletedExtras {
		if e.IsDeleted == 1 {
			deletedMap[e.MessageID] = true
		}
	}
	userDeletedMap := make(map[string]bool, len(userDeletedExtras))
	for _, e := range userDeletedExtras {
		if e.MessageIsDeleted == 1 {
			userDeletedMap[e.MessageID] = true
		}
	}

	result := make([]*config.MessageResp, 0, len(messages))
	for _, msg := range messages {
		if revokedMap[msg.MessageIDStr] || deletedMap[msg.MessageIDStr] || userDeletedMap[msg.MessageIDStr] {
			continue
		}
		if offsetSeq > 0 && msg.MessageSeq <= offsetSeq {
			continue
		}
		result = append(result, msg)
	}
	return result, nil
}

func buildChannelFileResp(msg *config.MessageResp, contentType int) *channelFileResp {
	payload, err := msg.GetPayloadMap()
	if err != nil || payload == nil {
		return nil
	}

	resp := &channelFileResp{
		MessageID:   msg.MessageID,
		MessageSeq:  msg.MessageSeq,
		FromUID:     msg.FromUID,
		ChannelID:   msg.ChannelID,
		ChannelType: msg.ChannelType,
		Timestamp:   msg.Timestamp,
	}

	switch contentType {
	case common.File.Int():
		resp.Name = payloadStr(payload, "name")
		resp.URL = payloadStr(payload, "url")
		resp.Size = payloadInt64(payload, "size")
	case common.Image.Int(), common.GIF.Int():
		resp.URL = payloadStr(payload, "url")
		resp.Name = filenameFromURL(resp.URL)
		resp.Width = payloadInt(payload, "width")
		resp.Height = payloadInt(payload, "height")
	case common.Video.Int():
		resp.URL = payloadStr(payload, "url")
		resp.Name = filenameFromURL(resp.URL)
		resp.Width = payloadInt(payload, "width")
		resp.Height = payloadInt(payload, "height")
		resp.Duration = payloadInt(payload, "duration")
	default:
		return nil
	}

	if resp.URL == "" {
		return nil
	}
	return resp
}

func categoryForContentType(contentType int, name string) fileCategory {
	switch contentType {
	case common.Image.Int(), common.GIF.Int():
		return fileCategoryImage
	case common.Video.Int():
		return fileCategoryVideo
	case common.File.Int():
		return categoryFromFilename(name)
	default:
		return fileCategoryDocument
	}
}

func payloadStr(payload map[string]interface{}, key string) string {
	v, ok := payload[key]
	if !ok || v == nil {
		return ""
	}
	s, ok := v.(string)
	if ok {
		return s
	}
	return ""
}

func payloadInt64(payload map[string]interface{}, key string) int64 {
	v, ok := payload[key]
	if !ok || v == nil {
		return 0
	}
	switch n := v.(type) {
	case json.Number:
		i, _ := n.Int64()
		return i
	case float64:
		return int64(n)
	case int64:
		return n
	case int:
		return int64(n)
	}
	return 0
}

func payloadInt(payload map[string]interface{}, key string) int {
	return int(payloadInt64(payload, key))
}

func filenameFromURL(rawURL string) string {
	if rawURL == "" {
		return ""
	}
	idx := strings.LastIndex(rawURL, "/")
	if idx < 0 || idx >= len(rawURL)-1 {
		return ""
	}
	name := rawURL[idx+1:]
	if qIdx := strings.Index(name, "?"); qIdx > 0 {
		name = name[:qIdx]
	}
	if decoded, err := url.PathUnescape(name); err == nil {
		name = decoded
	}
	return name
}
