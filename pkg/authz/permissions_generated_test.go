package authz

import "testing"

func TestGeneratedPermissionsMatchManifest(t *testing.T) {
	manifest := repositoryManifest(t)
	if PermissionContractSchemaVersion != manifest.SchemaVersion {
		t.Fatalf("generated schema version = %d, want %d", PermissionContractSchemaVersion, manifest.SchemaVersion)
	}
	if len(GeneratedPermissions) != len(manifest.Permissions) {
		t.Fatalf("generated permission count = %d, want %d", len(GeneratedPermissions), len(manifest.Permissions))
	}
	for _, permission := range manifest.Permissions {
		metadata, ok := GeneratedPermissions[permission.Key]
		if !ok || !IsKnownPermission(permission.Key) {
			t.Errorf("generated registry is missing %q", permission.Key)
			continue
		}
		if metadata.Resource != permission.Resource ||
			metadata.Action != permission.Action ||
			metadata.Description != permission.Description ||
			metadata.Sensitivity != permission.Sensitivity {
			t.Errorf("generated metadata for %q differs from manifest", permission.Key)
		}
	}
	if IsKnownPermission("unknown.permission") {
		t.Error("generated registry accepted an unknown permission")
	}
}

func TestGeneratedWorkplaceOperationsMatchManifest(t *testing.T) {
	want := map[string]struct {
		method     string
		path       string
		handler    string
		permission string
	}{
		OperationWorkplaceCategoryCreate:     {"POST", "/v1/manager/workplace/category", "manager.addCategory", PermissionWorkplaceCategoryWrite},
		OperationWorkplaceCategoryList:       {"GET", "/v1/manager/workplace/category", "manager.getCategory", PermissionWorkplaceCategoryRead},
		OperationWorkplaceCategoryReorder:    {"PUT", "/v1/manager/workplace/category/reorder", "manager.reorderCategory", PermissionWorkplaceCategoryWrite},
		OperationWorkplaceCategoryDelete:     {"DELETE", "/v1/manager/workplace/categorys/:category_no", "manager.deleteCategory", PermissionWorkplaceCategoryWrite},
		OperationWorkplaceCategoryUpdate:     {"PUT", "/v1/manager/workplace/categorys/:category_no", "manager.updateCategory", PermissionWorkplaceCategoryWrite},
		OperationWorkplaceCategoryAppList:    {"GET", "/v1/manager/workplace/categorys/:category_no/app", "manager.getCategoryApps", PermissionWorkplaceAppRead},
		OperationWorkplaceCategoryAppReorder: {"PUT", "/v1/manager/workplace/categorys/:category_no/app/reorder", "manager.reorderCategoryApp", PermissionWorkplaceCategoryAppReorder},
		OperationWorkplaceCategoryAppCreate:  {"POST", "/v1/manager/workplace/categorys/:category_no/app", "manager.addCategoryApp", PermissionWorkplaceAppWrite},
		OperationWorkplaceCategoryAppDelete:  {"DELETE", "/v1/manager/workplace/categorys/:category_no/apps/:app_id", "manager.deleteCategoryApp", PermissionWorkplaceAppWrite},
		OperationWorkplaceAppCreate:          {"POST", "/v1/manager/workplace/app", "manager.addApp", PermissionWorkplaceAppWrite},
		OperationWorkplaceAppList:            {"GET", "/v1/manager/workplace/app", "manager.getApps", PermissionWorkplaceAppRead},
		OperationWorkplaceAppUpdate:          {"PUT", "/v1/manager/workplace/apps/:app_id", "manager.updateApp", PermissionWorkplaceAppWrite},
		OperationWorkplaceAppDelete:          {"DELETE", "/v1/manager/workplace/apps/:app_id", "manager.deleteApp", PermissionWorkplaceAppWrite},
		OperationWorkplaceBannerCreate:       {"POST", "/v1/manager/workplace/banner", "manager.addBanner", PermissionWorkplaceBannerWrite},
		OperationWorkplaceBannerList:         {"GET", "/v1/manager/workplace/banner", "manager.getBanners", PermissionWorkplaceBannerRead},
		OperationWorkplaceBannerDelete:       {"DELETE", "/v1/manager/workplace/banners/:banner_no", "manager.deleteBanner", PermissionWorkplaceBannerWrite},
		OperationWorkplaceBannerUpdate:       {"PUT", "/v1/manager/workplace/banners/:banner_no", "manager.updateBanner", PermissionWorkplaceBannerWrite},
		OperationWorkplaceBannerReorder:      {"PUT", "/v1/manager/workplace/banner/reorder", "manager.reorderBanner", PermissionWorkplaceBannerWrite},
	}
	if got := len(want); got != 18 {
		t.Fatalf("workplace operation fixture count = %d, want 18", got)
	}
	for operationID, expected := range want {
		metadata, ok := LookupOperation(operationID)
		if !ok {
			t.Errorf("generated operation registry is missing %q", operationID)
			continue
		}
		if metadata.Method != expected.method || metadata.Path != expected.path || metadata.Handler != expected.handler || metadata.Permission != expected.permission {
			t.Errorf("operation %q = %#v, want method=%q path=%q handler=%q permission=%q", operationID, metadata, expected.method, expected.path, expected.handler, expected.permission)
		}
		if metadata.Module != "workplace" || metadata.Scope != ScopeGlobalAdmin || len(metadata.GateSites) != 1 {
			t.Errorf("operation %q has unexpected module/scope/gates: %#v", operationID, metadata)
		}
	}
	if _, ok := LookupOperation("workplace.unknown"); ok {
		t.Error("generated operation registry accepted an unknown operation")
	}
}

func TestLookupOperationReturnsDefensiveCopies(t *testing.T) {
	metadata, ok := LookupOperation(OperationWorkplaceCategoryCreate)
	if !ok {
		t.Fatal("LookupOperation() did not find workplace operation")
	}
	metadata.GateSites[0] = "mutated"
	metadata.BusinessACL = &BusinessACL{Type: "mutated"}

	got, ok := LookupOperation(OperationWorkplaceCategoryCreate)
	if !ok || got.GateSites[0] == "mutated" || got.BusinessACL != nil {
		t.Fatalf("LookupOperation() returned mutable internal state: %#v", got)
	}
}
