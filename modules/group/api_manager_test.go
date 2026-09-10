package group

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/Mininglamp-OSS/octo-server/modules/user"
	"github.com/Mininglamp-OSS/octo-server/pkg/aiteam"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestManagerGroupQueriesExcludeAIContainers(t *testing.T) {
	_, ctx := newTestServer(t)
	require.NoError(t, testutil.CleanAllTables(ctx))
	db := NewDB(ctx)
	managerDB := newManagerDB(ctx.DB())

	for _, model := range []*Model{
		{GroupNo: "manager-visible", Name: "manager query", Status: GroupStatusNormal},
		{GroupNo: "manager-hidden-ai", Name: "manager query", Status: GroupStatusNormal, Purpose: aiteam.GroupPurpose},
	} {
		require.NoError(t, db.Insert(model))
	}

	list, err := managerDB.listWithPage(10, 1)
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, "manager-visible", list[0].GroupNo)

	list, err = managerDB.listWithPageAndKeyword("manager query", 10, 1)
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, "manager-visible", list[0].GroupNo)

	count, err := managerDB.queryGroupCountWithKeyWord("manager query")
	require.NoError(t, err)
	assert.EqualValues(t, 1, count)
	count, err = db.queryGroupCount()
	require.NoError(t, err)
	assert.EqualValues(t, 1, count)
	count, err = managerDB.queryGroupCountWithStatus(GroupStatusNormal)
	require.NoError(t, err)
	assert.EqualValues(t, 1, count)

	list, err = managerDB.queryGroupsWithStatus(GroupStatusNormal, 10, 1)
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, "manager-visible", list[0].GroupNo)

	today := time.Now().Format("2006-01-02")
	list, err = managerDB.queryRegisterCountWithDateSpace(today, today)
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, "manager-visible", list[0].GroupNo)
	count, err = db.queryCreatedCountWithDate(today)
	require.NoError(t, err)
	assert.EqualValues(t, 1, count)

	service := NewService(ctx)
	count, err = service.GetAllGroupCount()
	require.NoError(t, err)
	assert.EqualValues(t, 1, count, "statistics total must use the product-visible group population")
	count, err = service.GetCreatedCountWithDate(today)
	require.NoError(t, err)
	assert.EqualValues(t, 1, count, "statistics daily total must use the product-visible group population")
}

func TestGroupList(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx := newTestServer(t)
	m := NewManager(ctx)
	m.Route(s.GetRoute())
	//清除数据
	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)
	err = m.db.Insert(&Model{
		GroupNo: "xxxx",
		Name:    "gxxx",
		Creator: "1111",
	})
	assert.NoError(t, err)
	w := httptest.NewRecorder()
	req, err := http.NewRequest("GET", "/v1/manager/group/list?pageIndex=1&pageSize=10&keyword=gx", nil)
	req.Header.Set("token", testutil.Token)
	assert.NoError(t, err)
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, true, strings.Contains(w.Body.String(), `"group_no":`))
}

func TestGroupCount(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx := newTestServer(t)
	m := NewManager(ctx)
	m.Route(s.GetRoute())
	//清除数据
	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)
	err = m.db.Insert(&Model{
		GroupNo: "111",
		Name:    "sss",
		Creator: "xxx",
	})
	assert.NoError(t, err)
	w := httptest.NewRecorder()
	req, err := http.NewRequest("GET", "/v1/manager/group/count", nil)
	req.Header.Set("token", testutil.Token)
	assert.NoError(t, err)
	s.GetRoute().ServeHTTP(w, req)
}
func TestDisableList(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx := newTestServer(t)
	m := NewManager(ctx)
	m.Route(s.GetRoute())
	//清除数据
	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)
	err = m.db.Insert(&Model{
		GroupNo: "111",
		Name:    "sss",
		Creator: "xxx",
		Status:  GroupStatusDisabled,
	})
	assert.NoError(t, err)
	err = m.userDB.Insert(&user.Model{
		UID:  "xxx",
		Name: "001",
	})
	assert.NoError(t, err)
	w := httptest.NewRecorder()
	req, err := http.NewRequest("GET", "/v1/manager/group/disablelist?page_size=1&page_index=1", nil)
	req.Header.Set("token", testutil.Token)
	assert.NoError(t, err)
	s.GetRoute().ServeHTTP(w, req)
}

func TestBlackList(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx := newTestServer(t)
	m := NewManager(ctx)
	m.Route(s.GetRoute())
	//清除数据
	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)
	err = m.db.Insert(&Model{
		GroupNo: "111",
		Name:    "sss",
		Creator: "xxx",
	})
	assert.NoError(t, err)
	err = m.userDB.Insert(&user.Model{
		UID:  "xxx",
		Name: "001",
	})
	assert.NoError(t, err)
	err = m.db.InsertMember(&MemberModel{
		UID:     "xxx",
		GroupNo: "111",
	})
	assert.NoError(t, err)
	w := httptest.NewRecorder()
	req, err := http.NewRequest("GET", "/v1/manager/groups/111/members/blacklist?page_size=1&page_index=1", nil)
	req.Header.Set("token", testutil.Token)
	assert.NoError(t, err)
	s.GetRoute().ServeHTTP(w, req)
}
