package document

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
)

type documentRepository interface {
	ListSpaces(uid, tenantSpaceID string) ([]*DocumentSpaceModel, error)
	ListAllSpaces(uid, tenantSpaceID string) ([]*DocumentSpaceModel, error)
	EnsureDefaultSpace(uid, tenantSpaceID string) (*DocumentSpaceModel, error)
	GetSpace(spaceID, uid, tenantSpaceID string) (*DocumentSpaceModel, error)
	SaveSpace(space *DocumentSpaceModel) error
	UpdateSpace(space *DocumentSpaceModel) error
	ListSpaceBindings(uid, tenantSpaceID string) ([]*DocumentSpaceBindingModel, error)
	SaveSpaceBinding(binding *DocumentSpaceBindingModel) error
	RemoveSpaceBinding(spaceID, bindingID, tenantSpaceID string) error
	ListSpaceMembers(uid, tenantSpaceID string) ([]*DocumentSpaceMemberModel, error)
	SearchUsers(uid, tenantSpaceID, keyword string, limit int) ([]*DocumentUserCandidateModel, error)
	SearchGroups(uid, tenantSpaceID, keyword string, limit int) ([]*DocumentGroupCandidateModel, error)
	SaveSpaceMember(member *DocumentSpaceMemberModel) error
	RemoveSpaceMember(spaceID, memberUID, tenantSpaceID string) error
	ListAssets(uid, tenantSpaceID string) ([]*DocumentAssetModel, error)
	GetAsset(assetID, uid, tenantSpaceID string) (*DocumentAssetModel, error)
	SaveAsset(asset *DocumentAssetModel) error
	UpdateAsset(asset *DocumentAssetModel) error
	DeleteAsset(assetID, tenantSpaceID string) error
	AddEvent(event *DocumentEventModel) error
	ListEvents(uid, tenantSpaceID string, limit int) ([]*DocumentEventModel, error)
	CanAccessSource(uid, tenantSpaceID, sourceChannelID string, sourceChannelType uint8) (bool, error)
	CanAccessSources(uid, tenantSpaceID string, sources []documentSourceRef) (map[string]bool, error)
}

type documentSourceRef struct {
	ChannelID   string
	ChannelType uint8
}

type DocumentService struct {
	repo documentRepository
}

func NewDocumentService(repo documentRepository) *DocumentService {
	return &DocumentService{repo: repo}
}

func (s *DocumentService) State(uid, tenantSpaceID string) (*DocumentStateResp, error) {
	if strings.TrimSpace(uid) == "" {
		return nil, errors.New("uid is required")
	}
	space, err := s.repo.EnsureDefaultSpace(uid, tenantSpaceID)
	if err != nil {
		return nil, err
	}
	if err := s.saveOwnerMember(uid, tenantSpaceID, space); err != nil {
		return nil, err
	}
	return s.buildState(uid, tenantSpaceID)
}

func (s *DocumentService) CreateSpace(uid, tenantSpaceID string, req SaveSpaceReq) (*DocumentStateResp, error) {
	name := strings.TrimSpace(req.Name)
	if name == "" {
		return nil, errors.New("空间名称不能为空")
	}
	if err := s.ensureSpaceNameAvailable("", tenantSpaceID, name); err != nil {
		return nil, err
	}
	now := nowDBTime()
	space := &DocumentSpaceModel{
		SpaceID:       "DOCSPACE-" + util.GenerUUID(),
		Name:          name,
		Description:   strings.TrimSpace(req.Description),
		OwnerUID:      uid,
		TenantSpaceID: tenantSpaceID,
		Status:        1,
	}
	space.CreatedAt = now
	space.UpdatedAt = now
	if err := s.repo.SaveSpace(space); err != nil {
		return nil, err
	}
	if err := s.saveOwnerMember(uid, tenantSpaceID, space); err != nil {
		return nil, err
	}
	if err := s.addEvent(uid, tenantSpaceID, space.SpaceID, "新建空间", space.Name); err != nil {
		return nil, err
	}
	return s.buildState(uid, tenantSpaceID)
}

func (s *DocumentService) UpdateSpace(uid, tenantSpaceID, spaceID string, req SaveSpaceReq) (*DocumentStateResp, error) {
	space, err := s.resolveSpace(uid, tenantSpaceID, spaceID)
	if err != nil {
		return nil, err
	}
	if err := s.requireSpaceAdmin(uid, tenantSpaceID, space.SpaceID); err != nil {
		return nil, err
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		return nil, errors.New("空间名称不能为空")
	}
	if err := s.ensureSpaceNameAvailable(space.SpaceID, tenantSpaceID, name); err != nil {
		return nil, err
	}
	space.Name = name
	space.Description = strings.TrimSpace(req.Description)
	space.UpdatedAt = nowDBTime()
	if err := s.repo.UpdateSpace(space); err != nil {
		return nil, err
	}
	if err := s.addEvent(uid, tenantSpaceID, space.SpaceID, "编辑空间", space.Name); err != nil {
		return nil, err
	}
	return s.buildState(uid, tenantSpaceID)
}

func (s *DocumentService) DisableSpace(uid, tenantSpaceID, spaceID string) (*DocumentStateResp, error) {
	space, err := s.resolveSpace(uid, tenantSpaceID, spaceID)
	if err != nil {
		return nil, err
	}
	if err := s.requireSpaceOwner(uid, space); err != nil {
		return nil, err
	}
	space.Status = 0
	space.UpdatedAt = nowDBTime()
	if err := s.repo.UpdateSpace(space); err != nil {
		return nil, err
	}
	if err := s.addEvent(uid, tenantSpaceID, space.SpaceID, "停用空间", space.Name); err != nil {
		return nil, err
	}
	return s.buildState(uid, tenantSpaceID)
}

func (s *DocumentService) UpsertSpaceMember(uid, tenantSpaceID, spaceID string, req SaveSpaceMemberReq) (*DocumentStateResp, error) {
	space, err := s.resolveSpace(uid, tenantSpaceID, spaceID)
	if err != nil {
		return nil, err
	}
	if err := s.requireSpaceAdmin(uid, tenantSpaceID, space.SpaceID); err != nil {
		return nil, err
	}
	memberUID := strings.TrimSpace(req.UID)
	if memberUID == "" {
		return nil, errors.New("成员不能为空")
	}
	role := normalizeSpaceRole(req.Role)
	if role == SpaceRoleOwner {
		return nil, errors.New("所有者转让暂不支持在成员编辑中完成")
	}
	now := nowDBTime()
	member := &DocumentSpaceMemberModel{
		MemberID:        "MEM-" + util.GenerUUID(),
		DocumentSpaceID: space.SpaceID,
		UID:             memberUID,
		Name:            fallbackString(req.Name, memberUID),
		Role:            role,
		Source:          "手动添加",
		CreatedBy:       uid,
		TenantSpaceID:   tenantSpaceID,
		Status:          1,
	}
	member.CreatedAt = now
	member.UpdatedAt = now
	if err := s.repo.SaveSpaceMember(member); err != nil {
		return nil, err
	}
	if err := s.addEvent(uid, tenantSpaceID, space.SpaceID, "设置成员", fmt.Sprintf("%s 为 %s", member.Name, role)); err != nil {
		return nil, err
	}
	return s.buildState(uid, tenantSpaceID)
}

func (s *DocumentService) SearchSpaceMemberCandidates(uid, tenantSpaceID, spaceID, keyword string) ([]*DocumentMemberCandidateResp, error) {
	space, err := s.resolveSpace(uid, tenantSpaceID, spaceID)
	if err != nil {
		return nil, err
	}
	if err := s.requireSpaceAdmin(uid, tenantSpaceID, space.SpaceID); err != nil {
		return nil, err
	}
	keyword = strings.TrimSpace(keyword)
	if keyword == "" {
		return []*DocumentMemberCandidateResp{}, nil
	}
	users, err := s.repo.SearchUsers(uid, tenantSpaceID, keyword, 20)
	if err != nil {
		return nil, err
	}
	members, err := s.repo.ListSpaceMembers(uid, tenantSpaceID)
	if err != nil {
		return nil, err
	}
	memberUIDs := map[string]bool{space.OwnerUID: true}
	for _, member := range members {
		if member.DocumentSpaceID == space.SpaceID && member.Status == 1 {
			memberUIDs[member.UID] = true
		}
	}
	resp := make([]*DocumentMemberCandidateResp, 0, len(users))
	for _, user := range users {
		if strings.TrimSpace(user.UID) == "" {
			continue
		}
		resp = append(resp, &DocumentMemberCandidateResp{
			UID:           user.UID,
			Name:          fallbackString(user.Name, user.UID),
			Username:      user.Username,
			AlreadyMember: memberUIDs[user.UID],
		})
	}
	return resp, nil
}

