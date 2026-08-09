package auth

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestExplicitTokenReadersDoNotBypassValidator(t *testing.T) {
	for _, path := range []string{
		"../../modules/group/api.go",
		"../../modules/message/api.go",
		"../../modules/qrcode/api.go",
		"../../modules/user/api.go",
	} {
		src, err := os.ReadFile(path)
		require.NoError(t, err)
		body := string(src)
		require.False(t, strings.Contains(body, "auth.Decode("), "%s contains an ad-hoc token decoder", path)
		for lineNo, line := range strings.Split(body, "\n") {
			require.False(t,
				strings.Contains(line, "Cache().Get(") && strings.Contains(line, "TokenCachePrefix"),
				"%s:%d contains a direct token-cache reader", path, lineNo+1,
			)
		}
	}
}
