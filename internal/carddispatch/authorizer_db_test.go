package carddispatch

import (
	"context"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-server/modules/botidentity"
	"github.com/Mininglamp-OSS/octo-server/modules/thread"
	"github.com/gocraft/dbr/v2"
	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newAuthorizerDB(t *testing.T) *dbr.Session {
	t.Helper()
	conn, err := dbr.Open("sqlite3", ":memory:", nil)
	require.NoError(t, err)
	conn.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = conn.Close() })
	session := conn.NewSession(nil)
	statements := []string{
		`CREATE TABLE space (space_id TEXT PRIMARY KEY, status INTEGER NOT NULL)`,
		`CREATE TABLE space_member (space_id TEXT NOT NULL, uid TEXT NOT NULL, status INTEGER NOT NULL)`,
		`CREATE TABLE friend (uid TEXT NOT NULL, to_uid TEXT NOT NULL, is_deleted INTEGER NOT NULL)`,
		`CREATE TABLE "group" (group_no TEXT PRIMARY KEY, space_id TEXT NOT NULL, status INTEGER NOT NULL)`,
		`CREATE TABLE group_member (group_no TEXT NOT NULL, uid TEXT NOT NULL, status INTEGER NOT NULL, is_deleted INTEGER NOT NULL, is_external INTEGER NOT NULL, robot INTEGER NOT NULL)`,
		`CREATE TABLE thread (short_id TEXT NOT NULL, group_no TEXT NOT NULL, status INTEGER NOT NULL, PRIMARY KEY (group_no, short_id))`,
		`INSERT INTO space(space_id,status) VALUES ('space-a',1),('space-b',1),('space-disabled',2)`,
		`INSERT INTO space_member(space_id,uid,status) VALUES
			('space-a','bot-user',1),
			('space-a','owner-a',1),
			('space-a','friend-a',1),
			('space-a','member-a',1),
			('space-a','stranger-a',1),
			('space-b','outside-b',1),
			('space-disabled','disabled-member',1)`,
		`INSERT INTO friend(uid,to_uid,is_deleted) VALUES
			('bot-user','friend-a',0),
			('app-platform','member-a',0),
			('app-space','member-a',0)`,
		`INSERT INTO "group"(group_no,space_id,status) VALUES
			('group-ok','space-a',1),
			('group-no-member','space-a',1),
			('group-blacklisted','space-a',1),
			('group-external','space-a',1),
			('group-deleted-member','space-a',1),
			('group-disabled','space-a',0),
			('group-disbanded','space-a',2),
			('group-wrong-space','space-b',1)`,
		`INSERT INTO group_member(group_no,uid,status,is_deleted,is_external,robot) VALUES
			('group-ok','bot-user',1,0,0,1),
			('group-blacklisted','bot-user',2,0,0,1),
			('group-external','bot-user',1,0,1,1),
			('group-deleted-member','bot-user',1,1,0,1)`,
		`INSERT INTO thread(short_id,group_no,status) VALUES
			('123456789012345','group-ok',1),
			('123456789012346','group-ok',2),
			('123456789012347','group-ok',3),
			('123456789012348','group-no-member',1)`,
	}
	for _, statement := range statements {
		_, err := session.Exec(statement)
		require.NoError(t, err, "statement=%s", statement)
	}
	return session
}