func (s *DocumentService) RemoveSpaceMember(uid, tenantSpaceID, spaceID, memberUID string) (*DocumentStateResp, error) {
	space, err := s.resolveSpace(uid, tenantSpaceID, spaceID)
	if err != nil {
		return nil, err
	}
	if err := s.requireSpaceAdmin(uid, tenantSpaceID, space.SpaceID); err != nil {
		return nil, err
	}
	if strings.TrimSpace(memberUID) == "" {
		return nil, errors.New("成员不能为空")
	}
	if memberUID == space.OwnerUID {
		return nil, errors.New("不能移除空间所有者")
	}
	if err := s.repo.RemoveSpaceMember(space.SpaceID, memberUID, tenantSpaceID); err != nil {
		return nil, err
	}
	if err := s.addEvent(uid, tenantSpaceID, space.SpaceID, "移除成员", memberUID); err != nil {
		return nil, err
	}
	return s.buildState(uid, tenantSpaceID)
}

func (s *DocumentService) Upload(uid, tenantSpaceID string, req UploadReq) (*DocumentStateResp, error) {
	if strings.TrimSpace(req.Name) == "" {
		return nil, errors.New("文件名不能为空")
	}
	space, err := s.resolveSpace(uid, tenantSpaceID, req.DocumentSpaceID)
	if err != nil {
		return nil, err
	}
	if err := s.requireSpaceEditor(uid, tenantSpaceID, space.SpaceID); err != nil {
		return nil, err
	}
	extension := normalizeExtension(req.Name, req.Extension)
	now := nowDBTime()
	displayName := fallbackString(req.UploaderName, uid)
	asset := &DocumentAssetModel{
		AssetID:         "DOC-" + util.GenerUUID(),
		Name:            req.Name,
		Kind:            documentKind(extension),
		Extension:       extension,
		Size:            req.Size,
		StoragePath:     req.StoragePath,
		SourceType:      SourceTypeApp,
		SourceName:      SourceNameDirectUpload,
		UploaderUID:     uid,
		UploaderName:    displayName,
		OwnerUID:        uid,
		OwnerName:       displayName,
		TenantSpaceID:   tenantSpaceID,
		DocumentSpaceID: space.SpaceID,
		OriginalSpaceID: space.SpaceID,
		Visibility:      VisibilitySpace,
		Status:          StatusArchived,
		Downloads:       0,
		Previewable:     previewableForExtension(extension),
		LastAccessAt:    &now,
	}
	asset.CreatedAt = now
	asset.UpdatedAt = now
	if err := s.repo.SaveAsset(asset); err != nil {
		return nil, err
	}
	if err := s.addEvent(uid, tenantSpaceID, asset.AssetID, "上传", fmt.Sprintf("上传到%s", space.Name)); err != nil {
		return nil, err
	}
	return s.buildState(uid, tenantSpaceID)
}

func (s *DocumentService) Archive(uid, tenantSpaceID string, req ArchiveReq) (*DocumentStateResp, error) {
	space, err := s.resolveArchiveSpace(uid, tenantSpaceID, req)
	if err != nil {
		return nil, err
	}
	if err := s.requireSpaceEditor(uid, tenantSpaceID, space.SpaceID); err != nil {
		return nil, err
	}
	asset, err := s.repo.GetAsset(req.AssetID, uid, tenantSpaceID)
	if err != nil {
		return nil, err
	}
	if asset != nil {
		if asset.Visibility == VisibilityConversation {
			if err := s.requireReadable(uid, tenantSpaceID, asset); err != nil {
				return nil, err
			}
		} else if err := s.requireEditable(uid, tenantSpaceID, asset); err != nil {
			return nil, err
		}
	}
	sourceChannelID := req.SourceChannelID
	sourceChannelType := req.SourceChannelType
	if asset != nil {
		sourceChannelID = asset.SourceChannelID
		sourceChannelType = asset.SourceChannelType
	}
	if err := s.requireSourceAccess(uid, tenantSpaceID, sourceChannelID, sourceChannelType); err != nil {
		return nil, err
	}
	now := nowDBTime()
	if asset == nil {
		extension := normalizeExtension(req.Name, req.Extension)
		asset = &DocumentAssetModel{
			AssetID:           "DOC-" + util.GenerUUID(),
			Name:              req.Name,
			Kind:              documentKind(extension),
			Extension:         extension,
			Size:              req.Size,
			StoragePath:       req.StoragePath,
			SourceType:        fallbackString(req.SourceType, SourceTypeGroup),
			SourceChannelID:   req.SourceChannelID,
			SourceChannelType: req.SourceChannelType,
			SourceMessageID:   req.SourceMessageID,
			SourceName:        req.SourceName,
			UploaderUID:       uid,
			UploaderName:      fallbackString(req.UploaderName, uid),
			OwnerUID:          uid,
			OwnerName:         uid,
			TenantSpaceID:     tenantSpaceID,
			DocumentSpaceID:   space.SpaceID,
			OriginalSpaceID:   space.SpaceID,
			Visibility:        VisibilitySpace,
			Status:            StatusArchived,
			Previewable:       previewableForExtension(extension),
			LastAccessAt:      &now,
		}
		asset.CreatedAt = now
		asset.UpdatedAt = now
		if err := s.repo.SaveAsset(asset); err != nil {
			return nil, err
		}
	} else {
		asset.DocumentSpaceID = space.SpaceID
		if asset.OriginalSpaceID == "" {
			asset.OriginalSpaceID = space.SpaceID
		}
		asset.Visibility = VisibilitySpace
		asset.Status = StatusArchived
		asset.UpdatedAt = now
		asset.LastAccessAt = &now
		if err := s.repo.UpdateAsset(asset); err != nil {
			return nil, err
		}
	}
	if err := s.addEvent(uid, tenantSpaceID, asset.AssetID, "归档", fmt.Sprintf("归档到%s", space.Name)); err != nil {
		return nil, err
	}
	return s.buildState(uid, tenantSpaceID)
}

func (s *DocumentService) RenameAsset(uid, tenantSpaceID, assetID string, req RenameAssetReq) (*DocumentStateResp, error) {
	asset, err := s.requireAsset(uid, tenantSpaceID, assetID)
	if err != nil {
		return nil, err
	}
	if err := s.requireEditable(uid, tenantSpaceID, asset); err != nil {
		return nil, err
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		return nil, errors.New("文件名不能为空")
	}
	if strings.ContainsAny(name, `\/:*?"<>|`) {
		return nil, errors.New("文件名包含非法字符")
	}
	oldName := asset.Name
	extension := normalizeExtension(name, "")
	asset.Name = name
	asset.Extension = extension
	asset.Kind = documentKind(extension)
	asset.UpdatedAt = nowDBTime()
	if err := s.repo.UpdateAsset(asset); err != nil {
		return nil, err
	}
	if err := s.addEvent(uid, tenantSpaceID, asset.AssetID, "重命名", fmt.Sprintf("%s -> %s", oldName, name)); err != nil {
		return nil, err
	}
	return s.buildState(uid, tenantSpaceID)
}

