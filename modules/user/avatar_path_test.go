package user

import "testing"

func TestUserAvatarFilePath(t *testing.T) {
	tests := []struct {
		name      string
		uid       string
		partition int
		version   int64
		want      string
	}{
		{
			name:      "legacy path when version is zero",
			uid:       "u_avatar_cache",
			partition: 100,
			version:   0,
			want:      "avatar/44/u_avatar_cache.png",
		},
		{
			name:      "legacy path when version is negative",
			uid:       "u_avatar_cache",
			partition: 100,
			version:   -1,
			want:      "avatar/44/u_avatar_cache.png",
		},
		{
			name:      "versioned path when version is positive",
			uid:       "u_avatar_cache",
			partition: 100,
			version:   1700000000000000001,
			want:      "avatar/44/u_avatar_cache/1700000000000000001.png",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := userAvatarFilePath(tt.uid, tt.partition, tt.version)
			if got != tt.want {
				t.Fatalf("userAvatarFilePath(%q, %d, %d) = %q, want %q", tt.uid, tt.partition, tt.version, got, tt.want)
			}
		})
	}
}
