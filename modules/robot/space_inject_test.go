// Package robot · YUJ-644 / Mininglamp-OSS#33 / YUJ-660 unit tests for
// PERSONAL DM payload.space_id authoritative injection on the legacy
// /v1/robot/... route.
package robot

import (
	"errors"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/gocraft/dbr/v2"
	"github.com/stretchr/testify/assert"
)

type fakeRobotSpaceQuerier struct {
	calls   []string
	spaceID string
	err     error
}

func (f *fakeRobotSpaceQuerier) querySpaceIDByRobotID(robotID string) (string, error) {
	f.calls = append(f.calls, robotID)
	return f.spaceID, f.err
}

// newTestRobot constructs a minimal *Robot with logger + injected querier
// suitable for unit-testing enrichBotPayloadWithSpaceID without DB.
func newTestRobot(q robotSpaceQuerier) *Robot {
	return &Robot{Log: log.NewTLog("Robot-test"), spaceQuerier: q}
}

func TestEnrichBotPayloadWithSpaceID_DBSpaceOverridesClient(t *testing.T) {
	q := &fakeRobotSpaceQuerier{spaceID: "spaceAuth"}
	rb := newTestRobot(q)
	payload := map[string]interface{}{"content": "hi", "space_id": "spaceForged"}
	got := rb.enrichBotPayloadWithSpaceID("user_bot_1", payload)
	assert.Equal(t, "spaceAuth", got["space_id"], "PERSONAL 必须用服务端权威 SpaceID 覆盖客户端伪造值")
	assert.Equal(t, []string{"user_bot_1"}, q.calls)
}

func TestEnrichBotPayloadWithSpaceID_NilPayloadInitialized(t *testing.T) {
	q := &fakeRobotSpaceQuerier{spaceID: "spaceAuth"}
	rb := newTestRobot(q)
	got := rb.enrichBotPayloadWithSpaceID("user_bot_1", nil)
	assert.NotNil(t, got)
	assert.Equal(t, "spaceAuth", got["space_id"])
}

func TestEnrichBotPayloadWithSpaceID_ErrNotFound_NoDBWarn_NoSpaceInjected(t *testing.T) {
	// YUJ-660 Medium-2：dbr.ErrNotFound 是合法状态，不报 DB 错误。
	q := &fakeRobotSpaceQuerier{err: dbr.ErrNotFound}
	rb := newTestRobot(q)
	payload := map[string]interface{}{"content": "hi"}
	got := rb.enrichBotPayloadWithSpaceID("orphan_bot", payload)
	_, ok := got["space_id"]
	assert.False(t, ok, "ErrNotFound 时不应注入 space_id")
	assert.Equal(t, []string{"orphan_bot"}, q.calls)
}

func TestEnrichBotPayloadWithSpaceID_RealDBError_PreservesPayload(t *testing.T) {
	// 真实 DB 错误：保持 payload 原样，不阻断发送。
	q := &fakeRobotSpaceQuerier{err: errors.New("connection refused")}
	rb := newTestRobot(q)
	payload := map[string]interface{}{"content": "hi", "space_id": "spaceX"}
	got := rb.enrichBotPayloadWithSpaceID("bot_with_db_error", payload)
	assert.Equal(t, "spaceX", got["space_id"], "真实 DB 错误下 payload 不动")
}
