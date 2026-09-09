package group

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/stretchr/testify/require"
)

func TestGroupCreateRejectsProjectAndCategoryTogether(t *testing.T) {
	s, _ := newTestServer(t)
	wireI18nRendererForGroupTest(s)

	w := httptest.NewRecorder()
	req, err := http.NewRequest(http.MethodPost, "/v1/group/create", bytes.NewReader([]byte(util.ToJson(map[string]any{
		"name":        "invalid project group",
		"members":     []string{"member"},
		"space_id":    "space",
		"project_id":  "project",
		"category_id": "category",
	}))))
	require.NoError(t, err)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)

	require.Equal(t, http.StatusBadRequest, w.Code, "body: %s", w.Body.String())
	env := decodeEnvelope(t, w.Body.Bytes())
	require.Equal(t, errcode.ErrGroupRequestInvalid.ID, env.Error.Code)
}

func TestGroupReqCheckRejectsProjectAndCategoryTogether(t *testing.T) {
	err := (groupReq{
		Members:    []string{"member"},
		ProjectID:  "project",
		CategoryID: "category",
	}).Check()
	require.Error(t, err)
}