func TestDBAuthorizerPolicyMatrix(t *testing.T) {
	session := newAuthorizerDB(t)
	authorizer := NewDBAuthorizer(session)
	userBot := &botidentity.Identity{UID: "bot-user", Kind: botidentity.KindUserBot, CreatorUID: "owner-a"}
	platformApp := &botidentity.Identity{UID: "app-platform", Kind: botidentity.KindAppBot, AppScope: botidentity.ScopePlatform}
	spaceApp := &botidentity.Identity{UID: "app-space", Kind: botidentity.KindAppBot, AppScope: botidentity.ScopeSpace, AppSpaceID: "space-a"}
	standard := AuthorizationPolicy{SpacePolicy: SpacePolicyMembership, GroupPolicy: GroupPolicyMemberRequired}
	systemNotification := AuthorizationPolicy{SpacePolicy: SpacePolicySystemNotification, GroupPolicy: GroupPolicyMemberRequired}
	memberExempt := AuthorizationPolicy{SpacePolicy: SpacePolicyMembership, GroupPolicy: GroupPolicyMemberExempt}

	cases := []struct {
		name     string
		identity *botidentity.Identity
		target   Target
		policy   AuthorizationPolicy
		allowed  bool
	}{
		{name: "user bot creator dm", identity: userBot, target: Target{SpaceID: "space-a", ChannelID: "owner-a", ChannelType: common.ChannelTypePerson.Uint8()}, policy: standard, allowed: true},
		{name: "user bot friend dm", identity: userBot, target: Target{SpaceID: "space-a", ChannelID: "friend-a", ChannelType: common.ChannelTypePerson.Uint8()}, policy: standard, allowed: true},
		{name: "user bot stranger dm", identity: userBot, target: Target{SpaceID: "space-a", ChannelID: "stranger-a", ChannelType: common.ChannelTypePerson.Uint8()}, policy: standard},
		{name: "system notification dm to active member", identity: userBot, target: Target{SpaceID: "space-a", ChannelID: "stranger-a", ChannelType: common.ChannelTypePerson.Uint8()}, policy: systemNotification, allowed: true},
		{name: "system notification outside verified space", identity: userBot, target: Target{SpaceID: "space-a", ChannelID: "outside-b", ChannelType: common.ChannelTypePerson.Uint8()}, policy: systemNotification},
		{name: "disabled space", identity: userBot, target: Target{SpaceID: "space-disabled", ChannelID: "disabled-member", ChannelType: common.ChannelTypePerson.Uint8()}, policy: systemNotification},
		{name: "platform app dm", identity: platformApp, target: Target{SpaceID: "space-a", ChannelID: "member-a", ChannelType: common.ChannelTypePerson.Uint8()}, policy: standard, allowed: true},
		{name: "platform app dm without friend", identity: platformApp, target: Target{SpaceID: "space-a", ChannelID: "stranger-a", ChannelType: common.ChannelTypePerson.Uint8()}, policy: standard},
		{name: "space app dm", identity: spaceApp, target: Target{SpaceID: "space-a", ChannelID: "member-a", ChannelType: common.ChannelTypePerson.Uint8()}, policy: standard, allowed: true},
		{name: "space app wrong space", identity: spaceApp, target: Target{SpaceID: "space-b", ChannelID: "outside-b", ChannelType: common.ChannelTypePerson.Uint8()}, policy: standard},
		{name: "app bot unknown scope", identity: &botidentity.Identity{UID: "app-platform", Kind: botidentity.KindAppBot, AppScope: "unknown"}, target: Target{SpaceID: "space-a", ChannelID: "member-a", ChannelType: common.ChannelTypePerson.Uint8()}, policy: standard},
		{name: "user bot missing sender membership", identity: &botidentity.Identity{UID: "bot-missing", Kind: botidentity.KindUserBot, CreatorUID: "owner-a"}, target: Target{SpaceID: "space-a", ChannelID: "owner-a", ChannelType: common.ChannelTypePerson.Uint8()}, policy: standard},
		{name: "user bot invalid space policy", identity: userBot, target: Target{SpaceID: "space-a", ChannelID: "owner-a", ChannelType: common.ChannelTypePerson.Uint8()}, policy: AuthorizationPolicy{SpacePolicy: "invalid", GroupPolicy: GroupPolicyMemberRequired}},
		{name: "unsupported identity kind", identity: &botidentity.Identity{UID: "bot-user", Kind: "unknown"}, target: Target{SpaceID: "space-a", ChannelID: "owner-a", ChannelType: common.ChannelTypePerson.Uint8()}, policy: standard},
		{name: "app bot group denied", identity: platformApp, target: Target{SpaceID: "space-a", ChannelID: "group-ok", ChannelType: common.ChannelTypeGroup.Uint8()}, policy: standard},
		{name: "user bot group member", identity: userBot, target: Target{SpaceID: "space-a", ChannelID: "group-ok", ChannelType: common.ChannelTypeGroup.Uint8()}, policy: standard, allowed: true},
		{name: "user bot group non-member", identity: userBot, target: Target{SpaceID: "space-a", ChannelID: "group-no-member", ChannelType: common.ChannelTypeGroup.Uint8()}, policy: standard},
		{name: "member exempt group", identity: userBot, target: Target{SpaceID: "space-a", ChannelID: "group-no-member", ChannelType: common.ChannelTypeGroup.Uint8()}, policy: memberExempt, allowed: true},
		{name: "member exempt normal member group", identity: userBot, target: Target{SpaceID: "space-a", ChannelID: "group-ok", ChannelType: common.ChannelTypeGroup.Uint8()}, policy: memberExempt, allowed: true},
		{name: "member exempt honors explicit blacklist", identity: userBot, target: Target{SpaceID: "space-a", ChannelID: "group-blacklisted", ChannelType: common.ChannelTypeGroup.Uint8()}, policy: memberExempt},
		{name: "blacklisted bot member", identity: userBot, target: Target{SpaceID: "space-a", ChannelID: "group-blacklisted", ChannelType: common.ChannelTypeGroup.Uint8()}, policy: standard},
		{name: "external bot member", identity: userBot, target: Target{SpaceID: "space-a", ChannelID: "group-external", ChannelType: common.ChannelTypeGroup.Uint8()}, policy: standard},
		{name: "deleted bot member", identity: userBot, target: Target{SpaceID: "space-a", ChannelID: "group-deleted-member", ChannelType: common.ChannelTypeGroup.Uint8()}, policy: standard},
		{name: "disabled group", identity: userBot, target: Target{SpaceID: "space-a", ChannelID: "group-disabled", ChannelType: common.ChannelTypeGroup.Uint8()}, policy: memberExempt},
		{name: "disbanded group", identity: userBot, target: Target{SpaceID: "space-a", ChannelID: "group-disbanded", ChannelType: common.ChannelTypeGroup.Uint8()}, policy: memberExempt},
		{name: "wrong space group", identity: userBot, target: Target{SpaceID: "space-a", ChannelID: "group-wrong-space", ChannelType: common.ChannelTypeGroup.Uint8()}, policy: memberExempt},
		{name: "missing group", identity: userBot, target: Target{SpaceID: "space-a", ChannelID: "group-missing", ChannelType: common.ChannelTypeGroup.Uint8()}, policy: memberExempt},
		{name: "invalid group policy", identity: userBot, target: Target{SpaceID: "space-a", ChannelID: "group-ok", ChannelType: common.ChannelTypeGroup.Uint8()}, policy: AuthorizationPolicy{SpacePolicy: SpacePolicyMembership, GroupPolicy: "invalid"}},
		{name: "active thread", identity: userBot, target: Target{SpaceID: "space-a", ChannelID: thread.BuildChannelID("group-ok", "123456789012345"), ChannelType: common.ChannelTypeCommunityTopic.Uint8()}, policy: standard, allowed: true},
		{name: "member exempt active thread", identity: userBot, target: Target{SpaceID: "space-a", ChannelID: thread.BuildChannelID("group-no-member", "123456789012348"), ChannelType: common.ChannelTypeCommunityTopic.Uint8()}, policy: memberExempt, allowed: true},
		{name: "archived thread", identity: userBot, target: Target{SpaceID: "space-a", ChannelID: thread.BuildChannelID("group-ok", "123456789012346"), ChannelType: common.ChannelTypeCommunityTopic.Uint8()}, policy: standard},
		{name: "deleted thread", identity: userBot, target: Target{SpaceID: "space-a", ChannelID: thread.BuildChannelID("group-ok", "123456789012347"), ChannelType: common.ChannelTypeCommunityTopic.Uint8()}, policy: standard},
		{name: "malformed thread", identity: userBot, target: Target{SpaceID: "space-a", ChannelID: "bad-thread", ChannelType: common.ChannelTypeCommunityTopic.Uint8()}, policy: standard},
		{name: "missing thread", identity: userBot, target: Target{SpaceID: "space-a", ChannelID: thread.BuildChannelID("group-ok", "999999999999999"), ChannelType: common.ChannelTypeCommunityTopic.Uint8()}, policy: standard},
		{name: "unsupported channel", identity: userBot, target: Target{SpaceID: "space-a", ChannelID: "x", ChannelType: common.ChannelTypeInfo.Uint8()}, policy: standard},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := authorizer.Authorize(context.Background(), tc.identity, tc.target, tc.policy)
			if tc.allowed {
				require.NoError(t, err)
				return
			}
			assert.ErrorIs(t, err, ErrTargetDenied)
		})
	}
}

