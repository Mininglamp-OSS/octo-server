package group

import (
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/config"
)

func TestGroupAvatarFilePathUsesVersionAwareOctoLibHelper(t *testing.T) {
	cfg := config.New()
	cfg.Avatar.Partition = 100

	tests := []struct {
		name    string
		groupNo string
		version int64
		want    string
	}{
		{
			name:    "legacy path when version is zero",
			groupNo: "G_avatar_cache",
			version: 0,
			want:    "group/1/G_avatar_cache.png",
		},
		{
			name:    "legacy path when version is negative",
			groupNo: "G_avatar_cache",
			version: -1,
			want:    "group/1/G_avatar_cache.png",
		},
		{
			name:    "versioned path when version is positive",
			groupNo: "G_avatar_cache",
			version: 1700000000000000001,
			want:    "group/1/G_avatar_cache/1700000000000000001.png",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := cfg.GetGroupAvatarFilePath(tt.groupNo, tt.version)
			if got != tt.want {
				t.Fatalf("GetGroupAvatarFilePath(%q, %d) = %q, want %q", tt.groupNo, tt.version, got, tt.want)
			}
		})
	}
}