func (s *DocumentService) MoveAsset(uid, tenantSpaceID, assetID string, req MoveAssetReq) (*DocumentStateResp, error) {
	asset, err := s.requireAsset(uid, tenantSpaceID, assetID)
	if err != nil {
		return nil, err
	}
	if asset.Status != StatusArchived {
		return nil, errors.New("只有空间文件可以移动")
	}
	if err := s.requireEditable(uid, tenantSpaceID, asset); err != nil {
		return nil, err
	}
	targetSpace, err := s.resolveSpace(uid, tenantSpaceID, req.DocumentSpaceID)
	if err != nil {
		return nil, err
	}
	if err := s.requireSpaceEditor(uid, tenantSpaceID, targetSpace.SpaceID); err != nil {
		return nil, err
	}
	oldSpaceID := asset.DocumentSpaceID
	asset.DocumentSpaceID = targetSpace.SpaceID
	asset.OriginalSpaceID = targetSpace.SpaceID
	asset.Visibility = VisibilitySpace
	asset.UpdatedAt = nowDBTime()
	if err := s.repo.UpdateAsset(asset); err != nil {
		return nil, err
	}
	oldSpaceName := oldSpaceID
	if oldSpace, err := s.repo.GetSpace(oldSpaceID, uid, tenantSpaceID); err == nil && oldSpace != nil {
		oldSpaceName = oldSpace.Name
	}
	if err := s.addEvent(uid, tenantSpaceID, asset.AssetID, "移动空间", fmt.Sprintf("%s -> %s", oldSpaceName, targetSpace.Name)); err != nil {
		return nil, err
	}
	return s.buildState(uid, tenantSpaceID)
}

func (s *DocumentService) BindConversation(uid, tenantSpaceID string, req BindConversationReq) (*DocumentStateResp, error) {
	space, err := s.resolveSpace(uid, tenantSpaceID, req.DocumentSpaceID)
	if err != nil {
		return nil, err
	}
	if err := s.requireSpaceAdmin(uid, tenantSpaceID, space.SpaceID); err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.SourceChannelID) == "" {
		return nil, errors.New("来源会话不能为空")
	}
	if req.SourceChannelType == 0 {
		return nil, errors.New("来源会话类型不能为空")
	}
	if err := s.requireSourceAccess(uid, tenantSpaceID, req.SourceChannelID, req.SourceChannelType); err != nil {
		return nil, err
	}
	if existing, err := s.activeBindingForSource(uid, tenantSpaceID, req.SourceChannelID, req.SourceChannelType); err != nil {
		return nil, err
	} else if existing != nil && existing.DocumentSpaceID != space.SpaceID {
		role, err := s.spaceRole(uid, tenantSpaceID, existing.DocumentSpaceID)
		if err != nil {
			return nil, err
		}
		if role != SpaceRoleOwner && role != SpaceRoleAdmin {
			return nil, errors.New("无权修改已有绑定")
		}
	}
	sourceName := fallbackString(req.SourceName, req.SourceChannelID)
	now := nowDBTime()
	binding := &DocumentSpaceBindingModel{
		BindingID:         "BIND-" + util.GenerUUID(),
		DocumentSpaceID:   space.SpaceID,
		SourceChannelID:   req.SourceChannelID,
		SourceChannelType: req.SourceChannelType,
		SourceName:        sourceName,
		CreatedBy:         uid,
		TenantSpaceID:     tenantSpaceID,
		Status:            1,
	}
	binding.CreatedAt = now
	binding.UpdatedAt = now
	if err := s.repo.SaveSpaceBinding(binding); err != nil {
		return nil, err
	}
	if err := s.addEvent(uid, tenantSpaceID, space.SpaceID, "绑定群聊", fmt.Sprintf("%s 设为%s默认归档空间", sourceName, space.Name)); err != nil {
		return nil, err
	}
	return s.buildState(uid, tenantSpaceID)
}

func (s *DocumentService) SearchBindingConversations(uid, tenantSpaceID, spaceID, keyword string) ([]*DocumentGroupBindingCandidateResp, error) {
	space, err := s.resolveSpace(uid, tenantSpaceID, spaceID)
	if err != nil {
		return nil, err
	}
	if err := s.requireSpaceAdmin(uid, tenantSpaceID, space.SpaceID); err != nil {
		return nil, err
	}
	keyword = strings.TrimSpace(keyword)
	if keyword == "" {
		return []*DocumentGroupBindingCandidateResp{}, nil
	}
	groups, err := s.repo.SearchGroups(uid, tenantSpaceID, keyword, 20)
	if err != nil {
		return nil, err
	}
	activeBindings, err := s.repo.ListSpaceBindings(uid, tenantSpaceID)
	if err != nil {
		return nil, err
	}
	accessibleSpaces, err := s.repo.ListSpaces(uid, tenantSpaceID)
	if err != nil {
		return nil, err
	}
	spaceNameByID := make(map[string]string, len(accessibleSpaces))
	for _, item := range accessibleSpaces {
		spaceNameByID[item.SpaceID] = item.Name
	}
	bindingByChannel := make(map[string]*DocumentSpaceBindingModel, len(activeBindings))
	for _, binding := range activeBindings {
		if binding.Status != 1 {
			continue
		}
		bindingByChannel[documentSourceKey(binding.SourceChannelID, binding.SourceChannelType)] = binding
	}

	resp := make([]*DocumentGroupBindingCandidateResp, 0, len(groups))
	for _, group := range groups {
		if group == nil || strings.TrimSpace(group.GroupNo) == "" {
			continue
		}
		candidate := &DocumentGroupBindingCandidateResp{
			ChannelID:   group.GroupNo,
			ChannelType: 2,
			Name:        fallbackString(group.Name, group.GroupNo),
		}
		if binding := bindingByChannel[documentSourceKey(group.GroupNo, 2)]; binding != nil {
			if binding.DocumentSpaceID == space.SpaceID {
				candidate.BoundSpaceID = binding.DocumentSpaceID
				candidate.BoundSpaceName = space.Name
				candidate.AlreadyBoundToCurrentSpace = true
			} else if boundSpaceName := strings.TrimSpace(spaceNameByID[binding.DocumentSpaceID]); boundSpaceName != "" {
				candidate.BoundSpaceID = binding.DocumentSpaceID
				candidate.BoundSpaceName = boundSpaceName
			}
		}
		resp = append(resp, candidate)
	}
	return resp, nil
}

func (s *DocumentService) ChannelStorageSpace(uid, tenantSpaceID, sourceChannelID string, sourceChannelType uint8) (*DocumentChannelStorageSpaceResp, error) {
	if strings.TrimSpace(sourceChannelID) != "" && sourceChannelType != 0 {
		if err := s.requireSourceAccess(uid, tenantSpaceID, sourceChannelID, sourceChannelType); err != nil {
			return nil, err
		}
	}
	space, err := s.boundSpaceForSource(uid, tenantSpaceID, sourceChannelID, sourceChannelType)
	if err != nil {
		return nil, err
	}
	if space == nil {
		return &DocumentChannelStorageSpaceResp{}, nil
	}
	return &DocumentChannelStorageSpaceResp{
		SpaceID:   space.SpaceID,
		SpaceName: space.Name,
	}, nil
}

func (s *DocumentService) UnbindConversation(uid, tenantSpaceID, spaceID, bindingID string) (*DocumentStateResp, error) {
	space, err := s.resolveSpace(uid, tenantSpaceID, spaceID)
	if err != nil {
		return nil, err
	}
	if err := s.requireSpaceAdmin(uid, tenantSpaceID, space.SpaceID); err != nil {
		return nil, err
	}
	if strings.TrimSpace(bindingID) == "" {
		return nil, errors.New("绑定关系不能为空")
	}
	if err := s.repo.RemoveSpaceBinding(space.SpaceID, bindingID, tenantSpaceID); err != nil {
		return nil, err
	}
	if err := s.addEvent(uid, tenantSpaceID, space.SpaceID, "解绑群聊", bindingID); err != nil {
		return nil, err
	}
	return s.buildState(uid, tenantSpaceID)
}

func (s *DocumentService) Preview(uid, tenantSpaceID, assetID string) (*DocumentStateResp, error) {
	asset, err := s.requireAsset(uid, tenantSpaceID, assetID)
	if err != nil {
		return nil, err
	}
	if err := s.requireReadable(uid, tenantSpaceID, asset); err != nil {
		return nil, err
	}
	now := nowDBTime()
	asset.LastAccessAt = &now
	asset.UpdatedAt = now
	if err := s.repo.UpdateAsset(asset); err != nil {
		return nil, err
	}
	if err := s.addEvent(uid, tenantSpaceID, asset.AssetID, "预览", "在线预览"); err != nil {
		return nil, err
	}
	return s.buildState(uid, tenantSpaceID)
}