func TestDBAuthorizerMemberExemptDoesNotMutateMembership(t *testing.T) {
	session := newAuthorizerDB(t)
	authorizer := NewDBAuthorizer(session)
	identity := &botidentity.Identity{UID: "bot-user", Kind: botidentity.KindUserBot, CreatorUID: "owner-a"}
	target := Target{SpaceID: "space-a", ChannelID: "group-no-member", ChannelType: common.ChannelTypeGroup.Uint8()}
	policy := AuthorizationPolicy{SpacePolicy: SpacePolicyMembership, GroupPolicy: GroupPolicyMemberExempt}

	var before int
	require.NoError(t, session.SelectBySql("SELECT COUNT(*) FROM group_member WHERE group_no=? AND uid=?", target.ChannelID, identity.UID).LoadOne(&before))
	require.NoError(t, authorizer.Authorize(context.Background(), identity, target, policy))
	var after int
	require.NoError(t, session.SelectBySql("SELECT COUNT(*) FROM group_member WHERE group_no=? AND uid=?", target.ChannelID, identity.UID).LoadOne(&after))
	assert.Zero(t, before)
	assert.Equal(t, before, after)
}

func TestDBAuthorizerFailsClosedOnDatabaseError(t *testing.T) {
	conn, err := dbr.Open("sqlite3", ":memory:", nil)
	require.NoError(t, err)
	session := conn.NewSession(nil)
	require.NoError(t, conn.Close())
	authorizer := NewDBAuthorizer(session)
	identity := &botidentity.Identity{UID: "bot-user", Kind: botidentity.KindUserBot}
	err = authorizer.Authorize(context.Background(), identity, validTarget(), AuthorizationPolicy{})
	assert.ErrorIs(t, err, ErrTargetDenied)
}

