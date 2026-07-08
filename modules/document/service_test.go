package document

import (
	"testing"
)

func TestServiceUploadCreatesSpaceAsset(t *testing.T) {
	repo := newMemoryRepository()
	repo.spaces = []*DocumentSpaceModel{{
		SpaceID:       "doc-space-1",
		Name:          "产品部公共空间",
		OwnerUID:      "u1",
		TenantSpaceID: "tenant-1",
		Status:        1,
	}}
	service := NewDocumentService(repo)

	state, err := service.Upload("u1", "tenant-1", UploadReq{
		Name:            "需求清单.xlsx",
		Extension:       ".xlsx",
		Size:            2048,
		StoragePath:     "/documents/需求清单.xlsx",
		DocumentSpaceID: "doc-space-1",
		UploaderName:    "陈一",
	})

	if err != nil {
		t.Fatalf("Upload returned error: %v", err)
	}
	if len(state.Files) != 1 {
		t.Fatalf("expected one file, got %d", len(state.Files))
	}
	file := state.Files[0]
	if file.Name != "需求清单.xlsx" {
		t.Fatalf("unexpected file name: %s", file.Name)
	}
	if file.Status != StatusArchived {
		t.Fatalf("expected archived status, got %s", file.Status)
	}
	if file.SourceName != SourceNameDirectUpload {
		t.Fatalf("expected direct upload source, got %s", file.SourceName)
	}
	if file.Uploader != "陈一" {
		t.Fatalf("expected uploader display name, got %s", file.Uploader)
	}
	if file.Owner != "陈一" {
		t.Fatalf("expected owner display name, got %s", file.Owner)
	}
	if state.Spaces[0].FileCount != 1 {
		t.Fatalf("expected space file count 1, got %d", state.Spaces[0].FileCount)
	}
}

func TestServiceStateDoesNotLeakSpaceAssetsToNonMember(t *testing.T) {
	repo := newMemoryRepository()
	repo.spaces = []*DocumentSpaceModel{{
		SpaceID:       "private-space",
		Name:          "私有项目空间",
		OwnerUID:      "owner",
		TenantSpaceID: "tenant-1",
		Status:        1,
	}}
	repo.assets = []*DocumentAssetModel{{
		AssetID:         "asset-private",
		Name:            "private-plan.pdf",
		Kind:            KindPDF,
		Extension:       ".pdf",
		Size:            2048,
		StoragePath:     "common/private-plan.pdf",
		UploaderUID:     "owner",
		UploaderName:    "空间 Owner",
		OwnerUID:        "owner",
		TenantSpaceID:   "tenant-1",
		DocumentSpaceID: "private-space",
		Visibility:      VisibilitySpace,
		Status:          StatusArchived,
		Previewable:     1,
	}}
	service := NewDocumentService(repo)

	state, err := service.State("outsider", "tenant-1")

	if err != nil {
		t.Fatalf("State returned error: %v", err)
	}
	if len(state.Files) != 0 {
		t.Fatalf("expected no leaked files, got %#v", state.Files)
	}
	for _, space := range state.Spaces {
		if space.ID == "private-space" {
			t.Fatalf("expected private space to stay hidden from non-member: %#v", state.Spaces)
		}
	}
}

func TestServiceBuildStateBatchesConversationSourceAccess(t *testing.T) {
	repo := newMemoryRepository()
	repo.assets = []*DocumentAssetModel{
		{
			AssetID:           "asset-visible",
			Name:              "visible.pdf",
			Kind:              KindPDF,
			Extension:         ".pdf",
			Size:              2048,
			StoragePath:       "common/visible.pdf",
			UploaderUID:       "owner",
			OwnerUID:          "owner",
			TenantSpaceID:     "tenant-1",
			Visibility:        VisibilityConversation,
			SourceChannelID:   "group-visible",
			SourceChannelType: 2,
			Status:            StatusConversation,
			Previewable:       1,
		},
		{
			AssetID:           "asset-hidden",
			Name:              "hidden.pdf",
			Kind:              KindPDF,
			Extension:         ".pdf",
			Size:              2048,
			StoragePath:       "common/hidden.pdf",
			UploaderUID:       "owner",
			OwnerUID:          "owner",
			TenantSpaceID:     "tenant-1",
			Visibility:        VisibilityConversation,
			SourceChannelID:   "group-hidden",
			SourceChannelType: 2,
			Status:            StatusConversation,
			Previewable:       1,
		},
	}
	repo.accessibleSources = map[string]map[string]bool{
		"group-visible:2": {"reader": true},
	}
	service := NewDocumentService(repo)

	state, err := service.buildState("reader", "tenant-1")
	if err != nil {
		t.Fatalf("buildState returned error: %v", err)
	}
	if len(state.Files) != 1 || state.Files[0].ID != "asset-visible" {
		t.Fatalf("expected only visible conversation asset, got %#v", state.Files)
	}
	if repo.canAccessSourceCalls != 0 {
		t.Fatalf("expected buildState to avoid per-asset CanAccessSource calls, got %d", repo.canAccessSourceCalls)
	}
	if repo.canAccessSourcesCalls != 1 {
		t.Fatalf("expected one batched CanAccessSources call, got %d", repo.canAccessSourcesCalls)
	}
}

