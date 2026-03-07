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