func TestDBAuthorizerRejectsUnavailableDependenciesAndCancellation(t *testing.T) {
	session := newAuthorizerDB(t)
	identity := &botidentity.Identity{UID: "bot-user", Kind: botidentity.KindUserBot}
	policy := AuthorizationPolicy{SpacePolicy: SpacePolicyMembership, GroupPolicy: GroupPolicyMemberRequired}

	assert.ErrorIs(t, NewDBAuthorizer(nil).Authorize(context.Background(), identity, validTarget(), policy), ErrTargetDenied)
	assert.ErrorIs(t, NewDBAuthorizer(session).Authorize(context.Background(), nil, validTarget(), policy), ErrTargetDenied)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	assert.ErrorIs(t, NewDBAuthorizer(session).Authorize(ctx, identity, validTarget(), policy), ErrTargetDenied)
}

func TestDBAuthorizerFailsClosedAtEveryStorageStage(t *testing.T) {
	identity := &botidentity.Identity{UID: "bot-user", Kind: botidentity.KindUserBot, CreatorUID: "owner-a"}
	standard := AuthorizationPolicy{SpacePolicy: SpacePolicyMembership, GroupPolicy: GroupPolicyMemberRequired}
	cases := []struct {
		name   string
		drop   string
		id     *botidentity.Identity
		target Target
		policy AuthorizationPolicy
	}{
		{name: "recipient membership", drop: "space_member", id: identity, target: Target{SpaceID: "space-a", ChannelID: "owner-a", ChannelType: common.ChannelTypePerson.Uint8()}, policy: standard},
		{name: "friend relation", drop: "friend", id: &botidentity.Identity{UID: "app-platform", Kind: botidentity.KindAppBot, AppScope: botidentity.ScopePlatform}, target: Target{SpaceID: "space-a", ChannelID: "member-a", ChannelType: common.ChannelTypePerson.Uint8()}, policy: standard},
		{name: "group", drop: "`group`", id: identity, target: Target{SpaceID: "space-a", ChannelID: "group-ok", ChannelType: common.ChannelTypeGroup.Uint8()}, policy: standard},
		{name: "group membership", drop: "group_member", id: identity, target: Target{SpaceID: "space-a", ChannelID: "group-ok", ChannelType: common.ChannelTypeGroup.Uint8()}, policy: standard},
		{name: "thread", drop: "thread", id: identity, target: Target{SpaceID: "space-a", ChannelID: thread.BuildChannelID("group-ok", "123456789012345"), ChannelType: common.ChannelTypeCommunityTopic.Uint8()}, policy: standard},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			session := newAuthorizerDB(t)
			_, err := session.Exec("DROP TABLE " + tc.drop)
			require.NoError(t, err)
			err = NewDBAuthorizer(session).Authorize(context.Background(), tc.id, tc.target, tc.policy)
			assert.ErrorIs(t, err, ErrTargetDenied)
		})
	}
}