func TestServiceArchiveMessageFileMovesConversationAssetToSpace(t *testing.T) {
	repo := newMemoryRepository()
	repo.spaces = []*DocumentSpaceModel{{
		SpaceID:       "doc-space-1",
		Name:          "华东交付部空间",
		OwnerUID:      "u1",
		TenantSpaceID: "tenant-1",
		Status:        1,
	}}
	repo.assets = []*DocumentAssetModel{{
		AssetID:           "asset-1",
		Name:              "现场计划.pdf",
		Kind:              KindPDF,
		Extension:         ".pdf",
		Size:              4096,
		SourceType:        SourceTypeGroup,
		SourceName:        "华东项目交付群",
		SourceChannelID:   "group-1",
		SourceChannelType: 2,
		UploaderUID:       "u2",
		UploaderName:      "张沐",
		OwnerUID:          "u2",
		TenantSpaceID:     "tenant-1",
		DocumentSpaceID:   "",
		Visibility:        VisibilityConversation,
		Status:            StatusConversation,
		Previewable:       1,
	}}
	service := NewDocumentService(repo)

	state, err := service.Archive("u1", "tenant-1", ArchiveReq{
		AssetID:         "asset-1",
		DocumentSpaceID: "doc-space-1",
	})

	if err != nil {
		t.Fatalf("Archive returned error: %v", err)
	}
	file := state.Files[0]
	if file.Status != StatusArchived {
		t.Fatalf("expected archived, got %s", file.Status)
	}
	if file.SpaceName != "华东交付部空间" {
		t.Fatalf("expected target space name, got %s", file.SpaceName)
	}
	if state.Spaces[0].FileCount != 1 {
		t.Fatalf("expected target space count 1, got %d", state.Spaces[0].FileCount)
	}
}

func TestServiceArchiveNewAssetUsesCallerAsUploader(t *testing.T) {
	repo := newMemoryRepository()
	repo.spaces = []*DocumentSpaceModel{{
		SpaceID:       "doc-space-1",
		Name:          "产品部公共空间",
		OwnerUID:      "u1",
		TenantSpaceID: "tenant-1",
		Status:        1,
	}}
	service := NewDocumentService(repo)

	_, err := service.Archive("u1", "tenant-1", ArchiveReq{
		AssetID:         "msg-file-1",
		Name:            "会议纪要.pdf",
		Extension:       ".pdf",
		StoragePath:     "common/documents/meeting.pdf",
		DocumentSpaceID: "doc-space-1",
		UploaderUID:     "victim",
		UploaderName:    "伪造上传人",
	})

	if err != nil {
		t.Fatalf("Archive returned error: %v", err)
	}
	if len(repo.assets) != 1 {
		t.Fatalf("expected one archived asset, got %d", len(repo.assets))
	}
	if repo.assets[0].UploaderUID != "u1" {
		t.Fatalf("expected archive uploader to be caller, got %s", repo.assets[0].UploaderUID)
	}
	if repo.assets[0].AssetID == "msg-file-1" {
		t.Fatalf("expected archive asset id to be server-generated")
	}
	if got := repo.assets[0].AssetID; len(got) < 4 || got[:4] != "DOC-" {
		t.Fatalf("expected generated DOC- asset id, got %s", got)
	}
}

func TestServiceUploadAndArchiveRequireSpaceEditor(t *testing.T) {
	repo := newMemoryRepository()
	repo.spaces = []*DocumentSpaceModel{{
		SpaceID:       "private-space",
		Name:          "私有交付空间",
		OwnerUID:      "owner",
		TenantSpaceID: "tenant-1",
		Status:        1,
	}}
	service := NewDocumentService(repo)

	if _, err := service.Upload("outsider", "tenant-1", UploadReq{
		Name:            "unauthorized.pdf",
		Extension:       ".pdf",
		StoragePath:     "common/unauthorized.pdf",
		DocumentSpaceID: "private-space",
	}); err == nil {
		t.Fatalf("expected Upload to reject non-editor space writes")
	}

	if _, err := service.Archive("outsider", "tenant-1", ArchiveReq{
		AssetID:         "asset-unauthorized",
		Name:            "unauthorized.pdf",
		Extension:       ".pdf",
		StoragePath:     "common/unauthorized.pdf",
		DocumentSpaceID: "private-space",
	}); err == nil {
		t.Fatalf("expected Archive to reject non-editor space writes")
	}
}

func TestServiceArchiveRejectsRehomingAssetWithoutEditAccess(t *testing.T) {
	repo := newMemoryRepository()
	repo.spaces = []*DocumentSpaceModel{
		{
			SpaceID:       "victim-space",
			Name:          "受限空间",
			OwnerUID:      "victim",
			TenantSpaceID: "tenant-1",
			Status:        1,
		},
		{
			SpaceID:       "attacker-space",
			Name:          "攻击者空间",
			OwnerUID:      "attacker",
			TenantSpaceID: "tenant-1",
			Status:        1,
		},
	}
	repo.assets = []*DocumentAssetModel{{
		AssetID:         "asset-victim",
		Name:            "private-plan.pdf",
		Kind:            KindPDF,
		Extension:       ".pdf",
		Size:            2048,
		StoragePath:     "common/private-plan.pdf",
		SourceType:      SourceTypeApp,
		SourceName:      SourceNameDirectUpload,
		UploaderUID:     "victim",
		OwnerUID:        "victim",
		TenantSpaceID:   "tenant-1",
		DocumentSpaceID: "victim-space",
		Visibility:      VisibilitySpace,
		Status:          StatusArchived,
		Previewable:     1,
	}}
	service := NewDocumentService(repo)

	if _, err := service.Archive("attacker", "tenant-1", ArchiveReq{
		AssetID:         "asset-victim",
		DocumentSpaceID: "attacker-space",
	}); err == nil {
		t.Fatalf("expected Archive to reject rehoming an asset without edit access")
	}
	if got := repo.assets[0].DocumentSpaceID; got != "victim-space" {
		t.Fatalf("expected asset to remain in victim space, got %s", got)
	}
}

