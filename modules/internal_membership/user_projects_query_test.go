package internal_membership

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/stretchr/testify/require"
)

func TestProjectListRejectsMalformedQueryDuplicates(t *testing.T) {
	for _, query := range []string{
		"?space_id=space-a&uid=User-A&uid=%ZZ",
		"?space_id=space-a&space_id=%ZZ&uid=User-A",
		"?space_id=space-a&project_ids=project-a&project_ids=%ZZ",
	} {
		t.Run(query, func(t *testing.T) {
			store := &stubStore{}
			response := doGetProjects(t, newRouter(newTestModule(store)), testInternalToken, query)
			require.Equal(t, errcode.ErrSharedParamInvalid.ID, decodeError(t, response).Error.Code)
			var body struct {
				Error struct {
					Details map[string]any `json:"details"`
				} `json:"error"`
			}
			require.NoError(t, json.Unmarshal(response.Body.Bytes(), &body))
			require.Empty(t, body.Error.Details,
				"malformed RawQuery must not be reported as one selector's validation failure")
			require.Zero(t, store.projectCalls, "query parse failures must be rejected before lookup")
			require.Zero(t, store.metadataCalls, "query parse failures must be rejected before metadata lookup")
		})
	}
}

// TestProjectListRejectsMalformedQueryKey pins whole-query parsing, not just
// selector parsing. The space_id and uid selector are well-formed and appear
// exactly once; only an unrelated pair has an undecodable key. gin's QueryArray
// discards net/url's parse error, so before the single up-front RawQuery parse
// this request would have been answered as if it were healthy.
func TestProjectListRejectsMalformedQueryKey(t *testing.T) {
	store := &stubStore{}
	response := doGetProjects(t, newRouter(newTestModule(store)), testInternalToken,
		"?space_id=space-a&uid=User-A&%ZZ=1")

	require.Equal(t, http.StatusBadRequest, response.Code, response.Body.String())
	require.Equal(t, errcode.ErrSharedParamInvalid.ID, decodeError(t, response).Error.Code)
	require.Zero(t, store.projectCalls,
		"a malformed query must be rejected before any database work, even when the Space and uid values are otherwise valid")
	require.Zero(t, store.metadataCalls,
		"a malformed query must be rejected before any metadata lookup")
}

func TestProjectListRejectsMalformedQueryBeforeSelectorValidation(t *testing.T) {
	store := &stubStore{}
	response := doGetProjects(t, newRouter(newTestModule(store)), testInternalToken,
		"?space_id=space-a&uid=User-A&project_ids=project-a&%ZZ=1")

	require.Equal(t, http.StatusBadRequest, response.Code, response.Body.String())
	env := decodeError(t, response)
	require.Equal(t, errcode.ErrSharedParamInvalid.ID, env.Error.Code)

	var body struct {
		Error struct {
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &body))
	require.Empty(t, body.Error.Details,
		"an undecodable RawQuery must be rejected before the uid/project_ids conflict is interpreted")
	require.Zero(t, store.projectCalls, "an undecodable RawQuery must be rejected before lookup")
	require.Zero(t, store.metadataCalls, "an undecodable RawQuery must be rejected before metadata lookup")
}
