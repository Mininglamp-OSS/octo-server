package project

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"
)

func TestProjectReadListDoesNotCreateProject(t *testing.T) {
	_, p := setup(t)
	seedSpace(t, spaceA, 1)
	token := seedUser(t, "read-empty-list-user")
	seedSpaceMember(t, spaceA, "read-empty-list-user", 0, 1)

	r := mountProject(t, p)
	w := doOn(t, r, http.MethodGet, "/v1/space/"+spaceA+"/projects", token, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("empty project list status = %d, body = %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("X-Total-Count"); got != "0" {
		t.Fatalf("empty project list total = %q, want 0", got)
	}
	var rows []*Resp
	if err := json.Unmarshal(w.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode empty project list: %v; body = %s", err, w.Body.String())
	}
	if len(rows) != 0 {
		t.Fatalf("empty project list returned %d rows", len(rows))
	}

	var projectCount int
	if err := testCtx.DB().SelectBySql(
		"SELECT COUNT(*) FROM `octo_project` WHERE space_id = ?", spaceA,
	).LoadOne(&projectCount); err != nil {
		t.Fatalf("count projects after empty list: %v", err)
	}
	if projectCount != 0 {
		t.Fatalf("empty project list created %d projects", projectCount)
	}
}

func TestProjectReadListRequiresActiveProjectMemberAndCountsFilteredRows(t *testing.T) {
	_, p := setup(t)
	uid := "read-list-user"
	seedUser(t, uid)
	seedSpace(t, spaceA, 1)
	seedSpaceMember(t, spaceA, uid, 0, 1)
	for _, name := range []string{"100% done", "1000 done", "a_b", "ab"} {
		_, err := p.createProject(createInput{SpaceID: spaceA, Creator: uid, Name: name})
		if err != nil {
			t.Fatalf("createProject(%q): %v", name, err)
		}
	}

	result, err := p.readProjects(spaceA, uid, "100%", projectReadPage{Limit: 50})
	if err != nil {
		t.Fatalf("readProjects: %v", err)
	}
	if result.Total != 1 || len(result.Rows) != 1 || result.Rows[0].Name != "100% done" {
		t.Fatalf("literal keyword filter = total %d rows %#v", result.Total, result.Rows)
	}

	if _, err := testCtx.DB().UpdateBySql(
		"UPDATE octo_project_member SET status = 0 WHERE project_id = ? AND uid = ?",
		result.Rows[0].ProjectID, uid,
	).Exec(); err != nil {
		t.Fatalf("remove project member: %v", err)
	}
	if _, err := p.readProject(spaceA, result.Rows[0].ProjectID, uid); !errors.Is(err, errProjectReadNotFound) {
		t.Fatalf("inactive project member detail error = %v, want errProjectReadNotFound", err)
	}
}

func TestProjectReadRosterUsesRepeatableReadSnapshotContract(t *testing.T) {
	_, p := setup(t)
	owner, member := "read-roster-owner", "read-roster-member"
	seedUser(t, owner)
	seedUser(t, member)
	seedSpace(t, spaceA, 1)
	seedSpaceMember(t, spaceA, owner, 0, 1)
	seedSpaceMember(t, spaceA, member, 0, 1)
	project, err := p.createProject(createInput{SpaceID: spaceA, Creator: owner, Name: "roster"})
	if err != nil {
		t.Fatalf("createProject: %v", err)
	}
	if _, err := testCtx.DB().InsertBySql(
		"INSERT INTO octo_project_member (project_id, uid, space_id, role, status, invite_uid, created_at, joined_at, updated_at) VALUES (?, ?, ?, ?, 1, ?, UTC_TIMESTAMP(3), UTC_TIMESTAMP(3), UTC_TIMESTAMP(3))",
		project.ProjectID, member, spaceA, RoleCommon, owner,
	).Exec(); err != nil {
		t.Fatalf("insert project member: %v", err)
	}

	result, err := p.readProjectMembers(spaceA, project.ProjectID, owner, projectReadPage{Limit: 1})
	if err != nil {
		t.Fatalf("readProjectMembers: %v", err)
	}
	if result.Total != 2 || len(result.Rows) != 1 {
		t.Fatalf("roster page/count = total %d rows %d, want 2/1", result.Total, len(result.Rows))
	}
}