func TestServiceArchiveRejectsConversationAssetWithForgedSource(t *testing.T) {
	repo := newMemoryRepository()
	repo.spaces = []*DocumentSpaceModel{
		{
			SpaceID:       "attacker-space",
			Name:          "攻击者空间",
			OwnerUID:      "attacker",
			TenantSpaceID: "tenant-1",
			Status:        1,
		},
	}
	repo.assets = []*DocumentAssetModel{{
		AssetID:           "asset-victim-conversation",
		Name:              "secret-chat-file.pdf",
		Kind:              KindPDF,
		Extension:         ".pdf",
		Size:              2048,
		StoragePath:       "common/secret-chat-file.pdf",
		SourceType:        SourceTypeGroup,
		SourceName:        "受限群聊",
		SourceChannelID:   "group-victim",
		SourceChannelType: 2,
		UploaderUID:       "victim",
		OwnerUID:          "victim",
		TenantSpaceID:     "tenant-1",
		Visibility:        VisibilityConversation,
		Status:            StatusConversation,
		Previewable:       1,
	}}
	repo.accessibleSources = map[string]map[string]bool{
		"group-attacker:2": {"attacker": true},
		"group-victim:2":   {"attacker": false},
	}
	service := NewDocumentService(repo)

	if _, err := service.Archive("attacker", "tenant-1", ArchiveReq{
		AssetID:           "asset-victim-conversation",
		DocumentSpaceID:   "attacker-space",
		SourceChannelID:   "group-attacker",
		SourceChannelType: 2,
	}); err == nil {
		t.Fatalf("expected Archive to reject forged source access for conversation assets")
	}
	if got := repo.assets[0].DocumentSpaceID; got != "" {
		t.Fatalf("expected conversation asset to remain unarchived, got space %s", got)
	}
}

func TestServiceArchiveRejectsUnreadableConversationAssetWithEmptySource(t *testing.T) {
	repo := newMemoryRepository()
	repo.spaces = []*DocumentSpaceModel{{
		SpaceID:       "attacker-space",
		Name:          "攻击者空间",
		OwnerUID:      "attacker",
		TenantSpaceID: "tenant-1",
		Status:        1,
	}}
	repo.assets = []*DocumentAssetModel{{
		AssetID:       "asset-empty-source",
		Name:          "orphan-chat-file.pdf",
		Kind:          KindPDF,
		Extension:     ".pdf",
		Size:          2048,
		StoragePath:   "common/orphan-chat-file.pdf",
		UploaderUID:   "victim",
		OwnerUID:      "victim",
		TenantSpaceID: "tenant-1",
		Visibility:    VisibilityConversation,
		Status:        StatusConversation,
		Previewable:   1,
	}}
	service := NewDocumentService(repo)

	if _, err := service.Archive("attacker", "tenant-1", ArchiveReq{
		AssetID:         "asset-empty-source",
		DocumentSpaceID: "attacker-space",
	}); err == nil {
		t.Fatalf("expected Archive to reject unreadable empty-source conversation asset")
	}
	if got := repo.assets[0].DocumentSpaceID; got != "" {
		t.Fatalf("expected conversation asset to remain unarchived, got space %s", got)
	}
}

func TestServiceBindConversationRequiresSourceAccess(t *testing.T) {
	repo := newMemoryRepository()
	repo.spaces = []*DocumentSpaceModel{{
		SpaceID:       "doc-space-1",
		Name:          "产品部公共空间",
		OwnerUID:      "owner",
		TenantSpaceID: "tenant-1",
		Status:        1,
	}}
	repo.accessibleSources = map[string]map[string]bool{
		"group-secret:2": {"owner": false},
	}
	service := NewDocumentService(repo)

	if _, err := service.BindConversation("owner", "tenant-1", BindConversationReq{
		DocumentSpaceID:   "doc-space-1",
		SourceChannelID:   "group-secret",
		SourceChannelType: 2,
		SourceName:        "秘密项目群",
	}); err == nil {
		t.Fatalf("expected BindConversation to reject inaccessible source channels")
	}
}

func TestServiceBindConversationDoesNotRebindInaccessibleSpace(t *testing.T) {
	repo := newMemoryRepository()
	repo.spaces = []*DocumentSpaceModel{
		{
			SpaceID:       "current-space",
			Name:          "当前空间",
			OwnerUID:      "u1",
			TenantSpaceID: "tenant-1",
			Status:        1,
		},
		{
			SpaceID:       "private-space",
			Name:          "私有空间",
			OwnerUID:      "u2",
			TenantSpaceID: "tenant-1",
			Status:        1,
		},
	}
	repo.bindings = []*DocumentSpaceBindingModel{{
		BindingID:         "bind-private",
		DocumentSpaceID:   "private-space",
		SourceChannelID:   "group-1",
		SourceChannelType: 2,
		SourceName:        "产品方案讨论群",
		CreatedBy:         "u2",
		TenantSpaceID:     "tenant-1",
		Status:            1,
	}}
	repo.accessibleSources = map[string]map[string]bool{
		"group-1:2": {"u1": true},
	}
	service := NewDocumentService(repo)

	if _, err := service.BindConversation("u1", "tenant-1", BindConversationReq{
		DocumentSpaceID:   "current-space",
		SourceChannelID:   "group-1",
		SourceChannelType: 2,
		SourceName:        "产品方案讨论群",
	}); err == nil {
		t.Fatalf("expected BindConversation to reject rebinding an inaccessible space")
	}
}

