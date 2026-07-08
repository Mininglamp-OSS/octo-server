package document

import (
	"path/filepath"
	"strings"
	"time"

	dbbase "github.com/Mininglamp-OSS/octo-server/pkg/db"
)

const (
	StatusConversation = "conversation"
	StatusArchived     = "archived"
	StatusDeleted      = "deleted"

	VisibilityConversation = "conversation"
	VisibilitySpace        = "space"

	KindPDF   = "pdf"
	KindDoc   = "doc"
	KindSheet = "sheet"
	KindImage = "image"
	KindZip   = "zip"

	SourceTypePerson = "单聊"
	SourceTypeGroup  = "群聊"
	SourceTypeApp    = "上传"

	SourceNameDirectUpload = "直接上传"

	SpaceRoleOwner  = "owner"
	SpaceRoleAdmin  = "admin"
	SpaceRoleEditor = "editor"
	SpaceRoleViewer = "viewer"
)

type DocumentSpaceModel struct {
	SpaceID       string
	Name          string
	Description   string
	OwnerUID      string
	TenantSpaceID string
	Status        int
	dbbase.BaseModel
}

type DocumentSpaceBindingModel struct {
	BindingID         string
	DocumentSpaceID   string
	SourceChannelID   string
	SourceChannelType uint8
	SourceName        string
	CreatedBy         string
	TenantSpaceID     string
	Status            int
	dbbase.BaseModel
}

type DocumentSpaceMemberModel struct {
	MemberID        string
	DocumentSpaceID string
	UID             string
	Name            string
	Role            string
	Source          string
	CreatedBy       string
	TenantSpaceID   string
	Status          int
	dbbase.BaseModel
}

type DocumentUserCandidateModel struct {
	UID      string
	Name     string
	Username string
	Email    string
	Phone    string
}

type DocumentGroupCandidateModel struct {
	GroupNo string
	Name    string
	SpaceID string
}

type DocumentAssetModel struct {
	AssetID           string
	Name              string
	Kind              string
	Extension         string
	Size              int64
	StoragePath       string
	SourceType        string
	SourceChannelID   string
	SourceChannelType uint8
	SourceMessageID   string
	SourceMessageSeq  uint32 `db:"-"`
	SourceName        string
	UploaderUID       string
	UploaderName      string
	OwnerUID          string
	OwnerName         string
	TenantSpaceID     string
	DocumentSpaceID   string
	OriginalSpaceID   string
	Visibility        string
	Status            string
	Downloads         int
	Previewable       int
	LastAccessAt      *dbbase.Time
	dbbase.BaseModel
}

type DocumentEventModel struct {
	EventID       string
	AssetID       string
	ActorUID      string
	Action        string
	Detail        string
	TenantSpaceID string
	dbbase.BaseModel
}

type DocumentAssetResp struct {
	ID                string                 `json:"id"`
	Name              string                 `json:"name"`
	Kind              string                 `json:"kind"`
	Extension         string                 `json:"extension"`
	Size              int64                  `json:"size"`
	StoragePath       string                 `json:"storagePath"`
	Owner             string                 `json:"owner"`
	Uploader          string                 `json:"uploader"`
	SourceName        string                 `json:"sourceName"`
	SourceChannelID   string                 `json:"sourceChannelId"`
	SourceChannelType uint8                  `json:"sourceChannelType"`
	SourceType        string                 `json:"sourceType"`
	SpaceName         string                 `json:"spaceName"`
	Visibility        string                 `json:"visibility"`
	Status            string                 `json:"status"`
	CreatedAt         string                 `json:"createdAt"`
	LastAccessAt      string                 `json:"lastAccessAt"`
	Downloads         int                    `json:"downloads"`
	Previewable       bool                   `json:"previewable"`
	Flow              []string               `json:"flow"`
	SourceRef         *DocumentSourceRefResp `json:"sourceRef,omitempty"`
	Permissions       DocumentPermissionResp `json:"permissions"`
}

type DocumentSourceRefResp struct {
	ChannelID   string `json:"channelId"`
	ChannelType uint8  `json:"channelType"`
	ChannelName string `json:"channelName"`
	MessageID   string `json:"messageId"`
	MessageSeq  uint32 `json:"messageSeq"`
	SenderUID   string `json:"senderUid"`
	SenderName  string `json:"senderName"`
	SentAt      string `json:"sentAt"`
}

type DocumentPermissionResp struct {
	CanPreview  bool     `json:"canPreview"`
	CanDownload bool     `json:"canDownload"`
	CanArchive  bool     `json:"canArchive"`
	CanEdit     bool     `json:"canEdit"`
	CanDelete   bool     `json:"canDelete"`
	CanRestore  bool     `json:"canRestore"`
	CanManage   bool     `json:"canManage"`
	Summary     string   `json:"summary"`
	Reasons     []string `json:"reasons"`
}

