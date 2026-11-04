package project

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/require"
)

// Keep this integration test in the Project package: project_external_test.go imports
// the full module registry, so NewTestServer applies the Project migration while mounting
// the actual Common appconfig route.
func TestAppConfigDoesNotExposeAProvisioningContainerID(t *testing.T) {
	s, ctx := testutil.NewTestServer()

	const containerID = "octows-00112233445566778899aabbccddeeff"
	now := time.Now().UTC()
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO `octo_project_provisioning` "+
			"(project_id, space_id, target, container_id, status, next_attempt_at, created_at) "+
			"VALUES (?, ?, ?, ?, ?, ?, ?)",
		"appconfig-disclosure-project", "appconfig-disclosure-space", "fleet", containerID, 0, now, now,
	).Exec()
	require.NoError(t, err)

	for _, path := range []string{
		"/v1/common/appconfig",
		"/v1/common/appconfig?version=99999999",
	} {
		t.Run(path, func(t *testing.T) {
			w := httptest.NewRecorder()
			s.GetRoute().ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			require.NotContains(t, w.Body.String(), containerID,
				"public appconfig response disclosed a provisioning container capability")
		})
	}
}