func TestServiceBindingDiscoveryHidesInaccessibleBoundSpace(t *testing.T) {
	repo := newMemoryRepository()
	repo.spaces = []*DocumentSpaceModel{
		{
			SpaceID:       "current-space",
			Name:          "当前空间",
			OwnerUID:      "u1",
			TenantSpaceID: "tenant-1",
			Status:        1,
		},
		{
			SpaceID:       "private-space",
			Name:          "私有空间",
			OwnerUID:      "u2",
			TenantSpaceID: "tenant-1",
			Status:        1,
		},
	}
	repo.bindings = []*DocumentSpaceBindingModel{{
		BindingID:         "bind-private",
		DocumentSpaceID:   "private-space",
		SourceChannelID:   "group-1",
		SourceChannelType: 2,
		SourceName:        "产品方案讨论群",
		CreatedBy:         "u2",
		TenantSpaceID:     "tenant-1",
		Status:            1,
	}}
	repo.groups = []*DocumentGroupCandidateModel{{
		GroupNo: "group-1",
		Name:    "产品方案讨论群",
		SpaceID: "tenant-1",
	}}
	service := NewDocumentService(repo)

	candidates, err := service.SearchBindingConversations("u1", "tenant-1", "current-space", "产品")
	if err != nil {
		t.Fatalf("SearchBindingConversations returned error: %v", err)
	}
	if len(candidates) != 1 {
		t.Fatalf("expected one candidate, got %d", len(candidates))
	}
	if candidates[0].BoundSpaceID != "" || candidates[0].BoundSpaceName != "" {
		t.Fatalf("expected inaccessible bound space to stay hidden, got %#v", candidates[0])
	}

	storageSpace, err := service.ChannelStorageSpace("u1", "tenant-1", "group-1", 2)
	if err != nil {
		t.Fatalf("ChannelStorageSpace returned error: %v", err)
	}
	if storageSpace.SpaceID != "" || storageSpace.SpaceName != "" {
		t.Fatalf("expected inaccessible storage space to stay hidden, got %#v", storageSpace)
	}
}

func TestServiceArchiveUsesBoundGroupStorageSpaceWhenSpaceIDEmpty(t *testing.T) {
	repo := newMemoryRepository()
	repo.spaces = []*DocumentSpaceModel{{
		SpaceID:       "doc-space-1",
		Name:          "产品部公共空间",
		OwnerUID:      "u1",
		TenantSpaceID: "tenant-1",
		Status:        1,
	}}
	repo.bindings = []*DocumentSpaceBindingModel{{
		BindingID:         "bind-1",
		DocumentSpaceID:   "doc-space-1",
		SourceChannelID:   "group-1",
		SourceChannelType: 2,
		SourceName:        "产品方案讨论群",
		CreatedBy:         "u1",
		TenantSpaceID:     "tenant-1",
		Status:            1,
	}}
	service := NewDocumentService(repo)

	state, err := service.Archive("u1", "tenant-1", ArchiveReq{
		AssetID:           "msg-1",
		Name:              "需求清单.xlsx",
		Extension:         ".xlsx",
		Size:              2048,
		StoragePath:       "common/documents/demo/requirements.xlsx",
		SourceType:        SourceTypeGroup,
		SourceChannelID:   "group-1",
		SourceChannelType: 2,
		SourceName:        "产品方案讨论群",
		UploaderUID:       "u2",
		UploaderName:      "张沐",
	})

	if err != nil {
		t.Fatalf("Archive returned error: %v", err)
	}
	file := state.Files[0]
	if file.SpaceName != "产品部公共空间" {
		t.Fatalf("expected bound storage space, got %s", file.SpaceName)
	}
	if file.Status != StatusArchived || file.Visibility != VisibilitySpace {
		t.Fatalf("expected archived space file, got status=%s visibility=%s", file.Status, file.Visibility)
	}
}

func TestServiceRebindingGroupKeepsOnlyLatestStorageSpace(t *testing.T) {
	repo := newMemoryRepository()
	repo.spaces = []*DocumentSpaceModel{
		{
			SpaceID:       "doc-space-1",
			Name:          "产品部公共空间",
			OwnerUID:      "u1",
			TenantSpaceID: "tenant-1",
			Status:        1,
		},
		{
			SpaceID:       "doc-space-2",
			Name:          "QA复验空间",
			OwnerUID:      "u1",
			TenantSpaceID: "tenant-1",
			Status:        1,
		},
	}
	repo.groups = []*DocumentGroupCandidateModel{{
		GroupNo: "group-1",
		Name:    "产品方案讨论群",
		SpaceID: "tenant-1",
	}}
	service := NewDocumentService(repo)

	if _, err := service.BindConversation("u1", "tenant-1", BindConversationReq{
		DocumentSpaceID:   "doc-space-1",
		SourceChannelID:   "group-1",
		SourceChannelType: 2,
		SourceName:        "产品方案讨论群",
	}); err != nil {
		t.Fatalf("first BindConversation returned error: %v", err)
	}
	if _, err := service.BindConversation("u1", "tenant-1", BindConversationReq{
		DocumentSpaceID:   "doc-space-2",
		SourceChannelID:   "group-1",
		SourceChannelType: 2,
		SourceName:        "产品方案讨论群",
	}); err != nil {
		t.Fatalf("second BindConversation returned error: %v", err)
	}

	storageSpace, err := service.ChannelStorageSpace("u1", "tenant-1", "group-1", 2)
	if err != nil {
		t.Fatalf("ChannelStorageSpace returned error: %v", err)
	}
	if storageSpace.SpaceName != "QA复验空间" {
		t.Fatalf("expected latest bound space, got %s", storageSpace.SpaceName)
	}
	candidates, err := service.SearchBindingConversations("u1", "tenant-1", "doc-space-2", "产品")
	if err != nil {
		t.Fatalf("SearchBindingConversations returned error: %v", err)
	}
	if len(candidates) != 1 || !candidates[0].AlreadyBoundToCurrentSpace {
		t.Fatalf("expected candidate bound to current space, got %#v", candidates)
	}
}