type DocumentSpaceMemberResp struct {
	UID      string `json:"uid"`
	Name     string `json:"name"`
	Role     string `json:"role"`
	Source   string `json:"source"`
	JoinedAt string `json:"joinedAt"`
}

type DocumentMemberCandidateResp struct {
	UID           string `json:"uid"`
	Name          string `json:"name"`
	Username      string `json:"username,omitempty"`
	Email         string `json:"email,omitempty"`
	Phone         string `json:"phone,omitempty"`
	AlreadyMember bool   `json:"alreadyMember"`
}

type DocumentGroupBindingCandidateResp struct {
	ChannelID                  string `json:"channelId"`
	ChannelType                uint8  `json:"channelType"`
	Name                       string `json:"name"`
	BoundSpaceID               string `json:"boundSpaceId,omitempty"`
	BoundSpaceName             string `json:"boundSpaceName,omitempty"`
	AlreadyBoundToCurrentSpace bool   `json:"alreadyBoundToCurrentSpace"`
}

type DocumentChannelStorageSpaceResp struct {
	SpaceID   string `json:"spaceId"`
	SpaceName string `json:"spaceName"`
}

type DocumentSpaceBindingResp struct {
	ID          string `json:"id"`
	ChannelID   string `json:"channelId"`
	ChannelType uint8  `json:"channelType"`
	Name        string `json:"name"`
	CreatedBy   string `json:"createdBy"`
}

type DocumentSpaceResp struct {
	ID                 string                      `json:"id"`
	Name               string                      `json:"name"`
	Owner              string                      `json:"owner"`
	FileCount          int                         `json:"fileCount"`
	MemberCount        int                         `json:"memberCount"`
	Members            []*DocumentSpaceMemberResp  `json:"members"`
	BoundConversations []*DocumentSpaceBindingResp `json:"boundConversations"`
	PinnedFileIDs      []string                    `json:"pinnedFileIds"`
	Description        string                      `json:"description"`
}

type DocumentAuditResp struct {
	ID     string `json:"id"`
	Time   string `json:"time"`
	Actor  string `json:"actor"`
	Action string `json:"action"`
	Target string `json:"target"`
	Detail string `json:"detail"`
}

type DocumentStateResp struct {
	Files  []*DocumentAssetResp `json:"files"`
	Spaces []*DocumentSpaceResp `json:"spaces"`
	Audits []*DocumentAuditResp `json:"audits"`
}

type UploadReq struct {
	Name            string `json:"name"`
	Extension       string `json:"extension"`
	Size            int64  `json:"size"`
	StoragePath     string `json:"storage_path"`
	DocumentSpaceID string `json:"document_space_id"`
	UploaderName    string `json:"uploader_name"`
}

type ArchiveReq struct {
	AssetID           string `json:"asset_id"`
	DocumentSpaceID   string `json:"document_space_id"`
	Name              string `json:"name"`
	Extension         string `json:"extension"`
	Size              int64  `json:"size"`
	StoragePath       string `json:"storage_path"`
	SourceType        string `json:"source_type"`
	SourceChannelID   string `json:"source_channel_id"`
	SourceChannelType uint8  `json:"source_channel_type"`
	SourceMessageID   string `json:"source_message_id"`
	SourceName        string `json:"source_name"`
	UploaderUID       string `json:"uploader_uid"`
	UploaderName      string `json:"uploader_name"`
}

type BindConversationReq struct {
	DocumentSpaceID   string `json:"document_space_id"`
	SourceChannelID   string `json:"source_channel_id"`
	SourceChannelType uint8  `json:"source_channel_type"`
	SourceName        string `json:"source_name"`
}

type SaveSpaceReq struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

type SaveSpaceMemberReq struct {
	UID  string `json:"uid"`
	Name string `json:"name"`
	Role string `json:"role"`
}

type RenameAssetReq struct {
	Name string `json:"name"`
}

type MoveAssetReq struct {
	DocumentSpaceID string `json:"document_space_id"`
}

func documentKind(extension string) string {
	ext := strings.ToLower(strings.TrimPrefix(extension, "."))
	switch ext {
	case "pdf":
		return KindPDF
	case "xls", "xlsx", "csv":
		return KindSheet
	case "png", "jpg", "jpeg", "gif", "webp":
		return KindImage
	case "zip", "rar", "7z":
		return KindZip
	default:
		return KindDoc
	}
}

func normalizeExtension(name, extension string) string {
	ext := strings.TrimSpace(extension)
	if ext == "" {
		ext = filepath.Ext(name)
	}
	if ext == "" {
		return ""
	}
	if !strings.HasPrefix(ext, ".") {
		ext = "." + ext
	}
	return strings.ToLower(ext)
}

func previewableForExtension(extension string) int {
	switch strings.ToLower(strings.TrimPrefix(extension, ".")) {
	case "zip", "rar", "7z":
		return 0
	default:
		return 1
	}
}

func formatDBTime(t dbbase.Time) string {
	return t.String()
}

func nowDBTime() dbbase.Time {
	return dbbase.Time(time.Now())
}
