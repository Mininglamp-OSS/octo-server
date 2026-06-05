package event

import "testing"

func TestShouldComposeGroupAvatar(t *testing.T) {
	tests := []struct {
		name           string
		isUploadAvatar int
		want           bool
	}{
		{
			name:           "manual group avatar should not be overwritten",
			isUploadAvatar: 1,
			want:           false,
		},
		{
			name:           "auto-managed group avatar can be recomposed",
			isUploadAvatar: 0,
			want:           true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := shouldComposeGroupAvatar(tt.isUploadAvatar)
			if got != tt.want {
				t.Fatalf("shouldComposeGroupAvatar(%d) = %v, want %v", tt.isUploadAvatar, got, tt.want)
			}
		})
	}
}
