package search

import (
	"fmt"
	"html"
	"strings"

	"github.com/Mininglamp-OSS/octo-server/modules/document"
	dbbase "github.com/Mininglamp-OSS/octo-server/pkg/db"
	"go.uber.org/zap"
)

type documentSearchResp struct {
	ID                string `json:"id"`
	Name              string `json:"name"`
	Kind              string `json:"kind"`
	Extension         string `json:"extension"`
	Size              int64  `json:"size"`
	SourceType        string `json:"source_type"`
	SourceName        string `json:"source_name"`
	SourceChannelID   string `json:"source_channel_id"`
	SourceChannelType uint8  `json:"source_channel_type"`
	SourceMessageID   string `json:"source_message_id"`
	SourceMessageSeq  uint32 `json:"source_message_seq"`
	SpaceName         string `json:"space_name"`
	Uploader          string `json:"uploader"`
	Status            string `json:"status"`
	CreatedAt         string `json:"created_at"`
}

type documentSearchRow struct {
	AssetID           string      `db:"asset_id"`
	Name              string      `db:"name"`
	Kind              string      `db:"kind"`
	Extension         string      `db:"extension"`
	Size              int64       `db:"size"`
	SourceType        string      `db:"source_type"`
	SourceName        string      `db:"source_name"`
	SourceChannelID   string      `db:"source_channel_id"`
	SourceChannelType uint8       `db:"source_channel_type"`
	SourceMessageID   string      `db:"source_message_id"`
	UploaderUID       string      `db:"uploader_uid"`
	UploaderName      string      `db:"uploader_name"`
	OwnerUID          string      `db:"owner_uid"`
	OwnerName         string      `db:"owner_name"`
	TenantSpaceID     string      `db:"tenant_space_id"`
	DocumentSpaceID   string      `db:"document_space_id"`
	Visibility        string      `db:"visibility"`
	Status            string      `db:"status"`
	CreatedAt         dbbase.Time `db:"created_at"`
	SpaceName         string      `db:"space_name"`
}

type sourceMessageSeqRow struct {
	MessageID   string `db:"message_id"`
	ClientMsgNo string `db:"client_msg_no"`
	MessageSeq  uint32 `db:"message_seq"`
}

func buildDocumentSearchResp(asset *document.DocumentAssetModel, spaceName string, sourceMessageSeq uint32, keyword string) *documentSearchResp {
	return &documentSearchResp{
		ID:                asset.AssetID,
		Name:              highlightSearchText(asset.Name, keyword),
		Kind:              asset.Kind,
		Extension:         asset.Extension,
		Size:              asset.Size,
		SourceType:        asset.SourceType,
		SourceName:        asset.SourceName,
		SourceChannelID:   asset.SourceChannelID,
		SourceChannelType: asset.SourceChannelType,
		SourceMessageID:   asset.SourceMessageID,
		SourceMessageSeq:  sourceMessageSeq,
		SpaceName:         spaceName,
		Uploader:          asset.UploaderName,
		Status:            asset.Status,
		CreatedAt:         asset.CreatedAt.String(),
	}
}

func highlightSearchText(text, keyword string) string {
	escaped := html.EscapeString(text)
	trimmed := strings.TrimSpace(keyword)
	if trimmed == "" {
		return escaped
	}
	return strings.ReplaceAll(escaped, html.EscapeString(trimmed), fmt.Sprintf("<mark>%s</mark>", html.EscapeString(trimmed)))
}