func (s *DocumentService) Download(uid, tenantSpaceID, assetID string) (*DocumentStateResp, error) {
	asset, err := s.requireAsset(uid, tenantSpaceID, assetID)
	if err != nil {
		return nil, err
	}
	if err := s.requireReadable(uid, tenantSpaceID, asset); err != nil {
		return nil, err
	}
	now := nowDBTime()
	asset.Downloads++
	asset.LastAccessAt = &now
	asset.UpdatedAt = now
	if err := s.repo.UpdateAsset(asset); err != nil {
		return nil, err
	}
	if err := s.addEvent(uid, tenantSpaceID, asset.AssetID, "下载", "下载文件"); err != nil {
		return nil, err
	}
	return s.buildState(uid, tenantSpaceID)
}

func (s *DocumentService) Trash(uid, tenantSpaceID, assetID string) (*DocumentStateResp, error) {
	asset, err := s.requireAsset(uid, tenantSpaceID, assetID)
	if err != nil {
		return nil, err
	}
	if err := s.requireManageable(uid, tenantSpaceID, asset); err != nil {
		return nil, err
	}
	now := nowDBTime()
	asset.Status = StatusDeleted
	asset.UpdatedAt = now
	if err := s.repo.UpdateAsset(asset); err != nil {
		return nil, err
	}
	if err := s.addEvent(uid, tenantSpaceID, asset.AssetID, "删除", "移动到回收站"); err != nil {
		return nil, err
	}
	return s.buildState(uid, tenantSpaceID)
}

func (s *DocumentService) Restore(uid, tenantSpaceID, assetID string) (*DocumentStateResp, error) {
	asset, err := s.requireAsset(uid, tenantSpaceID, assetID)
	if err != nil {
		return nil, err
	}
	if err := s.requireManageable(uid, tenantSpaceID, asset); err != nil {
		return nil, err
	}
	now := nowDBTime()
	if asset.DocumentSpaceID != "" || asset.OriginalSpaceID != "" {
		if asset.DocumentSpaceID == "" {
			asset.DocumentSpaceID = asset.OriginalSpaceID
		}
		asset.Status = StatusArchived
		asset.Visibility = VisibilitySpace
	} else {
		asset.Status = StatusConversation
		asset.Visibility = VisibilityConversation
	}
	asset.UpdatedAt = now
	if err := s.repo.UpdateAsset(asset); err != nil {
		return nil, err
	}
	if err := s.addEvent(uid, tenantSpaceID, asset.AssetID, "恢复", "从回收站恢复"); err != nil {
		return nil, err
	}
	return s.buildState(uid, tenantSpaceID)
}

func (s *DocumentService) PermanentDelete(uid, tenantSpaceID, assetID string) (*DocumentStateResp, error) {
	asset, err := s.requireAsset(uid, tenantSpaceID, assetID)
	if err != nil {
		return nil, err
	}
	if asset.Status != StatusDeleted {
		return nil, errors.New("只有回收站文件可以永久删除")
	}
	if err := s.requireManageable(uid, tenantSpaceID, asset); err != nil {
		return nil, err
	}
	if err := s.repo.DeleteAsset(asset.AssetID, tenantSpaceID); err != nil {
		return nil, err
	}
	if err := s.addEvent(uid, tenantSpaceID, asset.AssetID, "永久删除", asset.Name); err != nil {
		return nil, err
	}
	return s.buildState(uid, tenantSpaceID)
}

func (s *DocumentService) EmptyTrash(uid, tenantSpaceID string) (*DocumentStateResp, error) {
	assets, err := s.repo.ListAssets(uid, tenantSpaceID)
	if err != nil {
		return nil, err
	}
	deletedCount := 0
	for _, asset := range assets {
		if asset.Status != StatusDeleted {
			continue
		}
		if err := s.requireManageable(uid, tenantSpaceID, asset); err != nil {
			continue
		}
		if err := s.repo.DeleteAsset(asset.AssetID, tenantSpaceID); err != nil {
			return nil, err
		}
		deletedCount++
	}
	if err := s.addEvent(uid, tenantSpaceID, "trash", "清空回收站", fmt.Sprintf("永久删除%d个文件", deletedCount)); err != nil {
		return nil, err
	}
	return s.buildState(uid, tenantSpaceID)
}

func (s *DocumentService) CheckSource(uid, tenantSpaceID, assetID string) (bool, error) {
	asset, err := s.requireAsset(uid, tenantSpaceID, assetID)
	if err != nil {
		return false, err
	}
	if err := s.requireReadable(uid, tenantSpaceID, asset); err != nil {
		return false, err
	}
	if strings.TrimSpace(asset.SourceChannelID) == "" {
		return false, nil
	}
	return s.repo.CanAccessSource(uid, tenantSpaceID, asset.SourceChannelID, asset.SourceChannelType)
}

func (s *DocumentService) requireAsset(uid, tenantSpaceID, assetID string) (*DocumentAssetModel, error) {
	asset, err := s.repo.GetAsset(assetID, uid, tenantSpaceID)
	if err != nil {
		return nil, err
	}
	if asset == nil {
		return nil, errors.New("文件不存在")
	}
	return asset, nil
}

func (s *DocumentService) requireReadable(uid, tenantSpaceID string, asset *DocumentAssetModel) error {
	if asset.Status == StatusDeleted {
		return errors.New("文件已在回收站")
	}
	if asset.OwnerUID == uid || asset.UploaderUID == uid {
		return nil
	}
	if strings.TrimSpace(asset.DocumentSpaceID) != "" {
		space, err := s.repo.GetSpace(asset.DocumentSpaceID, uid, tenantSpaceID)
		if err != nil {
			return err
		}
		members, err := s.repo.ListSpaceMembers(uid, tenantSpaceID)
		if err != nil {
			return err
		}
		if space != nil && userHasSpaceAccess(uid, space, members) {
			return nil
		}
	}
	if asset.Visibility != VisibilityConversation {
		return errors.New("无权访问文件")
	}
	if strings.TrimSpace(asset.SourceChannelID) == "" {
		return errors.New("来源会话缺失")
	}
	allowed, err := s.repo.CanAccessSource(uid, tenantSpaceID, asset.SourceChannelID, asset.SourceChannelType)
	if err != nil {
		return err
	}
	if !allowed {
		return errors.New("无权访问来源会话")
	}
	return nil
}

func (s *DocumentService) requireSourceAccess(uid, tenantSpaceID, sourceChannelID string, sourceChannelType uint8) error {
	if strings.TrimSpace(sourceChannelID) == "" {
		return nil
	}
	if sourceChannelType == 0 {
		return errors.New("来源会话类型不能为空")
	}
	allowed, err := s.repo.CanAccessSource(uid, tenantSpaceID, sourceChannelID, sourceChannelType)
	if err != nil {
		return err
	}
	if !allowed {
		return errors.New("无权访问来源会话")
	}
	return nil
}

func (s *DocumentService) requireManageable(uid, tenantSpaceID string, asset *DocumentAssetModel) error {
	if asset.OwnerUID == uid || asset.UploaderUID == uid {
		return nil
	}
	if strings.TrimSpace(asset.DocumentSpaceID) == "" {
		return errors.New("无权管理文件")
	}
	space, err := s.repo.GetSpace(asset.DocumentSpaceID, uid, tenantSpaceID)
	if err != nil {
		return err
	}
	if space != nil {
		return s.requireSpaceAdmin(uid, tenantSpaceID, space.SpaceID)
	}
	return errors.New("无权管理文件")
}

func (s *DocumentService) requireEditable(uid, tenantSpaceID string, asset *DocumentAssetModel) error {
	if asset.Status == StatusDeleted {
		return errors.New("回收站文件不可编辑")
	}
	if asset.OwnerUID == uid || asset.UploaderUID == uid {
		return nil
	}
	if strings.TrimSpace(asset.DocumentSpaceID) == "" {
		return errors.New("无权编辑文件")
	}
	return s.requireSpaceEditor(uid, tenantSpaceID, asset.DocumentSpaceID)
}

func (s *DocumentService) resolveSpace(uid, tenantSpaceID, documentSpaceID string) (*DocumentSpaceModel, error) {
	if strings.TrimSpace(documentSpaceID) != "" {
		space, err := s.repo.GetSpace(documentSpaceID, uid, tenantSpaceID)
		if err != nil {
			return nil, err
		}
		if space == nil {
			return nil, errors.New("目标空间不存在")
		}
		return space, nil
	}
	return s.repo.EnsureDefaultSpace(uid, tenantSpaceID)
}

