package user

import "testing"

func TestCreateUserModelAuditLoginType(t *testing.T) {
	tests := []struct {
		name  string
		model createUserModel
		want  string
	}{
		{name: "ordinary registration", model: createUserModel{}, want: "register"},
		{name: "first github login", model: createUserModel{LoginType: "github"}, want: "github"},
		{name: "first gitee login", model: createUserModel{LoginType: "gitee"}, want: "gitee"},
		{name: "first oidc login", model: createUserModel{LoginType: "oidc"}, want: "oidc"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.model.auditLoginType(); got != tt.want {
				t.Fatalf("auditLoginType() = %q, want %q", got, tt.want)
			}
		})
	}
}
