package space

import (
	"testing"
)

func TestCheckMembershipEmptyArgs(t *testing.T) {
	ok, err := CheckMembership(nil, "", "uid1")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("expected false for empty spaceID")
	}

	ok, err = CheckMembership(nil, "space1", "")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("expected false for empty uid")
	}
}

func TestCheckBothMembersEmptyArgs(t *testing.T) {
	ok, err := CheckBothMembers(nil, "", "uid1", "uid2")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("expected false for empty spaceID")
	}

	ok, err = CheckBothMembers(nil, "space1", "", "uid2")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("expected false for empty uid1")
	}
}

func TestHaveCommonSpaceEmptyArgs(t *testing.T) {
	tests := []struct {
		name string
		uid1 string
		uid2 string
	}{
		{"both_empty", "", ""},
		{"uid1_empty", "", "u2"},
		{"uid2_empty", "u1", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ok, err := HaveCommonSpace(nil, tt.uid1, tt.uid2)
			if err != nil {
				t.Fatal(err)
			}
			if ok {
				t.Errorf("expected false for %s", tt.name)
			}
		})
	}
}

func TestCheckMembershipForCleanupEmptyArgs(t *testing.T) {
	ok, err := CheckMembershipForCleanup(nil, "", "uid1")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("expected false for empty spaceID")
	}

	ok, err = CheckMembershipForCleanup(nil, "space1", "")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("expected false for empty uid")
	}
}
