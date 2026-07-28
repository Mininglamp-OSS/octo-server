package message

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakePersonAccessDeps 只实现 personAccessDeps 的三个方法，让 DM 门的判定顺序
// 可以脱库断言（真实依赖是 user.IService，起它要 MySQL + Redis）。
type fakePersonAccessDeps struct {
	friends      map[string]bool // "uid→toUID" 有向好友边
	robots       map[string]string
	spaceMembers map[string]bool // "spaceID|uid1|uid2"

	friendErr error
	robotErr  error
	spaceErr  error

	friendCalls int
	robotCalls  int
	spaceCalls  int
}

func (f *fakePersonAccessDeps) IsFriend(uid string, toUID string) (bool, error) {
	f.friendCalls++
	if f.friendErr != nil {
		return false, f.friendErr
	}
	return f.friends[uid+"→"+toUID], nil
}

func (f *fakePersonAccessDeps) QueryPeerRobotInfo(peerUID string) (bool, string, error) {
	f.robotCalls++
	if f.robotErr != nil {
		return false, "", f.robotErr
	}
	creator, isRobot := f.robots[peerUID]
	return isRobot, creator, nil
}

func (f *fakePersonAccessDeps) AreSpaceMembers(spaceID, uid1, uid2 string) (bool, error) {
	f.spaceCalls++
	if f.spaceErr != nil {
		return false, f.spaceErr
	}
	return f.spaceMembers[spaceID+"|"+uid1+"|"+uid2], nil
}

// TestPersonChannelAccessAllowed 锁定 DM 门的判定口径（与
// modules/user/api_pinned.go::validateChannelAccess 对齐）：
// self → 好友 → bot（本人的放行/他人的仍需好友）→ 同 Space 真人，都不命中则拒绝。
func TestPersonChannelAccessAllowed(t *testing.T) {
	const me = "u_me"
	const peer = "u_peer"

	tests := []struct {
		name        string
		deps        *fakePersonAccessDeps
		peer        string
		spaceID     string
		wantAllowed bool
		wantErr     bool
	}{
		{
			name:        "自己和自己的备忘频道直接放行",
			deps:        &fakePersonAccessDeps{},
			peer:        me,
			wantAllowed: true,
		},
		{
			name:        "空 peer 拒绝",
			deps:        &fakePersonAccessDeps{},
			peer:        "",
			wantAllowed: false,
		},
		{
			name:        "好友放行（个人空间部署口径）",
			deps:        &fakePersonAccessDeps{friends: map[string]bool{me + "→" + peer: true}},
			peer:        peer,
			wantAllowed: true,
		},
		{
			name: "非好友但同 Space 同事放行（企业通讯录部署，friend 表为空）",
			deps: &fakePersonAccessDeps{
				spaceMembers: map[string]bool{"spc1|" + me + "|" + peer: true},
			},
			peer:        peer,
			spaceID:     "spc1",
			wantAllowed: true,
		},
		{
			name: "非好友且不同 Space 拒绝",
			deps: &fakePersonAccessDeps{
				spaceMembers: map[string]bool{"spc2|" + me + "|" + peer: true},
			},
			peer:        peer,
			spaceID:     "spc1",
			wantAllowed: false,
		},
		{
			// D1：请求没带 space_id 时不解析默认 Space，退回好友口径。
			name: "请求无 space_id → 不放行（不做默认 Space 兜底）",
			deps: &fakePersonAccessDeps{
				spaceMembers: map[string]bool{"spc1|" + me + "|" + peer: true},
			},
			peer:        peer,
			spaceID:     "",
			wantAllowed: false,
		},
		{
			name: "本人创建的 bot 放行，且不需要好友/Space",
			deps: &fakePersonAccessDeps{robots: map[string]string{"bot1": me}},
			peer: "bot1",
			// 故意不给 Space 成员关系：bot 没有 space_member 行。
			spaceID:     "spc1",
			wantAllowed: true,
		},
		{
			name: "他人创建的 bot 非好友 → 拒绝（即使同 Space 也不放行）",
			deps: &fakePersonAccessDeps{
				robots:       map[string]string{"bot1": "u_other"},
				spaceMembers: map[string]bool{"spc1|" + me + "|bot1": true},
			},
			peer:        "bot1",
			spaceID:     "spc1",
			wantAllowed: false,
		},
		{
			name: "他人创建的 bot 是好友 → 放行",
			deps: &fakePersonAccessDeps{
				friends: map[string]bool{me + "→bot1": true},
				robots:  map[string]string{"bot1": "u_other"},
			},
			peer:        "bot1",
			wantAllowed: true,
		},
		{
			// 已停用 bot：user.robot=1 但 robot.status!=1 → creatorUID 为空，
			// 即便是自己创建的也落回好友口径（db_pinned.go QueryPeerRobotInfo 注释）。
			name:        "已停用的 bot（creator 为空）→ 拒绝",
			deps:        &fakePersonAccessDeps{robots: map[string]string{"bot1": ""}},
			peer:        "bot1",
			spaceID:     "spc1",
			wantAllowed: false,
		},
		{
			name:        "好友查询报错 fail-closed",
			deps:        &fakePersonAccessDeps{friendErr: errors.New("db down")},
			peer:        peer,
			spaceID:     "spc1",
			wantAllowed: false,
			wantErr:     true,
		},
		{
			name:        "bot 查询报错 fail-closed",
			deps:        &fakePersonAccessDeps{robotErr: errors.New("db down")},
			peer:        peer,
			spaceID:     "spc1",
			wantAllowed: false,
			wantErr:     true,
		},
		{
			name:        "Space 查询报错 fail-closed",
			deps:        &fakePersonAccessDeps{spaceErr: errors.New("db down")},
			peer:        peer,
			spaceID:     "spc1",
			wantAllowed: false,
			wantErr:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			allowed, err := personChannelAccessAllowed(tt.deps, me, tt.peer, tt.spaceID)
			if tt.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tt.wantAllowed, allowed)
		})
	}
}