func TestServiceTrashAndRestoreKeepBusinessClosedLoop(t *testing.T) {
	repo := newMemoryRepository()
	repo.spaces = []*DocumentSpaceModel{{
		SpaceID:       "doc-space-1",
		Name:          "公司制度空间",
		OwnerUID:      "u1",
		TenantSpaceID: "tenant-1",
		Status:        1,
	}}
	repo.assets = []*DocumentAssetModel{{
		AssetID:         "asset-1",
		Name:            "制度更新说明.docx",
		Kind:            KindDoc,
		Extension:       ".docx",
		Size:            1024,
		SourceType:      SourceTypeGroup,
		SourceName:      "行政制度发布群",
		UploaderUID:     "u1",
		UploaderName:    "周岚",
		OwnerUID:        "u1",
		TenantSpaceID:   "tenant-1",
		DocumentSpaceID: "doc-space-1",
		Visibility:      VisibilitySpace,
		Status:          StatusArchived,
		Previewable:     1,
	}}
	service := NewDocumentService(repo)

	deleted, err := service.Trash("u1", "tenant-1", "asset-1")
	if err != nil {
		t.Fatalf("Trash returned error: %v", err)
	}
	if deleted.Files[0].Status != StatusDeleted {
		t.Fatalf("expected deleted, got %s", deleted.Files[0].Status)
	}
	if deleted.Spaces[0].FileCount != 0 {
		t.Fatalf("expected space count 0 after trash, got %d", deleted.Spaces[0].FileCount)
	}

	restored, err := service.Restore("u1", "tenant-1", "asset-1")
	if err != nil {
		t.Fatalf("Restore returned error: %v", err)
	}
	if restored.Files[0].Status != StatusArchived {
		t.Fatalf("expected archived after restore, got %s", restored.Files[0].Status)
	}
	if restored.Spaces[0].FileCount != 1 {
		t.Fatalf("expected space count 1 after restore, got %d", restored.Spaces[0].FileCount)
	}
}

func TestServiceBindConversationShowsOnDocumentSpace(t *testing.T) {
	repo := newMemoryRepository()
	repo.spaces = []*DocumentSpaceModel{{
		SpaceID:       "doc-space-1",
		Name:          "产品部公共空间",
		OwnerUID:      "u1",
		TenantSpaceID: "tenant-1",
		Status:        1,
	}}
	service := NewDocumentService(repo)

	state, err := service.BindConversation("u1", "tenant-1", BindConversationReq{
		DocumentSpaceID:   "doc-space-1",
		SourceChannelID:   "grp_product_docs",
		SourceChannelType: 2,
		SourceName:        "产品方案讨论群",
	})

	if err != nil {
		t.Fatalf("BindConversation returned error: %v", err)
	}
	if len(state.Spaces) != 1 {
		t.Fatalf("expected one space, got %d", len(state.Spaces))
	}
	if got := state.Spaces[0].BoundConversations; len(got) != 1 || got[0].Name != "产品方案讨论群" {
		t.Fatalf("expected bound conversation, got %#v", got)
	}

	unbound, err := service.UnbindConversation("u1", "tenant-1", "doc-space-1", state.Spaces[0].BoundConversations[0].ID)
	if err != nil {
		t.Fatalf("UnbindConversation returned error: %v", err)
	}
	if got := unbound.Spaces[0].BoundConversations; len(got) != 0 {
		t.Fatalf("expected no bound conversation after unbind, got %#v", got)
	}
}

func TestServiceCheckSourceRequiresAccessibleConversation(t *testing.T) {
	repo := newMemoryRepository()
	repo.assets = []*DocumentAssetModel{{
		AssetID:           "asset-1",
		Name:              "产品方案.pdf",
		Kind:              KindPDF,
		Extension:         ".pdf",
		SourceType:        SourceTypeGroup,
		SourceChannelID:   "grp_product_docs",
		SourceChannelType: 2,
		SourceName:        "产品方案讨论群",
		TenantSpaceID:     "tenant-1",
		Status:            StatusConversation,
		Visibility:        VisibilityConversation,
		Previewable:       1,
	}}
	repo.accessibleSources = map[string]map[string]bool{
		"grp_product_docs:2": {
			"u1": true,
			"u2": false,
		},
	}
	service := NewDocumentService(repo)

	allowed, err := service.CheckSource("u1", "tenant-1", "asset-1")
	if err != nil {
		t.Fatalf("CheckSource returned error for allowed user: %v", err)
	}
	if !allowed {
		t.Fatalf("expected allowed user to access source")
	}

	if denied, err := service.CheckSource("u2", "tenant-1", "asset-1"); err == nil || denied {
		t.Fatalf("expected denied user to be rejected, allowed=%v err=%v", denied, err)
	}
}

func TestServiceStateIncludesSourceReferenceAndPermissions(t *testing.T) {
	repo := newMemoryRepository()
	created := nowDBTime()
	repo.spaces = []*DocumentSpaceModel{{
		SpaceID:       "doc-space-1",
		Name:          "产品部公共空间",
		OwnerUID:      "space-owner",
		TenantSpaceID: "tenant-1",
		Status:        1,
	}}
	repo.assets = []*DocumentAssetModel{{
		AssetID:           "asset-1",
		Name:              "需求清单.xlsx",
		Kind:              KindSheet,
		Extension:         ".xlsx",
		Size:              4096,
		SourceType:        SourceTypeGroup,
		SourceName:        "产品方案讨论群",
		SourceChannelID:   "grp_product_docs",
		SourceChannelType: 2,
		SourceMessageID:   "2406171002",
		UploaderUID:       "pm_chen",
		UploaderName:      "陈一",
		OwnerUID:          "pm_chen",
		OwnerName:         "陈一",
		TenantSpaceID:     "tenant-1",
		DocumentSpaceID:   "doc-space-1",
		Visibility:        VisibilitySpace,
		Status:            StatusArchived,
		Previewable:       1,
	}}
	repo.assets[0].CreatedAt = created
	repo.accessibleSources = map[string]map[string]bool{
		"grp_product_docs:2": {"pm_chen": true},
	}
	service := NewDocumentService(repo)

	state, err := service.State("pm_chen", "tenant-1")
	if err != nil {
		t.Fatalf("State returned error: %v", err)
	}
	file := state.Files[0]
	if file.SourceRef == nil {
		t.Fatalf("expected source reference")
	}
	if file.SourceRef.MessageID != "2406171002" {
		t.Fatalf("expected source message id, got %s", file.SourceRef.MessageID)
	}
	if file.SourceRef.ChannelID != "grp_product_docs" || file.SourceRef.ChannelType != 2 {
		t.Fatalf("unexpected source channel: %#v", file.SourceRef)
	}
	if file.SourceRef.SenderName != "陈一" {
		t.Fatalf("expected sender display name, got %s", file.SourceRef.SenderName)
	}
	if file.SourceRef.SentAt == "" {
		t.Fatalf("expected source sent time")
	}
	if !file.Permissions.CanPreview || !file.Permissions.CanDownload || !file.Permissions.CanDelete {
		t.Fatalf("owner should have preview/download/delete permissions: %#v", file.Permissions)
	}
	if file.Permissions.Summary == "" {
		t.Fatalf("expected permission summary")
	}
}

