package user

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestProductionTokenWritersUseSessionStore(t *testing.T) {
	for _, path := range []string{"api.go", "api_manager.go"} {
		src, err := os.ReadFile(path)
		require.NoError(t, err)
		body := string(src)
		for _, forbidden := range []string{
			"Cache.TokenCachePrefix+loginToken, payload)",
			"Cache.TokenCachePrefix+token, tokenPayload",
			"Cache.UIDTokenCachePrefix, flag, userInfo.UID), token",
			"Cache.UIDTokenCachePrefix, flag, userModel.UID), token",
			"Cache.UIDTokenCachePrefix, config.Web, userInfo.UID), token",
		} {
			require.False(t, strings.Contains(body, forbidden), "%s still bypasses Session Store with %q", path, forbidden)
		}
	}
}