func (s *DocumentService) resolveArchiveSpace(uid, tenantSpaceID string, req ArchiveReq) (*DocumentSpaceModel, error) {
	if strings.TrimSpace(req.DocumentSpaceID) != "" {
		return s.resolveSpace(uid, tenantSpaceID, req.DocumentSpaceID)
	}
	if strings.TrimSpace(req.SourceChannelID) == "" || req.SourceChannelType == 0 {
		return nil, errors.New("文档空间不能为空")
	}
	space, err := s.boundSpaceForSource(uid, tenantSpaceID, req.SourceChannelID, req.SourceChannelType)
	if err != nil {
		return nil, err
	}
	if space == nil {
		return nil, errors.New("该会话未设置群文档存储空间")
	}
	return space, nil
}

func (s *DocumentService) boundSpaceForSource(uid, tenantSpaceID, sourceChannelID string, sourceChannelType uint8) (*DocumentSpaceModel, error) {
	sourceChannelID = strings.TrimSpace(sourceChannelID)
	if sourceChannelID == "" || sourceChannelType == 0 {
		return nil, nil
	}
	binding, err := s.activeBindingForSource(uid, tenantSpaceID, sourceChannelID, sourceChannelType)
	if err != nil || binding == nil {
		return nil, err
	}
	space, err := s.repo.GetSpace(binding.DocumentSpaceID, uid, tenantSpaceID)
	if err != nil || space == nil {
		return space, err
	}
	members, err := s.repo.ListSpaceMembers(uid, tenantSpaceID)
	if err != nil {
		return nil, err
	}
	if !userHasSpaceAccess(uid, space, members) {
		return nil, nil
	}
	return space, nil
}

func (s *DocumentService) activeBindingForSource(uid, tenantSpaceID, sourceChannelID string, sourceChannelType uint8) (*DocumentSpaceBindingModel, error) {
	sourceChannelID = strings.TrimSpace(sourceChannelID)
	if sourceChannelID == "" || sourceChannelType == 0 {
		return nil, nil
	}
	bindings, err := s.repo.ListSpaceBindings(uid, tenantSpaceID)
	if err != nil {
		return nil, err
	}
	for _, binding := range bindings {
		if binding.Status == 1 &&
			binding.SourceChannelID == sourceChannelID &&
			binding.SourceChannelType == sourceChannelType &&
			strings.TrimSpace(binding.DocumentSpaceID) != "" {
			return binding, nil
		}
	}
	return nil, nil
}

func (s *DocumentService) ensureSpaceNameAvailable(currentSpaceID, tenantSpaceID, name string) error {
	spaces, err := s.repo.ListAllSpaces("", tenantSpaceID)
	if err != nil {
		return err
	}
	for _, space := range spaces {
		if space.SpaceID != currentSpaceID && strings.EqualFold(space.Name, name) {
			return errors.New("空间名称已存在")
		}
	}
	return nil
}

func (s *DocumentService) saveOwnerMember(uid, tenantSpaceID string, space *DocumentSpaceModel) error {
	if space == nil || strings.TrimSpace(space.OwnerUID) == "" {
		return nil
	}
	members, err := s.repo.ListSpaceMembers(uid, tenantSpaceID)
	if err == nil {
		for _, member := range members {
			if member.DocumentSpaceID == space.SpaceID && member.UID == space.OwnerUID && member.Status == 1 {
				return nil
			}
		}
	}
	now := nowDBTime()
	member := &DocumentSpaceMemberModel{
		MemberID:        "MEM-" + util.GenerUUID(),
		DocumentSpaceID: space.SpaceID,
		UID:             space.OwnerUID,
		Name:            fallbackString(space.OwnerUID, uid),
		Role:            SpaceRoleOwner,
		Source:          "创建人",
		CreatedBy:       uid,
		TenantSpaceID:   tenantSpaceID,
		Status:          1,
	}
	member.CreatedAt = now
	member.UpdatedAt = now
	return s.repo.SaveSpaceMember(member)
}

func (s *DocumentService) requireSpaceOwner(uid string, space *DocumentSpaceModel) error {
	if space != nil && space.OwnerUID == uid {
		return nil
	}
	return errors.New("仅空间所有者可操作")
}

func (s *DocumentService) requireSpaceAdmin(uid, tenantSpaceID, spaceID string) error {
	role, err := s.spaceRole(uid, tenantSpaceID, spaceID)
	if err != nil {
		return err
	}
	if role == SpaceRoleOwner || role == SpaceRoleAdmin {
		return nil
	}
	return errors.New("无权管理空间")
}

func (s *DocumentService) requireSpaceEditor(uid, tenantSpaceID, spaceID string) error {
	role, err := s.spaceRole(uid, tenantSpaceID, spaceID)
	if err != nil {
		return err
	}
	if role == SpaceRoleOwner || role == SpaceRoleAdmin || role == SpaceRoleEditor {
		return nil
	}
	return errors.New("无权编辑空间文件")
}

func (s *DocumentService) spaceRole(uid, tenantSpaceID, spaceID string) (string, error) {
	space, err := s.repo.GetSpace(spaceID, uid, tenantSpaceID)
	if err != nil {
		return "", err
	}
	if space != nil && space.OwnerUID == uid {
		return SpaceRoleOwner, nil
	}
	members, err := s.repo.ListSpaceMembers(uid, tenantSpaceID)
	if err != nil {
		return "", err
	}
	for _, member := range members {
		if member.DocumentSpaceID == spaceID && member.UID == uid && member.Status == 1 {
			return normalizeSpaceRole(member.Role), nil
		}
	}
	return "", nil
}

func (s *DocumentService) addEvent(uid, tenantSpaceID, assetID, action, detail string) error {
	now := nowDBTime()
	event := &DocumentEventModel{
		EventID:       "EVT-" + util.GenerUUID(),
		AssetID:       assetID,
		ActorUID:      uid,
		Action:        action,
		Detail:        detail,
		TenantSpaceID: tenantSpaceID,
	}
	event.CreatedAt = now
	event.UpdatedAt = now
	return s.repo.AddEvent(event)
}

