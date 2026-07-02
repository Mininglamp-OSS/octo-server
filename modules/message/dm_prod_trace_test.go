//go:build integration

package message

// Server-side reproductions of the three conversation cross-Space paths that a
// production incident + a client-side SpaceFilter diagnostic surfaced. All data
// here is SYNTHETIC — no real uids, space ids, or names. The tests drive the
// REAL /v1/conversation/sync handler to characterise each path so they can be
// told apart from an actual request without needing a production capture.
//
// Three Spaces from the shared harness:
//
//	reproSpaceDefault  the user's DEFAULT Space (earliest joined)
//	reproSpaceB        a non-default Space the user is viewing
//	reproSpaceC        a third non-default Space (isolation contrast)
//
// None of these tests write dm_space_presence rows, so they characterise the
// pre-existing behaviour independent of the #484 fix.
//
//	go test -tags=integration ./modules/message/ -run TestRepro484_ProdTrace -v

import (
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/stretchr/testify/assert"
)

const (
	ppHumanA = "pp_human_a"
	ppHumanB = "pp_human_b"
	ppHumanC = "pp_human_c"
	ppRobot  = "pp_robot_untagged" // seeded user.robot=1, messages untagged, member of no Space
)

// TestRepro484_ProdTrace_MissingSpaceID_LeaksCrossSpaceDMs reproduces the FIRST
// path: the request carried NO space context. SpaceMiddleware fails open
// (pkg/space/middleware.go:108-115 → c.Next() without setting space_id), so the
// handler's `if spaceID != ""` guard skips FilterConversationsBySpace entirely
// (api_conversation.go:889) and returns the user's DMs from EVERY Space.
//
// Discriminator: with identical data, the only difference between the two calls
// is the presence of the space header. The DMs a non-default query correctly
// drops are returned when the header is omitted — including an untagged robot DM.
func TestRepro484_ProdTrace_MissingSpaceID_LeaksCrossSpaceDMs(t *testing.T) {
	s, ctx := reproSetup(t)
	// A robot (user.robot=1) that is a member of NO Space and whose messages carry
	// no space_id → any real filter MUST drop it under a non-default Space.
	reproSeedBotUser(t, ctx, ppRobot)

	// Two human DMs tagged only with the DEFAULT Space, one untagged robot DM, and
	// one DM that legitimately belongs to spaceB.
	reproIMConvs = []*config.SyncUserConversationResp{
		reproDMConvFor(ppHumanA, "default-A", reproSpaceDefault),
		reproDMConvFor(ppHumanB, "default-B", reproSpaceDefault),
		reproDMConvFor(ppRobot, "robot-untagged", ""),
		reproDMConvFor(ppHumanC, "spaceB-real", reproSpaceB),
	}

	// (1) No space context → filter skipped → every DM returned.
	noSpace := reproCallConvSyncNoSpace(t, s)
	assert.True(t, reproContains(noSpace, ppHumanA), "no space_id: a default-only DM leaks")
	assert.True(t, reproContains(noSpace, ppHumanB), "no space_id: a default-only DM leaks")
	assert.True(t, reproContains(noSpace, ppRobot),
		"no space_id: the untagged robot DM leaks too")
	assert.True(t, reproContains(noSpace, ppHumanC), "no space_id: the spaceB DM is also present")

	// (2) Same data, now WITH X-Space-ID=spaceB → filter runs → cross-Space dropped.
	inB := reproCallConvSync(t, s, reproSpaceB)
	assert.True(t, reproContains(inB, ppHumanC), "spaceB query keeps the DM that belongs to spaceB")
	assert.False(t, reproContains(inB, ppHumanA), "spaceB query drops the default-only DM")
	assert.False(t, reproContains(inB, ppHumanB), "spaceB query drops the default-only DM")
	assert.False(t, reproContains(inB, ppRobot), "spaceB query drops the untagged robot DM")

	// The delta between (1) and (2) — identical data, only the space header
	// differs — isolates "Space filter not executed" as the leak's direct cause.
}

// TestRepro484_ProdTrace_DefaultSpaceCatchAll_ShowsAllDMs reproduces the SECOND
// candidate: the request carried the user's DEFAULT Space. decideConvKeepInSpace's
// catch-all (space_filter.go:305-309) returns true for EVERY bare non-bot DM when
// filterSpaceID == defaultSpaceID — so the default Space lists DMs that belong
// only to OTHER Spaces.
//
// TELL distinguishing it from the missing-space_id path: the catch-all still runs
// the bot sub-check, so a robot DM that is NOT a member of the default Space is
// HIDDEN. A robot that leaked in production therefore points at the
// missing-space_id path, not this one. Capturing one real request decides between
// them.
func TestRepro484_ProdTrace_DefaultSpaceCatchAll_ShowsAllDMs(t *testing.T) {
	s, ctx := reproSetup(t)
	reproSeedBotUser(t, ctx, ppRobot) // robot, member of NO Space here

	reproIMConvs = []*config.SyncUserConversationResp{
		reproDMConvFor(ppHumanA, "only-spaceB", reproSpaceB), // human, msgs only in spaceB
		reproDMConvFor(ppHumanB, "only-spaceC", reproSpaceC), // human, msgs only in spaceC
		reproDMConvFor(ppRobot, "robot-untagged", ""),        // robot, untagged
	}

	// Query the DEFAULT Space → catch-all lists cross-Space HUMAN DMs...
	def := reproCallConvSync(t, s, reproSpaceDefault)
	assert.True(t, reproContains(def, ppHumanA),
		"catch-all: a spaceB-only DM shows in the default Space")
	assert.True(t, reproContains(def, ppHumanB),
		"catch-all: a spaceC-only DM shows in the default Space")
	// ...but the catch-all bot sub-check HIDES a robot that is not a default member.
	assert.False(t, reproContains(def, ppRobot),
		"catch-all hides a non-member robot — so this path alone can't explain a leaked robot")

	// Contrast: a non-default Space isolates correctly (no catch-all).
	spaceC := reproCallConvSync(t, s, reproSpaceC)
	assert.True(t, reproContains(spaceC, ppHumanB), "spaceC query keeps the spaceC-only DM")
	assert.False(t, reproContains(spaceC, ppHumanA), "spaceC query drops the spaceB-only DM")
}

// NOTE: the former TestRepro484_ProdTrace_SpacelessGroupShownInEverySpace
// characterized the pre-fix bug (an empty-space_id group visible in every
// Space). That path is now FIXED on this branch (spaceless groups/topics are
// attributed to the default Space only), so the reproduction is superseded by
// TestConvSpaceCatchall_SpacelessGroupAndTopicOnlyDefault, which asserts the
// fixed behavior end-to-end.
