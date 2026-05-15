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
	// Mininglamp-OSS/octo-server#36 — full ordered list. When non-nil, takes
	// precedence over `spaceID` for `querySpaceIDsByRobotID`.
	rows []string
}

func (f *fakeRobotSpaceQuerier) querySpaceIDByRobotID(robotID string) (string, error) {
	f.calls = append(f.calls, robotID)
	return f.spaceID, f.err
}

func (f *fakeRobotSpaceQuerier) querySpaceIDsByRobotID(robotID string) (string, []string, error) {
	primary, err := f.querySpaceIDByRobotID(robotID)
	if err != nil {
		return "", nil, err
	}
	if f.rows != nil {
		if len(f.rows) == 0 {
			return "", nil, dbr.ErrNotFound
		}
		return f.rows[0], f.rows, nil
	}
	if primary == "" {
		return "", nil, dbr.ErrNotFound
	}
	return primary, []string{primary}, nil
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

func TestEnrichBotPayloadWithSpaceID_ErrNotFound_StripsClientSpaceID(t *testing.T) {
	// YUJ-660 R3 Finding A：孤儿 Bot (ErrNotFound) + forged client space_id →
	// strip。fail-closed 语义：服务端无可信 SpaceID 时绝不允许客户端注入信号。
	q := &fakeRobotSpaceQuerier{err: dbr.ErrNotFound}
	rb := newTestRobot(q)
	payload := map[string]interface{}{"content": "hi", "space_id": "spaceForged"}
	got := rb.enrichBotPayloadWithSpaceID("orphan_bot", payload)
	_, ok := got["space_id"]
	assert.False(t, ok, "ErrNotFound + forged client space_id 必须 strip")
	assert.Equal(t, []string{"orphan_bot"}, q.calls)
}

func TestEnrichBotPayloadWithSpaceID_OrphanBot_NoForgedClient_NoSpaceInjected(t *testing.T) {
	// 孤儿 Bot + client 未上送：不注入 space_id，发 enrich_payload_space_id_empty warn。
	q := &fakeRobotSpaceQuerier{err: dbr.ErrNotFound}
	rb := newTestRobot(q)
	payload := map[string]interface{}{"content": "hi"}
	got := rb.enrichBotPayloadWithSpaceID("orphan_bot", payload)
	_, ok := got["space_id"]
	assert.False(t, ok, "孤儿 Bot 时不应注入 space_id")
}

func TestEnrichBotPayloadWithSpaceID_RealDBError_StripsClientSpaceID(t *testing.T) {
	// YUJ-660 R3 Finding A — INVERTED from prior _PreservesPayload behavior.
	// 真实 DB 错误（network blip / failover）时也走 strip 路径，不能保留 client
	// payload.space_id。攻击者可以构造 DB 错误 + forged payload 跨 Space 派发，
	// 必须 fail-closed strip。
	q := &fakeRobotSpaceQuerier{err: errors.New("connection refused")}
	rb := newTestRobot(q)
	payload := map[string]interface{}{"content": "hi", "space_id": "spaceVictim"}
	got := rb.enrichBotPayloadWithSpaceID("bot_with_db_error", payload)
	_, ok := got["space_id"]
	assert.False(t, ok,
		"DB 错误 + forged client space_id：robot 必须 strip，不能 preserve（fail-closed）")
}