func (s *DocumentService) buildState(uid, tenantSpaceID string) (*DocumentStateResp, error) {
	spaces, err := s.repo.ListSpaces(uid, tenantSpaceID)
	if err != nil {
		return nil, err
	}
	allSpaces, err := s.repo.ListAllSpaces(uid, tenantSpaceID)
	if err != nil {
		return nil, err
	}
	assets, err := s.repo.ListAssets(uid, tenantSpaceID)
	if err != nil {
		return nil, err
	}
	bindings, err := s.repo.ListSpaceBindings(uid, tenantSpaceID)
	if err != nil {
		return nil, err
	}
	members, err := s.repo.ListSpaceMembers(uid, tenantSpaceID)
	if err != nil {
		return nil, err
	}
	events, err := s.repo.ListEvents(uid, tenantSpaceID, 50)
	if err != nil {
		return nil, err
	}

	spaceByID := make(map[string]*DocumentSpaceModel, len(allSpaces))
	fileCountBySpace := make(map[string]int)
	bindingsBySpace := make(map[string][]*DocumentSpaceBindingResp)
	membersBySpace := make(map[string][]*DocumentSpaceMemberResp)
	for _, space := range allSpaces {
		spaceByID[space.SpaceID] = space
	}
	for _, binding := range bindings {
		if binding.Status == 1 && binding.DocumentSpaceID != "" {
			bindingsBySpace[binding.DocumentSpaceID] = append(bindingsBySpace[binding.DocumentSpaceID], &DocumentSpaceBindingResp{
				ID:          binding.BindingID,
				ChannelID:   binding.SourceChannelID,
				ChannelType: binding.SourceChannelType,
				Name:        binding.SourceName,
				CreatedBy:   binding.CreatedBy,
			})
		}
	}
	for _, member := range members {
		if member.Status != 1 || member.DocumentSpaceID == "" {
			continue
		}
		membersBySpace[member.DocumentSpaceID] = append(membersBySpace[member.DocumentSpaceID], &DocumentSpaceMemberResp{
			UID:      member.UID,
			Name:     fallbackString(member.Name, member.UID),
			Role:     normalizeSpaceRole(member.Role),
			Source:   fallbackString(member.Source, "手动添加"),
			JoinedAt: formatDBTime(member.CreatedAt),
		})
	}
	for _, space := range spaces {
		hasOwner := false
		for _, member := range membersBySpace[space.SpaceID] {
			if member.UID == space.OwnerUID {
				member.Role = SpaceRoleOwner
				hasOwner = true
				break
			}
		}
		if !hasOwner {
			membersBySpace[space.SpaceID] = append([]*DocumentSpaceMemberResp{{
				UID:      space.OwnerUID,
				Name:     space.OwnerUID,
				Role:     SpaceRoleOwner,
				Source:   "创建人",
				JoinedAt: formatDBTime(space.CreatedAt),
			}}, membersBySpace[space.SpaceID]...)
		}
	}
	accessibleAssets := make([]*DocumentAssetModel, 0, len(assets))
	accessibleAssetIDs := make(map[string]struct{}, len(assets))
	accessibleSpaceIDs := make(map[string]struct{}, len(spaces))
	for _, space := range spaces {
		accessibleSpaceIDs[space.SpaceID] = struct{}{}
	}
	sourceAccess, err := s.accessibleSourcesForState(uid, tenantSpaceID, assets, spaceByID, members)
	if err != nil {
		return nil, err
	}
	for _, asset := range assets {
		space := spaceByID[asset.DocumentSpaceID]
		allowed, err := s.canReadAssetWithSources(uid, tenantSpaceID, asset, space, members, sourceAccess)
		if err != nil {
			return nil, err
		}
		if !allowed {
			continue
		}
		accessibleAssets = append(accessibleAssets, asset)
		accessibleAssetIDs[asset.AssetID] = struct{}{}
		if asset.Status == StatusArchived && asset.DocumentSpaceID != "" {
			fileCountBySpace[asset.DocumentSpaceID]++
		}
	}

	resp := &DocumentStateResp{
		Files:  make([]*DocumentAssetResp, 0, len(accessibleAssets)),
		Spaces: make([]*DocumentSpaceResp, 0, len(spaces)),
		Audits: make([]*DocumentAuditResp, 0, len(events)),
	}
	for _, asset := range accessibleAssets {
		resp.Files = append(resp.Files, assetToResp(
			asset,
			spaceByID[asset.DocumentSpaceID],
			membersBySpace[asset.DocumentSpaceID],
			uid,
		))
	}
	for _, space := range spaces {
		resp.Spaces = append(resp.Spaces, &DocumentSpaceResp{
			ID:                 space.SpaceID,
			Name:               space.Name,
			Owner:              space.OwnerUID,
			FileCount:          fileCountBySpace[space.SpaceID],
			MemberCount:        len(membersBySpace[space.SpaceID]),
			Members:            ensureMembersSlice(membersBySpace[space.SpaceID]),
			BoundConversations: ensureBindingsSlice(bindingsBySpace[space.SpaceID]),
			PinnedFileIDs:      []string{},
			Description:        space.Description,
		})
	}
	for _, event := range events {
		if _, ok := accessibleAssetIDs[event.AssetID]; !ok {
			if _, ok := accessibleSpaceIDs[event.AssetID]; !ok {
				continue
			}
		}
		resp.Audits = append(resp.Audits, &DocumentAuditResp{
			ID:     event.EventID,
			Time:   formatDBTime(event.CreatedAt),
			Actor:  event.ActorUID,
			Action: event.Action,
			Target: event.AssetID,
			Detail: event.Detail,
		})
	}
	return resp, nil
}

func (s *DocumentService) accessibleSourcesForState(uid, tenantSpaceID string, assets []*DocumentAssetModel, spaces map[string]*DocumentSpaceModel, members []*DocumentSpaceMemberModel) (map[string]bool, error) {
	sources := make([]documentSourceRef, 0)
	seen := make(map[string]struct{})
	for _, asset := range assets {
		if asset == nil || asset.TenantSpaceID != tenantSpaceID {
			continue
		}
		if asset.OwnerUID == uid || asset.UploaderUID == uid {
			continue
		}
		if space := spaces[asset.DocumentSpaceID]; space != nil && userHasSpaceAccess(uid, space, members) {
			continue
		}
		if asset.Visibility != VisibilityConversation || strings.TrimSpace(asset.SourceChannelID) == "" {
			continue
		}
		key := documentSourceKey(asset.SourceChannelID, asset.SourceChannelType)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		sources = append(sources, documentSourceRef{
			ChannelID:   asset.SourceChannelID,
			ChannelType: asset.SourceChannelType,
		})
	}
	return s.repo.CanAccessSources(uid, tenantSpaceID, sources)
}

func (s *DocumentService) canReadAsset(uid, tenantSpaceID string, asset *DocumentAssetModel, space *DocumentSpaceModel, members []*DocumentSpaceMemberModel) (bool, error) {
	return s.canReadAssetWithSources(uid, tenantSpaceID, asset, space, members, nil)
}

func (s *DocumentService) canReadAssetWithSources(uid, tenantSpaceID string, asset *DocumentAssetModel, space *DocumentSpaceModel, members []*DocumentSpaceMemberModel, sourceAccess map[string]bool) (bool, error) {
	if asset == nil || asset.TenantSpaceID != tenantSpaceID {
		return false, nil
	}
	if asset.OwnerUID == uid || asset.UploaderUID == uid {
		return true, nil
	}
	if space != nil && userHasSpaceAccess(uid, space, members) {
		return true, nil
	}
	if asset.Visibility != VisibilityConversation || strings.TrimSpace(asset.SourceChannelID) == "" {
		return false, nil
	}
	if sourceAccess != nil {
		return sourceAccess[documentSourceKey(asset.SourceChannelID, asset.SourceChannelType)], nil
	}
	return s.repo.CanAccessSource(uid, tenantSpaceID, asset.SourceChannelID, asset.SourceChannelType)
}

func assetToResp(asset *DocumentAssetModel, space *DocumentSpaceModel, members []*DocumentSpaceMemberResp, uid string) *DocumentAssetResp {
	spaceName := "会话文件"
	if space != nil {
		spaceName = space.Name
		if space.Status != 1 {
			spaceName += "（已停用）"
		}
	}
	createdAt := formatDBTime(asset.CreatedAt)
	lastAccessAt := createdAt
	if asset.LastAccessAt != nil {
		lastAccessAt = asset.LastAccessAt.String()
	}
	owner := fallbackString(asset.OwnerName, asset.OwnerUID)
	uploader := fallbackString(asset.UploaderName, asset.UploaderUID)
	flow := []string{}
	if asset.SourceName != "" {
		flow = append(flow, "来自"+asset.SourceName)
	}
	if asset.Status == StatusArchived && spaceName != "" {
		flow = append(flow, "归档到"+spaceName)
	}
	if asset.SourceType == SourceTypeApp {
		flow = []string{"直接上传", "保存到" + spaceName}
	}
	if asset.Status == StatusDeleted {
		flow = append(flow, "移动到回收站")
	}
	sourceRef := buildSourceRef(asset, createdAt)
	permissions := buildDocumentPermissions(asset, space, members, uid)
	return &DocumentAssetResp{
		ID:                asset.AssetID,
		Name:              asset.Name,
		Kind:              asset.Kind,
		Extension:         strings.TrimPrefix(asset.Extension, "."),
		Size:              asset.Size,
		StoragePath:       "",
		Owner:             owner,
		Uploader:          uploader,
		SourceName:        asset.SourceName,
		SourceChannelID:   asset.SourceChannelID,
		SourceChannelType: asset.SourceChannelType,
		SourceType:        asset.SourceType,
		SpaceName:         spaceName,
		Visibility:        asset.Visibility,
		Status:            asset.Status,
		CreatedAt:         createdAt,
		LastAccessAt:      lastAccessAt,
		Downloads:         asset.Downloads,
		Previewable:       asset.Previewable == 1,
		Flow:              flow,
		SourceRef:         sourceRef,
		Permissions:       permissions,
	}
}

func ensureMembersSlice(items []*DocumentSpaceMemberResp) []*DocumentSpaceMemberResp {
	if items == nil {
		return []*DocumentSpaceMemberResp{}
	}
	return items
}

