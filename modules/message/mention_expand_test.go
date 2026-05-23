// Composer-level unit test for (*Message).fetchBotMemberUIDs — the
// modules/message-side wrapper that wires group.GetMembers +
// robot.ExistRobot into the pkg/mentionrewrite.ExpandAisToBotUIDs
// callback shape.
//
// Why this test exists
// ====================
// Mininglamp-OSS/octo-server#144 PR#145 review (yujiawei 2026-05-23):
// the composer's per-row best-effort policy — "if ExistRobot fails
// for one member, treat that member as not-a-bot and KEEP scanning"
// — was previously only asserted in prose comments. This test pins
// it as code. The alternative (abort the whole expansion on a
// single corrupt robot row) would silently disable `@所有 AI`
// expansion for an entire group whenever ONE robot DB row was
// transiently bad — the exact regression #144 set out to prevent.
package message

import (
	"errors"
	"testing"

	"github.com/Mininglamp-OSS/octo-server/modules/group"
	"github.com/Mininglamp-OSS/octo-server/modules/robot"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/stretchr/testify/assert"
)

// fakeExpandRobotService is a robot.IService stub that drives the
// per-row ExistRobot result from a UID-keyed table. UIDs absent from
// the table return (false, nil). UIDs whose value is a non-nil
// errResult return that error verbatim.
type fakeExpandRobotService struct {
	robot.IService

	exist map[string]bool
	errs  map[string]error
}

func (f *fakeExpandRobotService) ExistRobot(uid string) (bool, error) {
	if err, ok := f.errs[uid]; ok && err != nil {
		return false, err
	}
	return f.exist[uid], nil
}

// fakeExpandGroupService returns a static member roster for any
// groupNo. Only GetMembers is exercised by fetchBotMemberUIDs.
type fakeExpandGroupService struct {
	group.IService
	members []*group.MemberResp
	err     error
}

func (f *fakeExpandGroupService) GetMembers(groupNo string) ([]*group.MemberResp, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.members, nil
}

func newExpandTestMessage(robotSvc robot.IService, groupSvc group.IService) *Message {
	return &Message{
		Log:          log.NewTLog("expand-test"),
		robotService: robotSvc,
		groupService: groupSvc,
	}
}

// TestFetchBotMemberUIDs_PerRowExistRobotFailureDegrades is the
// per-row best-effort policy clause. The roster has three members:
// bot_a (ExistRobot → true), bot_b (ExistRobot → transient error),
// bot_c (ExistRobot → true). bot_b MUST be skipped (treated as
// not-a-bot) and the scan MUST continue, so the returned bot UID
// set is {bot_a, bot_c}. The whole expansion MUST NOT abort, and
// fetchBotMemberUIDs MUST NOT return an error (the composer's
// contract is "no error" so ExpandAisToBotUIDs treats the result
// as authoritative — see pkg/mentionrewrite/expand_ais.go clause 5).
func TestFetchBotMemberUIDs_PerRowExistRobotFailureDegrades(t *testing.T) {
	gs := &fakeExpandGroupService{
		members: []*group.MemberResp{
			{UID: "bot_a"},
			{UID: "bot_b"},
			{UID: "bot_c"},
			{UID: "u_human"},
		},
	}
	rs := &fakeExpandRobotService{
		exist: map[string]bool{
			"bot_a":   true,
			"bot_c":   true,
			"u_human": false,
		},
		errs: map[string]error{
			"bot_b": errors.New("transient robot row corrupt"),
		},
	}
	m := newExpandTestMessage(rs, gs)

	got, err := m.fetchBotMemberUIDs("group_1")
	assert.NoError(t, err, "per-row ExistRobot failure must NOT propagate as a composer error")
	assert.ElementsMatch(t, []string{"bot_a", "bot_c"}, got,
		"the failing row (bot_b) must be skipped and the scan must continue past it")
}

// TestFetchBotMemberUIDs_GroupGetMembersErrorPropagates is the sister
// clause: a GetMembers error IS propagated (ExpandAisToBotUIDs then
// short-circuits to a no-op per clause 5). This is the
// "lookup failed entirely" path, distinct from the per-row failure
// above.
func TestFetchBotMemberUIDs_GroupGetMembersErrorPropagates(t *testing.T) {
	gs := &fakeExpandGroupService{err: errors.New("group db down")}
	rs := &fakeExpandRobotService{}
	m := newExpandTestMessage(rs, gs)

	got, err := m.fetchBotMemberUIDs("group_1")
	assert.Error(t, err, "a group-level lookup failure must propagate so ExpandAisToBotUIDs can no-op")
	assert.Nil(t, got)
}

// TestFetchBotMemberUIDs_NilAndEmptyMembersSafe pins the
// "no panic on nil/empty roster" clause. fetchBotMemberUIDs is
// invoked inside an HTTP handler critical path — a panic on an
// empty group would 500 the send.
func TestFetchBotMemberUIDs_NilAndEmptyMembersSafe(t *testing.T) {
	rs := &fakeExpandRobotService{}
	for _, tc := range []struct {
		name    string
		members []*group.MemberResp
	}{
		{"nil roster", nil},
		{"empty roster", []*group.MemberResp{}},
		{"nil member in roster", []*group.MemberResp{nil, {UID: ""}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gs := &fakeExpandGroupService{members: tc.members}
			m := newExpandTestMessage(rs, gs)
			got, err := m.fetchBotMemberUIDs("group_1")
			assert.NoError(t, err)
			assert.Empty(t, got)
		})
	}
}