func (s *Search) searchDocuments(uid, tenantSpaceID, keyword string, limit int) ([]*documentSearchResp, error) {
	keyword = strings.TrimSpace(keyword)
	if tenantSpaceID == "" || keyword == "" {
		return []*documentSearchResp{}, nil
	}
	if limit <= 0 || limit > 100 {
		limit = 20
	}

	like := "%" + keyword + "%"
	rows := make([]*documentSearchRow, 0)
	_, err := s.ctx.DB().SelectBySql(
		documentSearchSQL(),
		tenantSpaceID,
		document.StatusDeleted,
		like, like, like, like,
		uid,
		uid,
		uid,
		uid,
		document.VisibilityConversation,
		tenantSpaceID,
		uid,
		uid,
		limit,
	).Load(&rows)
	if err != nil {
		return nil, err
	}

	messageIDs := make([]string, 0, len(rows))
	for _, row := range rows {
		if row.SourceMessageID != "" {
			messageIDs = append(messageIDs, row.SourceMessageID)
		}
	}
	messageSeqs := s.lookupSourceMessageSeqs(messageIDs)

	resp := make([]*documentSearchResp, 0, len(rows))
	for _, row := range rows {
		asset := &document.DocumentAssetModel{
			AssetID:           row.AssetID,
			Name:              row.Name,
			Kind:              row.Kind,
			Extension:         row.Extension,
			Size:              row.Size,
			SourceType:        row.SourceType,
			SourceName:        row.SourceName,
			SourceChannelID:   row.SourceChannelID,
			SourceChannelType: row.SourceChannelType,
			SourceMessageID:   row.SourceMessageID,
			UploaderUID:       row.UploaderUID,
			UploaderName:      row.UploaderName,
			OwnerUID:          row.OwnerUID,
			OwnerName:         row.OwnerName,
			TenantSpaceID:     row.TenantSpaceID,
			DocumentSpaceID:   row.DocumentSpaceID,
			Visibility:        row.Visibility,
			Status:            row.Status,
		}
		asset.CreatedAt = row.CreatedAt
		resp = append(resp, buildDocumentSearchResp(asset, row.SpaceName, messageSeqs[row.SourceMessageID], keyword))
	}
	return resp, nil
}

func documentSearchSQL() string {
	return `SELECT
			da.asset_id, da.name, da.kind, da.extension, da.size,
			da.source_type, da.source_name, da.source_channel_id, da.source_channel_type, da.source_message_id,
			da.uploader_uid, da.uploader_name, da.owner_uid, da.owner_name,
			da.tenant_space_id, da.document_space_id, da.visibility, da.status, da.created_at,
			COALESCE(ds.name, '') AS space_name
		 FROM document_asset da
		 LEFT JOIN document_space ds ON ds.space_id=da.document_space_id AND ds.tenant_space_id=da.tenant_space_id
		 WHERE da.tenant_space_id=?
		   AND da.status<>?
		   AND (da.name LIKE ? OR da.source_name LIKE ? OR da.uploader_name LIKE ? OR COALESCE(ds.name, '') LIKE ?)
		   AND (
		     da.uploader_uid=?
		     OR da.owner_uid=?
		     OR (
		       da.document_space_id<>''
		       AND ds.status=1
		       AND (
		         ds.owner_uid=?
		         OR EXISTS (
		           SELECT 1
		           FROM document_space_member dsm
		           WHERE dsm.tenant_space_id=da.tenant_space_id
		             AND dsm.document_space_id=da.document_space_id
		             AND dsm.uid=?
		             AND dsm.status=1
		         )
		       )
		     )
		     OR (
		       da.visibility=?
		       AND (
		         (
		           da.source_channel_type=2
		           AND EXISTS (
		             SELECT 1 FROM group_member gm
		             INNER JOIN ` + "`group`" + ` g ON g.group_no=gm.group_no AND g.status=1 AND g.space_id=?
		             WHERE gm.group_no=da.source_channel_id AND gm.uid=? AND gm.status=1 AND gm.is_deleted=0
		           )
		         )
		         OR (da.source_channel_type<>2 AND da.source_channel_id=?)
		       )
		     )
		   )
		 ORDER BY da.last_access_at DESC, da.created_at DESC
		 LIMIT ?`
}

func (s *Search) lookupSourceMessageSeqs(messageIDs []string) map[string]uint32 {
	result := make(map[string]uint32)
	if len(messageIDs) == 0 {
		return result
	}
	tables := []string{"message", "message1", "message2", "message3", "message4"}
	for _, table := range tables {
		rows := make([]*sourceMessageSeqRow, 0)
		_, err := s.ctx.DB().SelectBySql(
			fmt.Sprintf("SELECT message_id, client_msg_no, message_seq FROM `%s` WHERE message_id IN ? OR client_msg_no IN ?", table),
			messageIDs,
			messageIDs,
		).Load(&rows)
		if err != nil {
			s.Warn("查询文档来源消息序号失败，跳过分片", zap.String("table", table), zap.Error(err))
			continue
		}
		for _, row := range rows {
			if row.MessageID != "" {
				result[row.MessageID] = row.MessageSeq
			}
			if row.ClientMsgNo != "" {
				result[row.ClientMsgNo] = row.MessageSeq
			}
		}
	}
	return result
}