func ensureBindingsSlice(items []*DocumentSpaceBindingResp) []*DocumentSpaceBindingResp {
	if items == nil {
		return []*DocumentSpaceBindingResp{}
	}
	return items
}

func buildSourceRef(asset *DocumentAssetModel, createdAt string) *DocumentSourceRefResp {
	if strings.TrimSpace(asset.SourceChannelID) == "" && strings.TrimSpace(asset.SourceMessageID) == "" {
		return nil
	}
	return &DocumentSourceRefResp{
		ChannelID:   asset.SourceChannelID,
		ChannelType: asset.SourceChannelType,
		ChannelName: asset.SourceName,
		MessageID:   asset.SourceMessageID,
		MessageSeq:  asset.SourceMessageSeq,
		SenderUID:   fallbackString(asset.UploaderUID, asset.OwnerUID),
		SenderName:  fallbackString(asset.UploaderName, asset.OwnerName),
		SentAt:      createdAt,
	}
}

func buildDocumentPermissions(asset *DocumentAssetModel, space *DocumentSpaceModel, members []*DocumentSpaceMemberResp, uid string) DocumentPermissionResp {
	isOwner := asset.OwnerUID == uid || asset.UploaderUID == uid
	isSpaceOwner := space != nil && space.OwnerUID == uid
	spaceRole := ""
	if isSpaceOwner {
		spaceRole = SpaceRoleOwner
	} else {
		for _, member := range members {
			if member.UID == uid {
				spaceRole = normalizeSpaceRole(member.Role)
				break
			}
		}
	}
	isSpaceAdmin := spaceRole == SpaceRoleOwner || spaceRole == SpaceRoleAdmin
	isSpaceEditor := isSpaceAdmin || spaceRole == SpaceRoleEditor
	hasSpaceAccess := isSpaceOwner || spaceRole != ""
	canManage := isOwner || isSpaceAdmin
	canEdit := isOwner || isSpaceEditor
	canRead := isOwner || hasSpaceAccess || asset.Visibility == VisibilityConversation
	active := asset.Status != StatusDeleted
	reasons := []string{}
	if isOwner {
		reasons = append(reasons, "上传者/拥有者")
	}
	if isSpaceOwner {
		reasons = append(reasons, "空间管理员")
	} else if isSpaceAdmin {
		reasons = append(reasons, "空间管理员")
	} else if spaceRole == SpaceRoleEditor {
		reasons = append(reasons, "空间编辑者")
	}
	if asset.Visibility == VisibilitySpace && space != nil {
		reasons = append(reasons, "空间成员可访问")
	}
	if asset.Visibility == VisibilityConversation {
		reasons = append(reasons, "来源会话成员可访问")
	}
	if len(reasons) == 0 {
		reasons = append(reasons, "拥有访问权限")
	}
	summary := strings.Join(reasons, "、")
	return DocumentPermissionResp{
		CanPreview:  active && canRead && asset.Previewable == 1,
		CanDownload: active && canRead,
		CanArchive:  active && asset.Status == StatusConversation,
		CanEdit:     active && canEdit,
		CanDelete:   active && canManage,
		CanRestore:  asset.Status == StatusDeleted && canManage,
		CanManage:   canManage,
		Summary:     summary,
		Reasons:     reasons,
	}
}

func normalizeSpaceRole(role string) string {
	switch role {
	case SpaceRoleOwner, SpaceRoleAdmin, SpaceRoleEditor, SpaceRoleViewer:
		return role
	default:
		return SpaceRoleViewer
	}
}

func userHasSpaceAccess(uid string, space *DocumentSpaceModel, members []*DocumentSpaceMemberModel) bool {
	if space == nil {
		return false
	}
	if space.OwnerUID == uid {
		return true
	}
	for _, member := range members {
		if member.DocumentSpaceID == space.SpaceID && member.UID == uid && member.Status == 1 {
			return true
		}
	}
	return false
}

func fallbackString(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func documentSourceKey(sourceChannelID string, sourceChannelType uint8) string {
	return fmt.Sprintf("%s:%d", sourceChannelID, sourceChannelType)
}

type memoryRepository struct {
	spaces                []*DocumentSpaceModel
	bindings              []*DocumentSpaceBindingModel
	members               []*DocumentSpaceMemberModel
	users                 []*DocumentUserCandidateModel
	groups                []*DocumentGroupCandidateModel
	assets                []*DocumentAssetModel
	events                []*DocumentEventModel
	accessibleSources     map[string]map[string]bool
	canAccessSourceCalls  int
	canAccessSourcesCalls int
}

func newMemoryRepository() *memoryRepository {
	return &memoryRepository{}
}

func (r *memoryRepository) ListSpaces(uid, tenantSpaceID string) ([]*DocumentSpaceModel, error) {
	items := make([]*DocumentSpaceModel, 0, len(r.spaces))
	for _, space := range r.spaces {
		if space.Status == 1 && space.TenantSpaceID == tenantSpaceID && userHasSpaceAccess(uid, space, r.members) {
			items = append(items, cloneSpace(space))
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].CreatedAt.String() > items[j].CreatedAt.String() })
	return items, nil
}

func (r *memoryRepository) ListAllSpaces(uid, tenantSpaceID string) ([]*DocumentSpaceModel, error) {
	items := make([]*DocumentSpaceModel, 0, len(r.spaces))
	for _, space := range r.spaces {
		if space.TenantSpaceID == tenantSpaceID {
			items = append(items, cloneSpace(space))
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].CreatedAt.String() > items[j].CreatedAt.String() })
	return items, nil
}

func (r *memoryRepository) EnsureDefaultSpace(uid, tenantSpaceID string) (*DocumentSpaceModel, error) {
	for _, space := range r.spaces {
		if space.TenantSpaceID == tenantSpaceID && space.Status == 1 {
			return cloneSpace(space), nil
		}
	}
	now := nowDBTime()
	space := &DocumentSpaceModel{
		SpaceID:       "DOCSPACE-" + util.GenerUUID(),
		Name:          "团队文档空间",
		Description:   "默认文档空间",
		OwnerUID:      uid,
		TenantSpaceID: tenantSpaceID,
		Status:        1,
	}
	space.CreatedAt = now
	space.UpdatedAt = now
	r.spaces = append(r.spaces, space)
	return cloneSpace(space), nil
}

func (r *memoryRepository) GetSpace(spaceID, uid, tenantSpaceID string) (*DocumentSpaceModel, error) {
	for _, space := range r.spaces {
		if space.SpaceID == spaceID && space.TenantSpaceID == tenantSpaceID && space.Status == 1 {
			return cloneSpace(space), nil
		}
	}
	return nil, nil
}

func (r *memoryRepository) SaveSpace(space *DocumentSpaceModel) error {
	r.spaces = append(r.spaces, cloneSpace(space))
	return nil
}

func (r *memoryRepository) UpdateSpace(space *DocumentSpaceModel) error {
	for i, item := range r.spaces {
		if item.SpaceID == space.SpaceID && item.TenantSpaceID == space.TenantSpaceID {
			r.spaces[i] = cloneSpace(space)
			return nil
		}
	}
	return errors.New("space not found")
}

func (r *memoryRepository) ListSpaceBindings(uid, tenantSpaceID string) ([]*DocumentSpaceBindingModel, error) {
	items := make([]*DocumentSpaceBindingModel, 0, len(r.bindings))
	for _, binding := range r.bindings {
		if binding.TenantSpaceID == tenantSpaceID && binding.Status == 1 {
			items = append(items, cloneBinding(binding))
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].CreatedAt.String() > items[j].CreatedAt.String() })
	return items, nil
}

func (r *memoryRepository) SaveSpaceBinding(binding *DocumentSpaceBindingModel) error {
	for i, item := range r.bindings {
		if item.TenantSpaceID == binding.TenantSpaceID &&
			item.SourceChannelID == binding.SourceChannelID &&
			item.SourceChannelType == binding.SourceChannelType {
			if item.DocumentSpaceID == binding.DocumentSpaceID {
				r.bindings[i] = cloneBinding(binding)
				return nil
			}
			cp := cloneBinding(item)
			cp.Status = 0
			cp.UpdatedAt = nowDBTime()
			r.bindings[i] = cp
		}
	}
	r.bindings = append(r.bindings, cloneBinding(binding))
	return nil
}