// TestPersonChannelAccessAllowedSelfSkipsLookups 备忘频道不触发任何查询。
func TestPersonChannelAccessAllowedSelfSkipsLookups(t *testing.T) {
	deps := &fakePersonAccessDeps{}
	allowed, err := personChannelAccessAllowed(deps, "u_me", "u_me", "spc1")
	require.NoError(t, err)
	assert.True(t, allowed)
	assert.Zero(t, deps.friendCalls)
	assert.Zero(t, deps.robotCalls)
	assert.Zero(t, deps.spaceCalls)
}

// TestPersonChannelAccessAllowedFriendShortCircuits 好友命中后不再查 bot/Space，
// 保证个人空间部署下仍然只有一次查询（与改动前的开销一致）。
func TestPersonChannelAccessAllowedFriendShortCircuits(t *testing.T) {
	deps := &fakePersonAccessDeps{friends: map[string]bool{"u_me→u_peer": true}}
	allowed, err := personChannelAccessAllowed(deps, "u_me", "u_peer", "spc1")
	require.NoError(t, err)
	assert.True(t, allowed)
	assert.Equal(t, 1, deps.friendCalls)
	assert.Zero(t, deps.robotCalls)
	assert.Zero(t, deps.spaceCalls)
}

// TestPersonChannelAccessAllowedNoSpaceSkipsSpaceLookup D1 的开销侧断言：
// 请求没带 space_id 时不应发生 Space 查询。
func TestPersonChannelAccessAllowedNoSpaceSkipsSpaceLookup(t *testing.T) {
	deps := &fakePersonAccessDeps{}
	allowed, err := personChannelAccessAllowed(deps, "u_me", "u_peer", "")
	require.NoError(t, err)
	assert.False(t, allowed)
	assert.Equal(t, 1, deps.friendCalls)
	assert.Equal(t, 1, deps.robotCalls)
	assert.Zero(t, deps.spaceCalls)
}
