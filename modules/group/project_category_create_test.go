package group

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// 创建请求级门禁：Project 群与普通群一致，允许同时携带 project_id 与
// category_id（category 由创建者的 group_setting 承接，见
// applyCreatorCategoryBestEffort）。
func TestGroupReqCheckAllowsProjectAndCategoryTogether(t *testing.T) {
	err := (groupReq{
		Members:    []string{"member"},
		SpaceID:    "space",
		ProjectID:  "project",
		CategoryID: "category",
	}).Check()
	require.NoError(t, err)
}
