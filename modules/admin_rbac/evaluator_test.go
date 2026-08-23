package adminrbac

import (
	"errors"
	"reflect"
	"testing"
)

func TestEvaluateUnionsActiveRolePermissionsDeterministically(t *testing.T) {
	result, err := Evaluate("u1", []RoleSnapshot{
		{RoleKey: "writer", AuthorizationVersion: 2, Status: activeStatus, Permissions: []string{"user.create", "user.read"}},
		{RoleKey: "reader", AuthorizationVersion: 4, Status: activeStatus, Permissions: []string{"dashboard.read", "user.read"}},
		{RoleKey: "disabled", AuthorizationVersion: 7, Status: 1 - activeStatus, Permissions: []string{"system_setting.write"}},
	})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if want := []string{"dashboard.read", "user.create", "user.read"}; !reflect.DeepEqual(result.Permissions, want) {
		t.Fatalf("permissions = %#v, want %#v", result.Permissions, want)
	}
	wantVersions := []RoleVersion{{RoleKey: "disabled", AuthorizationVersion: 7}, {RoleKey: "reader", AuthorizationVersion: 4}, {RoleKey: "writer", AuthorizationVersion: 2}}
	if !reflect.DeepEqual(result.RoleVersions, wantVersions) {
		t.Fatalf("role versions = %#v, want %#v", result.RoleVersions, wantVersions)
	}
}

func TestEvaluateRejectsUnknownPermission(t *testing.T) {
	_, err := Evaluate("u1", []RoleSnapshot{{RoleKey: "bad", Status: activeStatus, Permissions: []string{"unknown.permission"}}})
	if !errors.Is(err, ErrInvalidPermission) {
		t.Fatalf("error = %v, want ErrInvalidPermission", err)
	}
}

func TestAllowsEffectiveUsesOnlyThePermissionKey(t *testing.T) {
	result, err := Evaluate("u1", []RoleSnapshot{{RoleKey: "reader", Status: activeStatus, Permissions: []string{"user.read"}}})
	if err != nil {
		t.Fatal(err)
	}
	allowed, err := allowsEffective(result, "user.read")
	if err != nil || !allowed {
		t.Fatalf("global permission = (%v, %v), want (true, nil)", allowed, err)
	}
	if allowed, err := allowsEffective(result, "unknown.permission"); allowed || err != ErrInvalidPermission {
		t.Fatalf("unknown permission = (%v, %v), want (false, ErrInvalidPermission)", allowed, err)
	}
}
