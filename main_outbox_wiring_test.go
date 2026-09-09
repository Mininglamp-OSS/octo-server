package main

import (
	"strings"
	"testing"
)

func TestLoadWorkspaceInternalTokenForStartup(t *testing.T) {
	const (
		loopEnv  = "OCTO_LOOP_INTERNAL_TOKEN"
		driveEnv = "OCTO_DRIVE_INTERNAL_TOKEN"
	)
	validLoop := strings.Repeat("l", 32)
	validDrive := strings.Repeat("d", 32)

	tests := []struct {
		name       string
		env        map[string]string
		loaderEnv  string
		want       string
		wantErr    bool
		secretLeak string
	}{
		{
			name:      "unset disables capability without error",
			env:       map[string]string{},
			loaderEnv: loopEnv,
		},
		{
			name:       "short token is rejected without echoing the value",
			env:        map[string]string{loopEnv: "short-loop-token"},
			loaderEnv:  loopEnv,
			wantErr:    true,
			secretLeak: "short-loop-token",
		},
		{
			name:       "sibling collision is rejected without echoing the value",
			env:        map[string]string{loopEnv: validLoop, driveEnv: validLoop},
			loaderEnv:  loopEnv,
			wantErr:    true,
			secretLeak: validLoop,
		},
		{
			name:      "unique token is accepted",
			env:       map[string]string{loopEnv: validLoop, driveEnv: validDrive},
			loaderEnv: loopEnv,
			want:      validLoop,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := loadWorkspaceInternalTokenForStartup(func(key string) string {
				return tt.env[key]
			}, tt.loaderEnv)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected validation error")
				}
				if strings.Contains(err.Error(), tt.secretLeak) {
					t.Fatalf("validation error echoed secret: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected validation error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("token = %q, want %q", got, tt.want)
			}
		})
	}
}