func (r *memoryRepository) RemoveSpaceBinding(spaceID, bindingID, tenantSpaceID string) error {
	for i, item := range r.bindings {
		if item.BindingID == bindingID && item.DocumentSpaceID == spaceID && item.TenantSpaceID == tenantSpaceID {
			cp := cloneBinding(item)
			cp.Status = 0
			cp.UpdatedAt = nowDBTime()
			r.bindings[i] = cp
			return nil
		}
	}
	return nil
}

func (r *memoryRepository) ListSpaceMembers(uid, tenantSpaceID string) ([]*DocumentSpaceMemberModel, error) {
	items := make([]*DocumentSpaceMemberModel, 0, len(r.members))
	for _, member := range r.members {
		if member.TenantSpaceID == tenantSpaceID && member.Status == 1 {
			items = append(items, cloneMember(member))
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].CreatedAt.String() > items[j].CreatedAt.String() })
	return items, nil
}

func (r *memoryRepository) SearchUsers(uid, tenantSpaceID, keyword string, limit int) ([]*DocumentUserCandidateModel, error) {
	keyword = strings.ToLower(strings.TrimSpace(keyword))
	if keyword == "" {
		return []*DocumentUserCandidateModel{}, nil
	}
	if limit <= 0 {
		limit = 20
	}
	items := make([]*DocumentUserCandidateModel, 0, limit)
	for _, user := range r.users {
		if user == nil || strings.TrimSpace(user.UID) == "" {
			continue
		}
		haystack := strings.ToLower(strings.Join([]string{
			user.UID,
			user.Name,
			user.Username,
			user.Email,
			user.Phone,
		}, " "))
		if strings.Contains(haystack, keyword) {
			cp := *user
			items = append(items, &cp)
			if len(items) >= limit {
				break
			}
		}
	}
	return items, nil
}

func (r *memoryRepository) SearchGroups(uid, tenantSpaceID, keyword string, limit int) ([]*DocumentGroupCandidateModel, error) {
	keyword = strings.ToLower(strings.TrimSpace(keyword))
	if keyword == "" {
		return []*DocumentGroupCandidateModel{}, nil
	}
	if limit <= 0 {
		limit = 20
	}
	items := make([]*DocumentGroupCandidateModel, 0, limit)
	for _, group := range r.groups {
		if group == nil || strings.TrimSpace(group.GroupNo) == "" {
			continue
		}
		if group.SpaceID != "" && group.SpaceID != tenantSpaceID {
			continue
		}
		haystack := strings.ToLower(strings.Join([]string{group.GroupNo, group.Name}, " "))
		if strings.Contains(haystack, keyword) {
			cp := *group
			items = append(items, &cp)
			if len(items) >= limit {
				break
			}
		}
	}
	return items, nil
}

func (r *memoryRepository) SaveSpaceMember(member *DocumentSpaceMemberModel) error {
	for i, item := range r.members {
		if item.TenantSpaceID == member.TenantSpaceID &&
			item.DocumentSpaceID == member.DocumentSpaceID &&
			item.UID == member.UID {
			existing := cloneMember(member)
			existing.CreatedAt = item.CreatedAt
			existing.Source = item.Source
			existing.CreatedBy = item.CreatedBy
			r.members[i] = existing
			return nil
		}
	}
	r.members = append(r.members, cloneMember(member))
	return nil
}

func (r *memoryRepository) RemoveSpaceMember(spaceID, memberUID, tenantSpaceID string) error {
	for i, item := range r.members {
		if item.TenantSpaceID == tenantSpaceID && item.DocumentSpaceID == spaceID && item.UID == memberUID {
			cp := cloneMember(item)
			cp.Status = 0
			cp.UpdatedAt = nowDBTime()
			r.members[i] = cp
			return nil
		}
	}
	return nil
}

func (r *memoryRepository) ListAssets(uid, tenantSpaceID string) ([]*DocumentAssetModel, error) {
	items := make([]*DocumentAssetModel, 0, len(r.assets))
	for _, asset := range r.assets {
		if asset.TenantSpaceID == tenantSpaceID {
			items = append(items, cloneAsset(asset))
		}
	}
	sort.Slice(items, func(i, j int) bool {
		left := items[i].CreatedAt.String()
		right := items[j].CreatedAt.String()
		if items[i].LastAccessAt != nil {
			left = items[i].LastAccessAt.String()
		}
		if items[j].LastAccessAt != nil {
			right = items[j].LastAccessAt.String()
		}
		return left > right
	})
	return items, nil
}

func (r *memoryRepository) GetAsset(assetID, uid, tenantSpaceID string) (*DocumentAssetModel, error) {
	for _, asset := range r.assets {
		if asset.AssetID == assetID && asset.TenantSpaceID == tenantSpaceID {
			return cloneAsset(asset), nil
		}
	}
	return nil, nil
}

func (r *memoryRepository) SaveAsset(asset *DocumentAssetModel) error {
	r.assets = append(r.assets, cloneAsset(asset))
	return nil
}

func (r *memoryRepository) UpdateAsset(asset *DocumentAssetModel) error {
	for i, item := range r.assets {
		if item.AssetID == asset.AssetID && item.TenantSpaceID == asset.TenantSpaceID {
			r.assets[i] = cloneAsset(asset)
			return nil
		}
	}
	return errors.New("asset not found")
}

func (r *memoryRepository) DeleteAsset(assetID, tenantSpaceID string) error {
	next := r.assets[:0]
	for _, item := range r.assets {
		if item.AssetID == assetID && item.TenantSpaceID == tenantSpaceID {
			continue
		}
		next = append(next, item)
	}
	r.assets = next
	return nil
}

func (r *memoryRepository) AddEvent(event *DocumentEventModel) error {
	r.events = append([]*DocumentEventModel{cloneEvent(event)}, r.events...)
	return nil
}

func (r *memoryRepository) ListEvents(uid, tenantSpaceID string, limit int) ([]*DocumentEventModel, error) {
	items := make([]*DocumentEventModel, 0, len(r.events))
	for _, event := range r.events {
		if event.TenantSpaceID == tenantSpaceID {
			items = append(items, cloneEvent(event))
		}
		if limit > 0 && len(items) >= limit {
			break
		}
	}
	return items, nil
}

func (r *memoryRepository) CanAccessSource(uid, tenantSpaceID, sourceChannelID string, sourceChannelType uint8) (bool, error) {
	r.canAccessSourceCalls++
	if r.accessibleSources == nil {
		return sourceChannelID != "", nil
	}
	members := r.accessibleSources[fmt.Sprintf("%s:%d", sourceChannelID, sourceChannelType)]
	if members == nil {
		return false, nil
	}
	return members[uid], nil
}

func (r *memoryRepository) CanAccessSources(uid, tenantSpaceID string, sources []documentSourceRef) (map[string]bool, error) {
	r.canAccessSourcesCalls++
	result := make(map[string]bool, len(sources))
	for _, source := range sources {
		if strings.TrimSpace(source.ChannelID) == "" {
			continue
		}
		key := documentSourceKey(source.ChannelID, source.ChannelType)
		if r.accessibleSources == nil {
			result[key] = true
			continue
		}
		members := r.accessibleSources[key]
		if members != nil && members[uid] {
			result[key] = true
		}
	}
	return result, nil
}

func cloneSpace(space *DocumentSpaceModel) *DocumentSpaceModel {
	if space == nil {
		return nil
	}
	cp := *space
	return &cp
}

func cloneAsset(asset *DocumentAssetModel) *DocumentAssetModel {
	if asset == nil {
		return nil
	}
	cp := *asset
	if asset.LastAccessAt != nil {
		v := *asset.LastAccessAt
		cp.LastAccessAt = &v
	}
	return &cp
}

func cloneBinding(binding *DocumentSpaceBindingModel) *DocumentSpaceBindingModel {
	if binding == nil {
		return nil
	}
	cp := *binding
	return &cp
}

func cloneMember(member *DocumentSpaceMemberModel) *DocumentSpaceMemberModel {
	if member == nil {
		return nil
	}
	cp := *member
	return &cp
}

func cloneEvent(event *DocumentEventModel) *DocumentEventModel {
	if event == nil {
		return nil
	}
	cp := *event
	return &cp
}