func TestServicePreviewRequiresConversationAccess(t *testing.T) {
	repo := newMemoryRepository()
	repo.assets = []*DocumentAssetModel{{
		AssetID:           "asset-1",
		Name:              "客户账号截图.png",
		Kind:              KindImage,
		Extension:         ".png",
		SourceType:        SourceTypeGroup,
		SourceChannelID:   "grp_delivery_docs",
		SourceChannelType: 2,
		SourceName:        "华东项目交付群",
		TenantSpaceID:     "tenant-1",
		Visibility:        VisibilityConversation,
		Status:            StatusConversation,
		Previewable:       1,
	}}
	repo.accessibleSources = map[string]map[string]bool{
		"grp_delivery_docs:2": {"outsider": false},
	}
	service := NewDocumentService(repo)

	if _, err := service.Preview("outsider", "tenant-1", "asset-1"); err == nil {
		t.Fatalf("expected preview to reject users outside the source conversation")
	}
}

func TestServiceTrashRequiresManagePermission(t *testing.T) {
	repo := newMemoryRepository()
	repo.spaces = []*DocumentSpaceModel{{
		SpaceID:       "doc-space-1",
		Name:          "产品部公共空间",
		OwnerUID:      "space-owner",
		TenantSpaceID: "tenant-1",
		Status:        1,
	}}
	repo.assets = []*DocumentAssetModel{{
		AssetID:         "asset-1",
		Name:            "产品方案.pdf",
		Kind:            KindPDF,
		Extension:       ".pdf",
		UploaderUID:     "pm_chen",
		OwnerUID:        "pm_chen",
		TenantSpaceID:   "tenant-1",
		DocumentSpaceID: "doc-space-1",
		Visibility:      VisibilitySpace,
		Status:          StatusArchived,
		Previewable:     1,
	}}
	service := NewDocumentService(repo)

	if _, err := service.Trash("ordinary-user", "tenant-1", "asset-1"); err == nil {
		t.Fatalf("expected trash to reject users without manage permission")
	}
}

func TestServiceRenameAndMoveFileRequireSpaceEditPermission(t *testing.T) {
	repo := newMemoryRepository()
	repo.spaces = []*DocumentSpaceModel{{
		SpaceID:       "space-a",
		Name:          "产品部公共空间",
		OwnerUID:      "owner",
		TenantSpaceID: "tenant-1",
		Status:        1,
	}, {
		SpaceID:       "space-b",
		Name:          "华东交付空间",
		OwnerUID:      "owner",
		TenantSpaceID: "tenant-1",
		Status:        1,
	}}
	repo.members = []*DocumentSpaceMemberModel{{
		MemberID:        "mem-1",
		DocumentSpaceID: "space-a",
		UID:             "editor",
		Name:            "编辑成员",
		Role:            SpaceRoleEditor,
		TenantSpaceID:   "tenant-1",
		Status:          1,
	}, {
		MemberID:        "mem-2",
		DocumentSpaceID: "space-b",
		UID:             "editor",
		Name:            "编辑成员",
		Role:            SpaceRoleEditor,
		TenantSpaceID:   "tenant-1",
		Status:          1,
	}}
	repo.assets = []*DocumentAssetModel{{
		AssetID:         "asset-1",
		Name:            "旧名称.docx",
		Kind:            KindDoc,
		Extension:       ".docx",
		UploaderUID:     "uploader",
		OwnerUID:        "uploader",
		TenantSpaceID:   "tenant-1",
		DocumentSpaceID: "space-a",
		Visibility:      VisibilitySpace,
		Status:          StatusArchived,
		Previewable:     1,
	}}
	service := NewDocumentService(repo)

	renamed, err := service.RenameAsset("editor", "tenant-1", "asset-1", RenameAssetReq{Name: "新名称.docx"})
	if err != nil {
		t.Fatalf("RenameAsset returned error: %v", err)
	}
	if renamed.Files[0].Name != "新名称.docx" {
		t.Fatalf("expected renamed file, got %s", renamed.Files[0].Name)
	}

	moved, err := service.MoveAsset("editor", "tenant-1", "asset-1", MoveAssetReq{DocumentSpaceID: "space-b"})
	if err != nil {
		t.Fatalf("MoveAsset returned error: %v", err)
	}
	if moved.Files[0].SpaceName != "华东交付空间" {
		t.Fatalf("expected moved file space, got %s", moved.Files[0].SpaceName)
	}
	if !moved.Files[0].Permissions.CanEdit {
		t.Fatalf("expected editor to keep edit permission after move: %#v", moved.Files[0].Permissions)
	}
	if moved.Files[0].Permissions.CanManage || moved.Files[0].Permissions.CanDelete {
		t.Fatalf("editor should not gain delete/manage permission: %#v", moved.Files[0].Permissions)
	}

	movedAgain, err := service.MoveAsset("editor", "tenant-1", "asset-1", MoveAssetReq{DocumentSpaceID: "space-a"})
	if err != nil {
		t.Fatalf("MoveAsset second move returned error: %v", err)
	}
	if movedAgain.Files[0].SpaceName != "产品部公共空间" {
		t.Fatalf("expected file to move back, got %s", movedAgain.Files[0].SpaceName)
	}

	if _, err := service.RenameAsset("viewer", "tenant-1", "asset-1", RenameAssetReq{Name: "不可改.docx"}); err == nil {
		t.Fatalf("expected viewer rename to be rejected")
	}
}

