package group

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestResolveScanJoinIdentity(t *testing.T) {
	const (
		groupSpace = "space_b"
		curSpace   = "space_a"
		homeSpace  = "space_home"
	)
	errDB := errors.New("db down")
	member := func(spaces ...string) func(string) (bool, error) {
		set := map[string]bool{}
		for _, s := range spaces {
			set[s] = true
		}
		return func(id string) (bool, error) { return set[id], nil }
	}
	home := func() string { return homeSpace }

	cases := []struct {
		name          string
		groupSpace    string
		allowExternal int
		header        string
		isMember      func(string) (bool, error)
		want          scanJoinIdentity
		wantErr       bool
	}{
		{
			name: "private group has no space identity",
			isMember: func(string) (bool, error) {
				t.Fatal("membership must not be queried for private groups")
				return false, nil
			},
			header: curSpace,
		},
		{
			name:          "member of both spaces scanning from current space joins with current space identity",
			groupSpace:    groupSpace,
			allowExternal: 1,
			header:        curSpace,
			isMember:      member(groupSpace, curSpace),
			want:          scanJoinIdentity{IsExternal: 1, SourceSpaceID: curSpace},
		},
		{
			name:          "member of both spaces stays internal when group forbids external members",
			groupSpace:    groupSpace,
			allowExternal: 0,
			header:        curSpace,
			isMember:      member(groupSpace, curSpace),
			want:          scanJoinIdentity{},
		},
		{
			name:          "scanning from group space joins as internal member",
			groupSpace:    groupSpace,
			allowExternal: 1,
			header:        groupSpace,
			isMember:      member(groupSpace, curSpace),
			want:          scanJoinIdentity{},
		},
		{
			name:          "group space member without header joins as internal member",
			groupSpace:    groupSpace,
			allowExternal: 1,
			isMember:      member(groupSpace),
			want:          scanJoinIdentity{},
		},
		{
			name:          "group space member with forged header joins as internal member",
			groupSpace:    groupSpace,
			allowExternal: 1,
			header:        "space_forged",
			isMember:      member(groupSpace),
			want:          scanJoinIdentity{HeaderIgnored: true},
		},
		{
			name:          "outsider joins as external member from current space",
			groupSpace:    groupSpace,
			allowExternal: 1,
			header:        curSpace,
			isMember:      member(curSpace),
			want:          scanJoinIdentity{IsExternal: 1, SourceSpaceID: curSpace},
		},
		{
			name:          "outsider with forged header falls back to home space",
			groupSpace:    groupSpace,
			allowExternal: 1,
			header:        "space_forged",
			isMember:      member(curSpace),
			want:          scanJoinIdentity{IsExternal: 1, SourceSpaceID: homeSpace, HeaderIgnored: true},
		},
		{
			name:          "outsider without header falls back to home space",
			groupSpace:    groupSpace,
			allowExternal: 1,
			isMember:      member(curSpace),
			want:          scanJoinIdentity{IsExternal: 1, SourceSpaceID: homeSpace},
		},
		{
			name:          "outsider is forbidden when group forbids external members",
			groupSpace:    groupSpace,
			allowExternal: 0,
			header:        curSpace,
			isMember:      member(curSpace),
			want:          scanJoinIdentity{Forbidden: true},
		},
		{
			name:          "header membership error degrades to home space",
			groupSpace:    groupSpace,
			allowExternal: 1,
			header:        curSpace,
			isMember: func(id string) (bool, error) {
				if id == curSpace {
					return false, errDB
				}
				return false, nil
			},
			want: scanJoinIdentity{IsExternal: 1, SourceSpaceID: homeSpace, HeaderIgnored: true, HeaderErr: errDB},
		},
		{
			name:          "group space membership error is returned",
			groupSpace:    groupSpace,
			allowExternal: 1,
			header:        curSpace,
			isMember:      func(string) (bool, error) { return false, errDB },
			wantErr:       true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveScanJoinIdentity(tc.groupSpace, tc.allowExternal, tc.header, tc.isMember, home)
			if tc.wantErr {
				assert.Error(t, err)
				return
			}
			assert.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}
