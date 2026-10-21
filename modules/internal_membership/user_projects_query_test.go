package internal_membership

import (
	"net/http"
	"testing"

	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/stretchr/testify/require"
)

func TestUserProjectsRejectsMalformedQueryDuplicates(t *testing.T) {
	for _, query := range []string{
		"?space_id=space-a&uid=User-A&uid=%ZZ",
		"?space_id=space-a&space_id=%ZZ&uid=User-A",
	} {
		t.Run(query, func(t *testing.T) {
			store := &stubStore{}
			response := doGetProjects(t, newRouter(newTestModule(store)), testInternalToken, query)
			require.Equal(t, http.StatusBadRequest, response.Code, response.Body.String())
			require.Equal(t, errcode.ErrSharedParamInvalid.ID, decodeError(t, response).Error.Code)
			require.Zero(t, store.projectCalls, "query parse failures must be rejected before lookup")
		})
	}
}

// TestUserProjectsRejectsMalformedQueryKey pins that the whole query is
// rejected, not just the selectors. Both selectors here are well-formed and
// appear exactly once; only an unrelated pair has an undecodable key. gin's
// QueryArray discards net/url's parse error, so before the single up-front
// RawQuery parse this request would have been answered as if it were healthy.
func TestUserProjectsRejectsMalformedQueryKey(t *testing.T) {
	store := &stubStore{}
	response := doGetProjects(t, newRouter(newTestModule(store)), testInternalToken,
		"?space_id=space-a&uid=User-A&%ZZ=1")

	require.Equal(t, http.StatusBadRequest, response.Code, response.Body.String())
	require.Equal(t, errcode.ErrSharedParamInvalid.ID, decodeError(t, response).Error.Code)
	require.Zero(t, store.projectCalls,
		"a malformed query must be rejected before any database work, even when both selectors are valid")
}