func TestServiceSpaceLifecycleAndMembers(t *testing.T) {
	repo := newMemoryRepository()
	service := NewDocumentService(repo)

	created, err := service.CreateSpace("owner", "tenant-1", SaveSpaceReq{Name: "交付知识库", Description: "交付材料"})
	if err != nil {
		t.Fatalf("CreateSpace returned error: %v", err)
	}
	if len(created.Spaces) != 1 || created.Spaces[0].Name != "交付知识库" {
		t.Fatalf("expected created space, got %#v", created.Spaces)
	}
	if created.Spaces[0].MemberCount != 1 || created.Spaces[0].Members[0].Role != SpaceRoleOwner {
		t.Fatalf("expected owner member, got %#v", created.Spaces[0].Members)
	}

	spaceID := created.Spaces[0].ID
	updated, err := service.UpdateSpace("owner", "tenant-1", spaceID, SaveSpaceReq{Name: "交付资料库", Description: "项目交付资料"})
	if err != nil {
		t.Fatalf("UpdateSpace returned error: %v", err)
	}
	if updated.Spaces[0].Name != "交付资料库" || updated.Spaces[0].Description != "项目交付资料" {
		t.Fatalf("expected updated space info, got %#v", updated.Spaces[0])
	}

	withMember, err := service.UpsertSpaceMember("owner", "tenant-1", spaceID, SaveSpaceMemberReq{UID: "u2", Name: "刘青", Role: SpaceRoleEditor})
	if err != nil {
		t.Fatalf("UpsertSpaceMember returned error: %v", err)
	}
	if withMember.Spaces[0].MemberCount != 2 {
		t.Fatalf("expected two members, got %d", withMember.Spaces[0].MemberCount)
	}

	repo.members[1].Source = "验收预置"
	roleUpdated, err := service.UpsertSpaceMember("owner", "tenant-1", spaceID, SaveSpaceMemberReq{UID: "u2", Name: "刘青", Role: SpaceRoleAdmin})
	if err != nil {
		t.Fatalf("UpsertSpaceMember role update returned error: %v", err)
	}
	var updatedMember *DocumentSpaceMemberResp
	for _, member := range roleUpdated.Spaces[0].Members {
		if member.UID == "u2" {
			updatedMember = member
			break
		}
	}
	if updatedMember == nil {
		t.Fatalf("expected updated member in state")
	}
	if updatedMember.Role != SpaceRoleAdmin {
		t.Fatalf("expected updated role admin, got %s", updatedMember.Role)
	}
	if updatedMember.Source != "验收预置" {
		t.Fatalf("expected role update to preserve source, got %s", updatedMember.Source)
	}

	withoutMember, err := service.RemoveSpaceMember("owner", "tenant-1", spaceID, "u2")
	if err != nil {
		t.Fatalf("RemoveSpaceMember returned error: %v", err)
	}
	if withoutMember.Spaces[0].MemberCount != 1 {
		t.Fatalf("expected one member after removal, got %d", withoutMember.Spaces[0].MemberCount)
	}

	disabled, err := service.DisableSpace("owner", "tenant-1", spaceID)
	if err != nil {
		t.Fatalf("DisableSpace returned error: %v", err)
	}
	if len(disabled.Spaces) != 0 {
		t.Fatalf("expected disabled space hidden from active state, got %#v", disabled.Spaces)
	}
}

func TestServiceSearchSpaceMemberCandidates(t *testing.T) {
	repo := newMemoryRepository()
	repo.spaces = []*DocumentSpaceModel{{
		SpaceID:       "space-product",
		Name:          "产品部公共空间",
		OwnerUID:      "owner",
		TenantSpaceID: "tenant-1",
		Status:        1,
	}}
	repo.members = []*DocumentSpaceMemberModel{{
		MemberID:        "mem-owner",
		DocumentSpaceID: "space-product",
		UID:             "owner",
		Name:            "空间所有者",
		Role:            SpaceRoleOwner,
		TenantSpaceID:   "tenant-1",
		Status:          1,
	}, {
		MemberID:        "mem-qa",
		DocumentSpaceID: "space-product",
		UID:             "qa_wang01",
		Name:            "王敏",
		Role:            SpaceRoleEditor,
		TenantSpaceID:   "tenant-1",
		Status:          1,
	}}
	repo.users = []*DocumentUserCandidateModel{{
		UID:      "qa_wang01",
		Name:     "王敏",
		Username: "qa_wang01",
	}, {
		UID:      "sales_wang01",
		Name:     "王强",
		Username: "sales_wang01",
		Email:    "sales@example.com",
	}, {
		UID:      "legal_chen01",
		Name:     "陈法务",
		Username: "legal_chen01",
	}}
	service := NewDocumentService(repo)

	candidates, err := service.SearchSpaceMemberCandidates("owner", "tenant-1", "space-product", "wang")
	if err != nil {
		t.Fatalf("SearchSpaceMemberCandidates returned error: %v", err)
	}
	if len(candidates) != 2 {
		t.Fatalf("expected two candidates, got %#v", candidates)
	}
	if !candidates[0].AlreadyMember {
		t.Fatalf("expected existing member to be marked already member: %#v", candidates[0])
	}
	if candidates[1].AlreadyMember {
		t.Fatalf("expected non-member candidate to be selectable: %#v", candidates[1])
	}
	if _, err := service.SearchSpaceMemberCandidates("qa_wang01", "tenant-1", "space-product", "wang"); err == nil {
		t.Fatalf("expected non-admin member search to be rejected")
	}
}

