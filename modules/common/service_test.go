package common

import (
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/require"
)

func TestServiceGetAppConfigEmptyUsesDefaults(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	cleanAllTablesAndReloadSettings(t, ctx)

	appConfig, err := NewService(ctx).GetAppConfig()
	require.NoError(t, err)
	require.NotNil(t, appConfig)
	require.Zero(t, appConfig.SendWelcomeMessageOn)
}
