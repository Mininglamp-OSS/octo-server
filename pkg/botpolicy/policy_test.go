package botpolicy

import "testing"

func TestAvatarCapabilitiesAreExplicit(t *testing.T) {
	for _, capability := range []Capability{Message, ReadGroup, ReadThread, File, Card, Commands, Runtime} {
		if !Allows(Avatar, capability) {
			t.Errorf("avatar must allow %s", capability)
		}
	}
	for _, capability := range []Capability{ManageGroup, ManageThread, Search, Voice, OBO, SpacePrincipal, "future"} {
		if Allows(Avatar, capability) {
			t.Errorf("avatar must deny %s", capability)
		}
	}
	if Allows(Kind("future"), Message) {
		t.Fatal("unknown kind must fail closed")
	}
	if Allows(App, ReadGroup) || Allows(App, ReadThread) {
		t.Fatal("App Bot must remain DM-only")
	}
}

func TestCreatorAbsenceDoesNotGrantAvatarIdentity(t *testing.T) {
	cases := []Identity{
		{Kind: User, Status: 1},
		{Kind: Avatar, Status: 1, PublicationState: Published},
		{Kind: Avatar, Status: 1, PublicationState: Published, Scope: ScopeSpace},
		{Kind: Avatar, Status: 1, PublicationState: Published, Scope: ScopePlatform, CreatorUID: "human"},
		{Kind: Avatar, Status: 0, PublicationState: Unpublished, Scope: ScopePlatform},
	}
	for _, identity := range cases {
		if identity.ActiveAvatar() {
			t.Errorf("invalid avatar accepted: %+v", identity)
		}
	}
	valid := Identity{Kind: Avatar, Status: 1, PublicationState: Published, Scope: ScopeSpace, SpaceID: "space"}
	if !valid.ActiveAvatar() {
		t.Fatal("published scoped avatar must be recognized")
	}
}