func TestServiceStateUsesStableEmptyArraysForSpaces(t *testing.T) {
	repo := newMemoryRepository()
	repo.spaces = []*DocumentSpaceModel{{
		SpaceID:       "doc-space-1",
		Name:          "产品部公共空间",
		OwnerUID:      "owner",
		TenantSpaceID: "tenant-1",
		Status:        1,
	}}
	service := NewDocumentService(repo)

	state, err := service.State("owner", "tenant-1")
	if err != nil {
		t.Fatalf("State returned error: %v", err)
	}
	if len(state.Spaces) != 1 {
		t.Fatalf("expected one active space, got %d", len(state.Spaces))
	}
	if state.Spaces[0].Members == nil {
		t.Fatalf("expected members to be an empty/stable array, got nil")
	}
	if state.Spaces[0].BoundConversations == nil {
		t.Fatalf("expected bound conversations to be an empty/stable array, got nil")
	}
	if state.Spaces[0].PinnedFileIDs == nil {
		t.Fatalf("expected pinned file ids to be an empty/stable array, got nil")
	}
}

func TestServiceStateKeepsDisabledSpaceNameForHistoricalFiles(t *testing.T) {
	repo := newMemoryRepository()
	repo.spaces = []*DocumentSpaceModel{{
		SpaceID:       "space-disabled",
		Name:          "停用交付空间",
		OwnerUID:      "owner",
		TenantSpaceID: "tenant-1",
		Status:        0,
	}}
	repo.assets = []*DocumentAssetModel{{
		AssetID:         "asset-1",
		Name:            "历史交付材料.pdf",
		Kind:            KindPDF,
		Extension:       ".pdf",
		UploaderUID:     "owner",
		OwnerUID:        "owner",
		TenantSpaceID:   "tenant-1",
		DocumentSpaceID: "space-disabled",
		Visibility:      VisibilitySpace,
		Status:          StatusArchived,
		Previewable:     1,
	}}
	service := NewDocumentService(repo)

	state, err := service.State("owner", "tenant-1")
	if err != nil {
		t.Fatalf("State returned error: %v", err)
	}
	if len(state.Spaces) != 1 {
		t.Fatalf("expected EnsureDefaultSpace to create one active space, got %#v", state.Spaces)
	}
	if state.Spaces[0].Name == "停用交付空间" {
		t.Fatalf("disabled space should not appear in active space list")
	}
	if len(state.Files) != 1 {
		t.Fatalf("expected one historical file, got %d", len(state.Files))
	}
	if state.Files[0].SpaceName != "停用交付空间（已停用）" {
		t.Fatalf("expected disabled space name to be preserved, got %s", state.Files[0].SpaceName)
	}
}

func TestServicePermanentDeleteAndEmptyTrash(t *testing.T) {
	repo := newMemoryRepository()
	repo.assets = []*DocumentAssetModel{{
		AssetID:       "asset-1",
		Name:          "待清理.docx",
		UploaderUID:   "u1",
		OwnerUID:      "u1",
		TenantSpaceID: "tenant-1",
		Status:        StatusDeleted,
		Visibility:    VisibilitySpace,
	}, {
		AssetID:       "asset-2",
		Name:          "待清理2.docx",
		UploaderUID:   "u1",
		OwnerUID:      "u1",
		TenantSpaceID: "tenant-1",
		Status:        StatusDeleted,
		Visibility:    VisibilitySpace,
	}}
	service := NewDocumentService(repo)

	afterDelete, err := service.PermanentDelete("u1", "tenant-1", "asset-1")
	if err != nil {
		t.Fatalf("PermanentDelete returned error: %v", err)
	}
	if len(afterDelete.Files) != 1 || afterDelete.Files[0].ID != "asset-2" {
		t.Fatalf("expected only asset-2 to remain, got %#v", afterDelete.Files)
	}

	emptied, err := service.EmptyTrash("u1", "tenant-1")
	if err != nil {
		t.Fatalf("EmptyTrash returned error: %v", err)
	}
	if len(emptied.Files) != 0 {
		t.Fatalf("expected empty state after empty trash, got %#v", emptied.Files)
	}
}

func TestServiceEmptyTrashAuditDoesNotLeakToNonMember(t *testing.T) {
	repo := newMemoryRepository()
	repo.spaces = []*DocumentSpaceModel{{
		SpaceID:       "private-space",
		Name:          "私有空间",
		OwnerUID:      "owner",
		TenantSpaceID: "tenant-1",
		Status:        1,
	}}
	repo.assets = []*DocumentAssetModel{{
		AssetID:         "asset-private-trash",
		Name:            "private-trash.pdf",
		UploaderUID:     "owner",
		OwnerUID:        "owner",
		TenantSpaceID:   "tenant-1",
		DocumentSpaceID: "private-space",
		Status:          StatusDeleted,
		Visibility:      VisibilitySpace,
	}}
	service := NewDocumentService(repo)

	if _, err := service.EmptyTrash("owner", "tenant-1"); err != nil {
		t.Fatalf("EmptyTrash returned error: %v", err)
	}
	state, err := service.State("outsider", "tenant-1")
	if err != nil {
		t.Fatalf("State returned error: %v", err)
	}
	for _, audit := range state.Audits {
		if audit.Target == "trash" {
			t.Fatalf("expected trash audit to stay hidden from non-member, got %#v", state.Audits)
		}
	}
}
