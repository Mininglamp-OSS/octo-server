package user

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestValidatePinnedSortItems(t *testing.T) {
	existing := map[pinnedKey]struct{}{
		{ChannelID: "c1", ChannelType: 1}: {},
		{ChannelID: "c2", ChannelType: 2}: {},
		{ChannelID: "c3", ChannelType: 2}: {},
	}

	tests := []struct {
		name    string
		items   []PinnedSortItem
		wantErr string
	}{
		{
			name: "valid subset",
			items: []PinnedSortItem{
				{ChannelID: "c2", ChannelType: 2},
				{ChannelID: "c1", ChannelType: 1},
			},
		},
		{
			name: "valid full set",
			items: []PinnedSortItem{
				{ChannelID: "c1", ChannelType: 1},
				{ChannelID: "c2", ChannelType: 2},
				{ChannelID: "c3", ChannelType: 2},
			},
		},
		{
			name: "unknown channel rejected",
			items: []PinnedSortItem{
				{ChannelID: "c1", ChannelType: 1},
				{ChannelID: "ghost", ChannelType: 2},
			},
			wantErr: "未置顶",
		},
		{
			name: "duplicate key in request rejected",
			items: []PinnedSortItem{
				{ChannelID: "c1", ChannelType: 1},
				{ChannelID: "c1", ChannelType: 1},
			},
			wantErr: "重复",
		},
		{
			name:    "empty items rejected",
			items:   nil,
			wantErr: "不能为空",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validatePinnedSortItems(tc.items, existing)
			if tc.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			assert.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

func TestGroupMemberChecker_RegisterAndGet(t *testing.T) {
	// reset global state
	setGroupMemberChecker(nil)

	assert.Nil(t, getGroupMemberChecker())

	fn := func(groupNo, uid string) (bool, error) { return true, nil }
	RegisterGroupMemberChecker(fn)

	got := getGroupMemberChecker()
	assert.NotNil(t, got)
	ok, err := got("g1", "u1")
	assert.NoError(t, err)
	assert.True(t, ok)

	// cleanup
	setGroupMemberChecker(nil)
}

// TestGroupMemberChecker_ConcurrentAccess is intended to be run with -race.
func TestGroupMemberChecker_ConcurrentAccess(t *testing.T) {
	setGroupMemberChecker(nil)
	defer setGroupMemberChecker(nil)

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			RegisterGroupMemberChecker(func(groupNo, uid string) (bool, error) { return true, nil })
		}()
		go func() {
			defer wg.Done()
			if fn := getGroupMemberChecker(); fn != nil {
				_, _ = fn("g", "u")
			}
		}()
	}
	wg.Wait()
}
