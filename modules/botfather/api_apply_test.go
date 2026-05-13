package botfather

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/server"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/assert"

	// 导入依赖模块以确保迁移按正确顺序执行
	_ "github.com/Mininglamp-OSS/octo-server/modules/robot"
)

func setupTestBotFather(t *testing.T) (*server.Server, *BotFather) {
	s, ctx := testutil.NewTestServer()
	// module.Setup 已注册所有模块路由，无需手动注册
	// 创建 BotFather 实例仅用于访问 db 和 ctx
	bf := New(ctx)

	// Clean tables
	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	return s, bf
}

func createTestRobot(t *testing.T, bf *BotFather, robotID, creatorUID string, autoApprove int) {
	_, err := bf.db.session.InsertInto("robot").Columns(
		"app_id", "robot_id", "username", "token", "version", "status",
		"creator_uid", "description", "bot_token", "im_token_cache", "bot_commands",
		"auto_approve",
	).Values(
		robotID, robotID, robotID, "test_token", 1, 1,
		creatorUID, "test robot", "bf_"+robotID, "", "[]",
		autoApprove,
	).Exec()
	assert.NoError(t, err)
}

func createTestUser(t *testing.T, bf *BotFather, uid, name string) {
	_, err := bf.db.session.InsertInto("user").Columns(
		"uid", "name", "username", "short_no", "status",
	).Values(
		uid, name, uid, "sn_"+uid, 1,
	).Exec()
	assert.NoError(t, err)
}

func TestRobotApply_RequireApproval(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, bf := setupTestBotFather(t)

	// Create test data
	// ownerUID 必须不同于 testutil.UID，否则会触发"无需申请使用自己的AI"
	ownerUID := "owner_001"
	applicantUID := testutil.UID // testutil.Token 对应的用户
	robotID := "test_robot_001"

	createTestUser(t, bf, ownerUID, "Owner")
	createTestUser(t, bf, applicantUID, "Applicant")
	createTestRobot(t, bf, robotID, ownerUID, 0)

	// Apply for robot access
	req, _ := http.NewRequest("POST", "/v1/robot/apply", bytes.NewReader([]byte(util.ToJson(map[string]interface{}{
		"robot_uid": robotID,
		"remark":    "I want to use this AI",
	}))))
	req.Header.Set("token", testutil.Token)
	w := httptest.NewRecorder()
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var resp map[string]interface{}
	err := json.Unmarshal(w.Body.Bytes(), &resp)
	assert.NoError(t, err)
	assert.Equal(t, "pending", resp["status"])
}

func TestRobotApply_AutoApprove(t *testing.T) {
	s, bf := setupTestBotFather(t)

	// Create test data
	ownerUID := "owner_002"
	applicantUID := testutil.UID
	robotID := "test_robot_002"

	createTestUser(t, bf, ownerUID, "Owner")
	createTestUser(t, bf, applicantUID, "Applicant")
	createTestRobot(t, bf, robotID, ownerUID, 1) // auto_approve=1

	// Apply for robot access - should auto-approve
	req, _ := http.NewRequest("POST", "/v1/robot/apply", bytes.NewReader([]byte(util.ToJson(map[string]interface{}{
		"robot_uid": robotID,
	}))))
	req.Header.Set("token", testutil.Token)
	w := httptest.NewRecorder()
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var resp map[string]interface{}
	err := json.Unmarshal(w.Body.Bytes(), &resp)
	assert.NoError(t, err)
	assert.Equal(t, "approved", resp["status"])
}

func TestRobotApply_Forbidden(t *testing.T) {
	t.Skip("access_mode 三态模式当前 schema 不支持，robot 表只有 auto_approve 两态列")
	s, bf := setupTestBotFather(t)

	// Create test data
	ownerUID := "owner_003"
	applicantUID := testutil.UID
	robotID := "test_robot_003"

	createTestUser(t, bf, ownerUID, "Owner")
	createTestUser(t, bf, applicantUID, "Applicant")
	createTestRobot(t, bf, robotID, ownerUID, 0)

	// Apply for robot access - should be rejected
	req, _ := http.NewRequest("POST", "/v1/robot/apply", bytes.NewReader([]byte(util.ToJson(map[string]interface{}{
		"robot_uid": robotID,
	}))))
	req.Header.Set("token", testutil.Token)
	w := httptest.NewRecorder()
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestRobotApply_OwnRobot(t *testing.T) {
	s, bf := setupTestBotFather(t)

	// Create test data - owner is the same as applicant
	ownerUID := testutil.UID
	robotID := "test_robot_004"

	createTestUser(t, bf, ownerUID, "Owner")
	createTestRobot(t, bf, robotID, ownerUID, 0)

	// Apply for own robot - should fail
	req, _ := http.NewRequest("POST", "/v1/robot/apply", bytes.NewReader([]byte(util.ToJson(map[string]interface{}{
		"robot_uid": robotID,
	}))))
	req.Header.Set("token", testutil.Token)
	w := httptest.NewRecorder()
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestRobotApply_RobotNotExist(t *testing.T) {
	s, bf := setupTestBotFather(t)

	createTestUser(t, bf, testutil.UID, "Applicant")

	// Apply for non-existent robot
	req, _ := http.NewRequest("POST", "/v1/robot/apply", bytes.NewReader([]byte(util.ToJson(map[string]interface{}{
		"robot_uid": "nonexistent_robot",
	}))))
	req.Header.Set("token", testutil.Token)
	w := httptest.NewRecorder()
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestRobotApply_DuplicatePending(t *testing.T) {
	s, bf := setupTestBotFather(t)

	// Create test data
	ownerUID := "owner_005"
	applicantUID := testutil.UID
	robotID := "test_robot_005"

	createTestUser(t, bf, ownerUID, "Owner")
	createTestUser(t, bf, applicantUID, "Applicant")
	createTestRobot(t, bf, robotID, ownerUID, 0)

	// First apply
	req, _ := http.NewRequest("POST", "/v1/robot/apply", bytes.NewReader([]byte(util.ToJson(map[string]interface{}{
		"robot_uid": robotID,
	}))))
	req.Header.Set("token", testutil.Token)
	w := httptest.NewRecorder()
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)

	// Second apply - should fail
	req, _ = http.NewRequest("POST", "/v1/robot/apply", bytes.NewReader([]byte(util.ToJson(map[string]interface{}{
		"robot_uid": robotID,
	}))))
	req.Header.Set("token", testutil.Token)
	w = httptest.NewRecorder()
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestRobotApplySure(t *testing.T) {
	s, bf := setupTestBotFather(t)

	// Create test data
	ownerUID := testutil.UID
	applicantUID := "applicant_006"
	robotID := "test_robot_006"

	createTestUser(t, bf, ownerUID, "Owner")
	createTestUser(t, bf, applicantUID, "Applicant")
	createTestRobot(t, bf, robotID, ownerUID, 0)

	// Insert pending apply
	applyDB := newRobotApplyDB(bf.ctx)
	err := applyDB.insert(&robotApplyModel{
		UID:      applicantUID,
		RobotUID: robotID,
		OwnerUID: ownerUID,
		Remark:   "test",
		Status:   ApplyStatusPending,
	})
	assert.NoError(t, err)

	// Get apply ID
	apply, err := applyDB.queryPendingByUIDAndRobot(applicantUID, robotID)
	assert.NoError(t, err)
	assert.NotNil(t, apply)

	// Approve
	req, _ := http.NewRequest("POST", "/v1/robot/apply/sure", bytes.NewReader([]byte(util.ToJson(map[string]interface{}{
		"apply_id": apply.Id,
	}))))
	req.Header.Set("token", testutil.Token)
	w := httptest.NewRecorder()
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	// Verify status updated
	updatedApply, err := applyDB.queryByID(apply.Id)
	assert.NoError(t, err)
	assert.Equal(t, ApplyStatusApproved, updatedApply.Status)
}

func TestRobotApplyRefuse(t *testing.T) {
	s, bf := setupTestBotFather(t)

	// Create test data
	ownerUID := testutil.UID
	applicantUID := "applicant_007"
	robotID := "test_robot_007"

	createTestUser(t, bf, ownerUID, "Owner")
	createTestUser(t, bf, applicantUID, "Applicant")
	createTestRobot(t, bf, robotID, ownerUID, 0)

	// Insert pending apply
	applyDB := newRobotApplyDB(bf.ctx)
	err := applyDB.insert(&robotApplyModel{
		UID:      applicantUID,
		RobotUID: robotID,
		OwnerUID: ownerUID,
		Remark:   "test",
		Status:   ApplyStatusPending,
	})
	assert.NoError(t, err)

	// Get apply ID
	apply, err := applyDB.queryPendingByUIDAndRobot(applicantUID, robotID)
	assert.NoError(t, err)
	assert.NotNil(t, apply)

	// Refuse
	req, _ := http.NewRequest("PUT", fmt.Sprintf("/v1/robot/apply/refuse/%d", apply.Id), nil)
	req.Header.Set("token", testutil.Token)
	w := httptest.NewRecorder()
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	// Verify status updated
	updatedApply, err := applyDB.queryByID(apply.Id)
	assert.NoError(t, err)
	assert.Equal(t, ApplyStatusRejected, updatedApply.Status)
}

func TestRobotApplies_List(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, bf := setupTestBotFather(t)

	// Create test data
	ownerUID := testutil.UID
	robotID := "test_robot_008"

	createTestUser(t, bf, ownerUID, "Owner")
	createTestRobot(t, bf, robotID, ownerUID, 0)

	// Insert some pending applies
	applyDB := newRobotApplyDB(bf.ctx)
	for i := 1; i <= 3; i++ {
		applicantUID := fmt.Sprintf("applicant_%d", i)
		createTestUser(t, bf, applicantUID, fmt.Sprintf("Applicant %d", i))
		err := applyDB.insert(&robotApplyModel{
			UID:      applicantUID,
			RobotUID: robotID,
			OwnerUID: ownerUID,
			Remark:   fmt.Sprintf("remark %d", i),
			Status:   ApplyStatusPending,
		})
		assert.NoError(t, err)
	}

	// Get applies list
	req, _ := http.NewRequest("GET", "/v1/robot/applies?page=1&page_size=10", nil)
	req.Header.Set("token", testutil.Token)
	w := httptest.NewRecorder()
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var resp RobotApplyListResp
	err := json.Unmarshal(w.Body.Bytes(), &resp)
	assert.NoError(t, err)
	assert.Equal(t, int64(3), resp.Count)
	assert.Len(t, resp.List, 3)
}

func TestRobotApplySure_NotOwner(t *testing.T) {
	s, bf := setupTestBotFather(t)

	// Create test data
	ownerUID := "owner_009"
	applicantUID := "applicant_009"
	notOwnerUID := testutil.UID // testutil.Token is for this user
	robotID := "test_robot_009"

	createTestUser(t, bf, ownerUID, "Owner")
	createTestUser(t, bf, applicantUID, "Applicant")
	createTestUser(t, bf, notOwnerUID, "NotOwner")
	createTestRobot(t, bf, robotID, ownerUID, 0)

	// Insert pending apply
	applyDB := newRobotApplyDB(bf.ctx)
	err := applyDB.insert(&robotApplyModel{
		UID:      applicantUID,
		RobotUID: robotID,
		OwnerUID: ownerUID,
		Remark:   "test",
		Status:   ApplyStatusPending,
	})
	assert.NoError(t, err)

	// Get apply ID
	apply, err := applyDB.queryPendingByUIDAndRobot(applicantUID, robotID)
	assert.NoError(t, err)

	// Try to approve (not owner)
	req, _ := http.NewRequest("POST", "/v1/robot/apply/sure", bytes.NewReader([]byte(util.ToJson(map[string]interface{}{
		"apply_id": apply.Id,
	}))))
	req.Header.Set("token", testutil.Token)
	w := httptest.NewRecorder()
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestRobotApplyReq_JSON(t *testing.T) {
	req := RobotApplyReq{
		RobotUID: "robot_001",
		Remark:   "test remark",
	}

	data, err := json.Marshal(req)
	assert.NoError(t, err)

	var decoded RobotApplyReq
	err = json.Unmarshal(data, &decoded)
	assert.NoError(t, err)
	assert.Equal(t, req, decoded)
}

func TestRobotApplySureReq_JSON(t *testing.T) {
	req := RobotApplySureReq{
		ApplyID: 123,
	}

	data, err := json.Marshal(req)
	assert.NoError(t, err)

	var decoded RobotApplySureReq
	err = json.Unmarshal(data, &decoded)
	assert.NoError(t, err)
	assert.Equal(t, req, decoded)
}

func TestRobotApplyResp_JSON(t *testing.T) {
	resp := RobotApplyResp{
		ID:            1,
		UID:           "user_001",
		RobotUID:      "robot_001",
		RobotName:     "Test Bot",
		ApplicantName: "Test User",
		OwnerUID:      "owner_001",
		Remark:        "test",
		Status:        0,
		CreatedAt:     "2026-03-07 10:00:00",
	}

	data, err := json.Marshal(resp)
	assert.NoError(t, err)

	var decoded RobotApplyResp
	err = json.Unmarshal(data, &decoded)
	assert.NoError(t, err)
	assert.Equal(t, resp, decoded)
}
