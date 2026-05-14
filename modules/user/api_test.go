package user

import (
	"bytes"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/assert"
)

var token = "token122323"

func TestUser_Register(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx := testutil.NewTestServer()
	// u := New(ctx)
	// u.Route(s.GetRoute())
	//清除数据
	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/user/register", bytes.NewReader([]byte(util.ToJson(map[string]interface{}{
		"code":     "123456",
		"zone":     "0086",
		"phone":    "13600000002",
		"password": "1234567",
		"device": map[string]interface{}{
			"device_id":    "device_id1",
			"device_name":  "device_name1",
			"device_model": "device_model1",
		},
	}))))

	s.GetRoute().ServeHTTP(w, req)
	fmt.Println(w.Body.String())
	assert.Equal(t, true, strings.Contains(w.Body.String(), `"token":`))
	assert.Equal(t, true, strings.Contains(w.Body.String(), `"uid":`))
	assert.Equal(t, true, strings.Contains(w.Body.String(), `"username":`))
	assert.Equal(t, true, strings.Contains(w.Body.String(), `"name":`))
	assert.Equal(t, true, strings.Contains(w.Body.String(), `"sex"`))
	assert.Equal(t, true, strings.Contains(w.Body.String(), `"category"`))
	assert.Equal(t, true, strings.Contains(w.Body.String(), `"short_no":`))
	assert.Equal(t, true, strings.Contains(w.Body.String(), `"zone":"0086"`))
	assert.Equal(t, true, strings.Contains(w.Body.String(), `"phone":"13600000002"`))
	assert.Equal(t, true, strings.Contains(w.Body.String(), `"setting":{"search_by_phone":1,"search_by_short":1,"new_msg_notice":1,"msg_show_detail":1,"voice_on":1,"shock_on":1}`))
}
func TestUsernameRegister(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx := testutil.NewTestServer()
	u := New(ctx)
	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)
	username := "userone123123"
	password := "123123"
	u.db.Insert(&Model{
		UID:      "123",
		Username: username,
		Password: util.MD5(util.MD5(password)),
		Name:     username,
		ShortNo:  "123",
		Status:   1,
	})
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/user/usernameregister", bytes.NewReader([]byte(util.ToJson(map[string]interface{}{
		"username": "skldkdlskds",
		"password": password,
		"device": map[string]interface{}{
			"device_id":    "device_id3",
			"device_name":  "device_name1",
			"device_model": "device_model1",
		},
	}))))
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, true, strings.Contains(w.Body.String(), `"status":110`))
}
func TestUsernameLogin(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx := testutil.NewTestServer()
	u := New(ctx)
	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)
	username := "userone123123"
	password := "123123"
	u.db.Insert(&Model{
		UID:           "123",
		Username:      username,
		Password:      util.MD5(util.MD5(password)),
		Name:          username,
		ShortNo:       "123",
		Status:        1,
		Web3PublicKey: "03af80b90d25145da28c583359beb47b21796b2fe1a23c1511e443e7a64dfdb27d",
	})
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/user/usernamelogin", bytes.NewReader([]byte(util.ToJson(map[string]interface{}{
		"username": username,
		"password": password,
		"device": map[string]interface{}{
			"device_id":    "device_id3",
			"device_name":  "device_name1",
			"device_model": "device_model1",
		},
	}))))
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, true, strings.Contains(w.Body.String(), `"username":userone123123`))
}
func TestUploadWeb3PublicKey(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx := testutil.NewTestServer()
	u := New(ctx)
	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)
	username := "userone"
	password := "123123"
	uid := "123"
	u.db.Insert(&Model{
		UID:      uid,
		Username: username,
		Password: util.MD5(util.MD5(password)),
		Name:     username,
		ShortNo:  "123",
	})
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/user/web3publickey", bytes.NewReader([]byte(util.ToJson(map[string]interface{}{
		"uid":             uid,
		"web3_public_key": "03af80b90d25145da28c583359beb47b21796b2fe1a23c1511e443e7a64dfdb27d",
	}))))
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, true, strings.Contains(w.Body.String(), `"uid":123`))
}
func TestGetVerifyText(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx := testutil.NewTestServer()
	u := New(ctx)
	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)
	uid := "123"
	err = u.db.Insert(&Model{
		UID:           uid,
		Username:      "123",
		ShortNo:       "123",
		Status:        1,
		Web3PublicKey: "03af80b90d25145da28c583359beb47b21796b2fe1a23c1511e443e7a64dfdb27d",
	})
	assert.NoError(t, err)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", fmt.Sprintf("/v1/user/web3verifytext?uid=%s", uid), nil)
	s.GetRoute().ServeHTTP(w, req)
	panic(w.Body)
}
func TestUpdatePassword(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx := testutil.NewTestServer()
	u := New(ctx)
	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)
	username := "userone"
	password := "123123"
	u.db.Insert(&Model{
		UID:           testutil.UID,
		Username:      username,
		Password:      util.MD5(util.MD5(password)),
		Name:          username,
		ShortNo:       "123",
		Web3PublicKey: "03af80b90d25145da28c583359beb47b21796b2fe1a23c1511e443e7a64dfdb27d",
	})
	w := httptest.NewRecorder()

	req, _ := http.NewRequest("PUT", "/v1/user/updatepassword", bytes.NewReader([]byte(util.ToJson(map[string]interface{}{
		"new_password": "new_pwd_123",
		"password":     password,
	}))))
	req.Header.Set("token", testutil.Token)

	s.GetRoute().ServeHTTP(w, req)
	panic(w.Body)
	// assert.Equal(t, http.StatusOK, w.Code)
}
func TestResetPwd(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx := testutil.NewTestServer()
	u := New(ctx)
	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)
	username := "userone"
	password := "123123"
	u.db.Insert(&Model{
		UID:           "123",
		Username:      username,
		Password:      util.MD5(util.MD5(password)),
		Name:          username,
		ShortNo:       "123",
		Web3PublicKey: "03af80b90d25145da28c583359beb47b21796b2fe1a23c1511e443e7a64dfdb27d",
	})
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/user/pwdforget_web3", bytes.NewReader([]byte(util.ToJson(map[string]interface{}{
		"username":    username,
		"password":    "new_pwd_123",
		"verify_text": "hello123",
		"sign_text":   "44459fd9146290dcd913350bae6fe79fd48050d39b3c1c315e8f032af3b555d41af6f2c07d4d0f1d8d5dd041af8175e657ae981cf47e58aa5547ab08fc7066e401",
	}))))
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestUser_Login(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx := testutil.NewTestServer()
	u := New(ctx)

	err := u.db.Insert(&Model{
		UID:           testutil.UID,
		Name:          "admin",
		Username:      "admin",
		Sex:           1,
		Password:      util.MD5(util.MD5("123456")),
		Category:      "客服",
		ShortNo:       "uid_xxx1",
		SearchByPhone: 1,
		SearchByShort: 1,
		NewMsgNotice:  1,
		MsgShowDetail: 1,
		VoiceOn:       1,
		ShockOn:       1,
		DeviceLock:    0,
		Status:        1,
		Zone:          "0086",
		Phone:         "13600000001",
	})
	assert.NoError(t, err)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/user/login", bytes.NewReader([]byte(util.ToJson(map[string]interface{}{
		"username": "admin",
		"password": "123456",
		"device": map[string]interface{}{
			"device_id":    "device_id3",
			"device_name":  "device_name1",
			"device_model": "device_model1",
		},
	}))))
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, true, strings.Contains(w.Body.String(), `"token":`))
	assert.Equal(t, true, strings.Contains(w.Body.String(), fmt.Sprintf(`"uid":"%s"`, testutil.UID)))
	assert.Equal(t, true, strings.Contains(w.Body.String(), `"username":"admin"`))
	assert.Equal(t, true, strings.Contains(w.Body.String(), `"name":"admin"`))
	assert.Equal(t, true, strings.Contains(w.Body.String(), `"sex":1`))
	assert.Equal(t, true, strings.Contains(w.Body.String(), `"category":"客服"`))
	assert.Equal(t, true, strings.Contains(w.Body.String(), `"short_no":"uid_xxx1"`))
	assert.Equal(t, true, strings.Contains(w.Body.String(), `"zone":"0086"`))
	assert.Equal(t, true, strings.Contains(w.Body.String(), `"phone":"13600000001"`))

	time.Sleep(2 * time.Second)
}

func TestUser_Search(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx := testutil.NewTestServer()

	u := New(ctx)
	u.Route(s.GetRoute())
	//清除数据
	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)
	err = u.db.Insert(&Model{
		UID:           "1234",
		Zone:          "0086",
		Phone:         "13600000001",
		Username:      "008613600000001",
		Password:      util.MD5(util.MD5("123456")),
		Name:          "tt",
		ShortNo:       "wukongchat_001",
		SearchByPhone: 1,
		SearchByShort: 1,
	})
	assert.NoError(t, err)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/v1/user/search?keyword=wukongchat_001", nil)
	s.GetRoute().ServeHTTP(w, req)

	fmt.Println(w.Body.String())

	assert.Equal(t, true, strings.Contains(w.Body.String(), `"exist":1`))
	assert.Equal(t, true, strings.Contains(w.Body.String(), `"uid":"1234"`))
	assert.Equal(t, true, strings.Contains(w.Body.String(), `"name":"tt"`))

}

func TestUserGet(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx := testutil.NewTestServer()

	u := New(ctx)
	u.Route(s.GetRoute())
	//清除数据
	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	err = u.db.Insert(&Model{
		UID:      "1234",
		Username: "admin",
		Password: util.MD5(util.MD5("123456")),
		Name:     "tt",
		Category: "客服",
		Sex:      1,
		ShortNo:  "test11",
	})
	assert.NoError(t, err)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/v1/users/1234", nil)
	req.Header.Set("token", token)
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, true, strings.Contains(w.Body.String(), `"uid":"1234"`))
	assert.Equal(t, true, strings.Contains(w.Body.String(), `"name":"tt"`))
	assert.Equal(t, true, strings.Contains(w.Body.String(), `"category":"客服"`))
	assert.Equal(t, true, strings.Contains(w.Body.String(), `"sex":1`))
	assert.Equal(t, true, strings.Contains(w.Body.String(), `"short_no":"test11"`))

}

func TestUserUpdateInfo(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx := testutil.NewTestServer()
	u := New(ctx)
	u.Route(s.GetRoute())
	//清除数据
	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	err = u.db.Insert(&Model{
		UID:           testutil.UID,
		Name:          "admin",
		Username:      "admin",
		Sex:           1,
		Password:      util.MD5(util.MD5("123456")),
		Category:      "客服",
		ShortNo:       "uid_xxx1",
		SearchByPhone: 1,
		SearchByShort: 1,
		NewMsgNotice:  1,
		MsgShowDetail: 1,
		VoiceOn:       1,
		ShockOn:       1,
		Zone:          "0086",
		Phone:         "13600000001",
	})
	assert.NoError(t, err)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("PUT", "/v1/user/current", bytes.NewReader([]byte(util.ToJson(map[string]interface{}{
		"name": "张丹丹",
	}))))
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestUserSetting(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx := testutil.NewTestServer()
	u := New(ctx)
	u.Route(s.GetRoute())
	//清除数据
	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	err = u.db.Insert(&Model{
		UID:           testutil.UID,
		Name:          "admin",
		Username:      "admin",
		Sex:           1,
		Password:      util.MD5(util.MD5("123456")),
		Category:      "客服",
		ShortNo:       "uid_xxx1",
		SearchByPhone: 1,
		SearchByShort: 1,
		NewMsgNotice:  1,
		MsgShowDetail: 1,
		VoiceOn:       1,
		ShockOn:       1,
		Zone:          "0086",
		Phone:         "13600000001",
	})
	assert.NoError(t, err)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("PUT", "/v1/user/my/setting", bytes.NewReader([]byte(util.ToJson(map[string]interface{}{
		"device_lock": 1,
	}))))
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
}
func TestAddBlackList(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx := testutil.NewTestServer()
	u := New(ctx)
	u.Route(s.GetRoute())
	//清除数据
	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)
	err = u.db.Insert(&Model{
		UID:           "adminuid1",
		Name:          "admin",
		Username:      "admin",
		Sex:           1,
		Password:      util.MD5(util.MD5("123456")),
		Category:      "客服",
		ShortNo:       "uid_xxx1",
		SearchByPhone: 1,
		SearchByShort: 1,
		NewMsgNotice:  1,
		MsgShowDetail: 1,
		VoiceOn:       1,
		ShockOn:       1,
		Zone:          "0086",
		Phone:         "13600000001",
	})
	assert.NoError(t, err)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/user/blacklist/adminuid1", nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestBlacklists(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx := testutil.NewTestServer()
	u := New(ctx)
	u.Route(s.GetRoute())
	//清除数据
	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)
	err = u.db.Insert(&Model{
		UID:           "adminuid2",
		Name:          "admin",
		Username:      "admin",
		Sex:           1,
		Password:      util.MD5(util.MD5("123456")),
		Category:      "客服",
		ShortNo:       "uid_xxx1",
		SearchByPhone: 1,
		SearchByShort: 1,
		NewMsgNotice:  1,
		MsgShowDetail: 1,
		VoiceOn:       1,
		ShockOn:       1,
		Zone:          "0086",
		Phone:         "13600000001",
	})
	assert.NoError(t, err)

	err = u.settingDB.InsertUserSettingModel(&SettingModel{
		UID:       testutil.UID,
		ToUID:     "adminuid2",
		Blacklist: 1,
	})
	assert.NoError(t, err)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/v1/user/blacklists", nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, true, strings.Contains(w.Body.String(), `"uid":"adminuid2"`))
	assert.Equal(t, true, strings.Contains(w.Body.String(), `"name":"admin"`))
	assert.Equal(t, true, strings.Contains(w.Body.String(), `"usename":"admin"`))
}

func TestSetChatPwd(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx := testutil.NewTestServer()
	u := New(ctx)
	u.Route(s.GetRoute())
	//清除数据
	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	err = u.db.Insert(&Model{
		UID:           testutil.UID,
		Name:          "admin",
		Username:      "admin",
		Sex:           1,
		Password:      util.MD5(util.MD5("123456")),
		Category:      "客服",
		ShortNo:       "uid_xxx1",
		SearchByPhone: 1,
		SearchByShort: 1,
		NewMsgNotice:  1,
		MsgShowDetail: 1,
		VoiceOn:       1,
		ShockOn:       1,
		Zone:          "0086",
		Phone:         "13600000001",
	})
	assert.NoError(t, err)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/user/chatpwd", bytes.NewReader([]byte(util.ToJson(map[string]interface{}{
		"login_pwd": "123456",
		"chat_pwd":  "111111",
	}))))
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
}
func TestSendLoginCheckPhoneCode(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx := testutil.NewTestServer()
	u := New(ctx)
	u.Route(s.GetRoute())
	//清除数据
	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	err = u.db.Insert(&Model{
		UID:           testutil.UID,
		Name:          "admin",
		Username:      "admin",
		Sex:           1,
		Password:      util.MD5(util.MD5("123456")),
		Category:      "客服",
		ShortNo:       "uid_xxx1",
		SearchByPhone: 1,
		SearchByShort: 1,
		NewMsgNotice:  1,
		MsgShowDetail: 1,
		VoiceOn:       1,
		ShockOn:       1,
		Zone:          "0086",
		Phone:         "13781388696",
	})
	assert.NoError(t, err)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/user/sms/login_check_phone", bytes.NewReader([]byte(util.ToJson(map[string]interface{}{
		"uid": testutil.UID,
	}))))
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestLoginCheckPhone(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx := testutil.NewTestServer()
	u := New(ctx)
	u.Route(s.GetRoute())
	w := httptest.NewRecorder()
	err := u.db.Insert(&Model{
		UID:           testutil.UID,
		Name:          "admin",
		Username:      "admin",
		Sex:           1,
		Password:      util.MD5(util.MD5("123456")),
		Category:      "客服",
		ShortNo:       "uid_xxx1",
		SearchByPhone: 1,
		SearchByShort: 1,
		NewMsgNotice:  1,
		MsgShowDetail: 1,
		VoiceOn:       1,
		ShockOn:       1,
		Zone:          "0086",
		Phone:         "13781388696",
	})
	assert.NoError(t, err)
	req, _ := http.NewRequest("POST", "/v1/user/login/check_phone", bytes.NewReader([]byte(util.ToJson(map[string]interface{}{
		"uid":  testutil.UID,
		"code": "3346",
	}))))
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, true, strings.Contains(w.Body.String(), `"token":`))
	assert.Equal(t, true, strings.Contains(w.Body.String(), `"uid":`))
	assert.Equal(t, true, strings.Contains(w.Body.String(), `"username":"admin"`))
	assert.Equal(t, true, strings.Contains(w.Body.String(), `"name":"admin"`))
	assert.Equal(t, true, strings.Contains(w.Body.String(), `"sex":1`))
	assert.Equal(t, true, strings.Contains(w.Body.String(), `"category":"客服"`))
	assert.Equal(t, true, strings.Contains(w.Body.String(), `"short_no":"uid_xxx1"`))
	assert.Equal(t, true, strings.Contains(w.Body.String(), `"zone":"0086"`))
	assert.Equal(t, true, strings.Contains(w.Body.String(), `"phone":"13781388696"`))
	assert.Equal(t, true, strings.Contains(w.Body.String(), `"setting":{"search_by_phone":1,"search_by_short":1,"new_msg_notice":1,"msg_show_detail":1,"voice_on":1,"shock_on":1}`))
}
func TestCustomerservices(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx := testutil.NewTestServer()
	u := New(ctx)
	u.Route(s.GetRoute())
	err := u.db.Insert(&Model{
		UID:           testutil.UID,
		Name:          "admin",
		Username:      "admin",
		Sex:           1,
		Password:      util.MD5(util.MD5("123456")),
		Category:      "service",
		ShortNo:       "uid_xxx1",
		SearchByPhone: 1,
		SearchByShort: 1,
		NewMsgNotice:  1,
		MsgShowDetail: 1,
		VoiceOn:       1,
		ShockOn:       1,
		Zone:          "0086",
		Phone:         "13781388696",
	})
	assert.NoError(t, err)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/v1/user/customerservices", nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, true, strings.Contains(w.Body.String(), `"uid":`))
	assert.Equal(t, true, strings.Contains(w.Body.String(), `"name":"admin"`))
}

func TestUploadAvatar(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	path := "../../../assets/assets/avatar.png"
	file, err := os.Open(path)
	if err != nil {
		t.Error(err)
	}
	defer file.Close()
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, err := writer.CreateFormFile("file", path)
	if err != nil {
		writer.Close()
		t.Error(err)
	}
	io.Copy(part, file)
	writer.Close()

	s, ctx := testutil.NewTestServer()
	u := New(ctx)
	u.Route(s.GetRoute())
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", fmt.Sprintf("/v1/users/%s/avatar", testutil.UID), body)
	req.Header.Set("token", testutil.Token)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestGetUserRedDot(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	u := New(ctx)
	//u.Route(s.GetRoute())
	//清除数据
	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	err = u.db.insertUserRedDot(&userRedDotModel{
		UID:      testutil.UID,
		Count:    1,
		IsDot:    0,
		Category: UserRedDotCategoryFriendApply,
	})
	assert.NoError(t, err)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", fmt.Sprintf("/v1/user/reddot/%s", UserRedDotCategoryFriendApply), nil)
	req.Header.Set("token", token)
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, true, strings.Contains(w.Body.String(), `"count":1`))
	// assert.Equal(t, http.StatusOK, w.Code)
}

func TestDeleteUserRedDot(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	u := New(ctx)
	//u.Route(s.GetRoute())
	//清除数据
	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	err = u.db.insertUserRedDot(&userRedDotModel{
		UID:      testutil.UID,
		Count:    1,
		IsDot:    0,
		Category: UserRedDotCategoryFriendApply,
	})
	assert.NoError(t, err)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("DELETE", fmt.Sprintf("/v1/user/reddot/%s", UserRedDotCategoryFriendApply), nil)
	req.Header.Set("token", token)
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
}

// TestUser_Login_WrongPassword 测试使用错误密码登录
func TestUser_Login_WrongPassword(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	u := New(ctx)
	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	err = u.db.Insert(&Model{
		UID:      testutil.UID,
		Name:     "admin",
		Username: "admin",
		Password: util.MD5(util.MD5("123456")),
		ShortNo:  "uid_xxx1",
		Status:   1,
	})
	assert.NoError(t, err)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/user/login", bytes.NewReader([]byte(util.ToJson(map[string]interface{}{
		"username": "admin",
		"password": "wrong_password",
		"device": map[string]interface{}{
			"device_id":    "device_id1",
			"device_name":  "device_name1",
			"device_model": "device_model1",
		},
	}))))
	s.GetRoute().ServeHTTP(w, req)
	// 密码错误应该返回错误
	assert.NotEqual(t, http.StatusOK, w.Code)
}

// TestUser_Login_NonExistentUser 测试使用不存在的用户登录
func TestUser_Login_NonExistentUser(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	_ = New(ctx)
	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/user/login", bytes.NewReader([]byte(util.ToJson(map[string]interface{}{
		"username": "nonexistent_user",
		"password": "123456",
		"device": map[string]interface{}{
			"device_id":    "device_id1",
			"device_name":  "device_name1",
			"device_model": "device_model1",
		},
	}))))
	s.GetRoute().ServeHTTP(w, req)
	assert.NotEqual(t, http.StatusOK, w.Code)
}

// TestUser_Register_MissingFields 测试缺少必填字段的注册请求
func TestUser_Register_MissingFields(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	_ = New(ctx)
	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	// 缺少 phone
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/user/register", bytes.NewReader([]byte(util.ToJson(map[string]interface{}{
		"code":     "123456",
		"zone":     "0086",
		"password": "1234567",
	}))))
	s.GetRoute().ServeHTTP(w, req)
	assert.NotEqual(t, http.StatusOK, w.Code)
}

// TestUserGet_NotFound 测试获取不存在的用户
func TestUserGet_NotFound(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx := testutil.NewTestServer()
	u := New(ctx)
	u.Route(s.GetRoute())
	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/v1/users/nonexistent_uid", nil)
	req.Header.Set("token", token)
	s.GetRoute().ServeHTTP(w, req)
	// 用户不存在时不应返回正常用户数据
	assert.False(t, strings.Contains(w.Body.String(), `"uid":"nonexistent_uid"`))
}

// TestRemoveBlackList 测试移除黑名单
func TestRemoveBlackList(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx := testutil.NewTestServer()
	u := New(ctx)
	u.Route(s.GetRoute())
	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	err = u.db.Insert(&Model{
		UID:      "target_uid",
		Name:     "target",
		Username: "target",
		ShortNo:  "uid_xxx2",
	})
	assert.NoError(t, err)

	// 先添加黑名单
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/user/blacklist/target_uid", nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)

	// 再移除黑名单
	w2 := httptest.NewRecorder()
	req2, _ := http.NewRequest("DELETE", "/v1/user/blacklist/target_uid", nil)
	req2.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w2, req2)
	assert.Equal(t, http.StatusOK, w2.Code)
}

// TestUserUpdateInfo_MultipleFields 测试更新多个用户字段
func TestUserUpdateInfo_MultipleFields(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx := testutil.NewTestServer()
	u := New(ctx)
	u.Route(s.GetRoute())
	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	err = u.db.Insert(&Model{
		UID:      testutil.UID,
		Name:     "admin",
		Username: "admin",
		Sex:      1,
		Password: util.MD5(util.MD5("123456")),
		ShortNo:  "uid_xxx1",
		Zone:     "0086",
		Phone:    "13600000001",
	})
	assert.NoError(t, err)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("PUT", "/v1/user/current", bytes.NewReader([]byte(util.ToJson(map[string]interface{}{
		"name": "新名字",
		"sex":  0,
	}))))
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
}

// TestUser_Search_ByPhone 测试通过手机号搜索
func TestUser_Search_ByPhone(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx := testutil.NewTestServer()
	u := New(ctx)
	u.Route(s.GetRoute())
	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)
	err = u.db.Insert(&Model{
		UID:           "1234",
		Zone:          "0086",
		Phone:         "13600000099",
		Username:      "008613600000099",
		Password:      util.MD5(util.MD5("123456")),
		Name:          "手机搜索",
		ShortNo:       "wukongchat_099",
		SearchByPhone: 1,
		SearchByShort: 1,
	})
	assert.NoError(t, err)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/v1/user/search?keyword=008613600000099", nil)
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, true, strings.Contains(w.Body.String(), `"exist":1`))
	assert.Equal(t, true, strings.Contains(w.Body.String(), `"uid":"1234"`))
}

// TestUserSetting_DeviceLock 测试设备锁设置
func TestUserSetting_DeviceLock(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx := testutil.NewTestServer()
	u := New(ctx)
	u.Route(s.GetRoute())
	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	err = u.db.Insert(&Model{
		UID:      testutil.UID,
		Name:     "admin",
		Username: "admin",
		ShortNo:  "uid_xxx1",
		Password: util.MD5(util.MD5("123456")),
	})
	assert.NoError(t, err)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("PUT", "/v1/user/my/setting", bytes.NewReader([]byte(util.ToJson(map[string]interface{}{
		"device_lock": 1,
	}))))
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
}

// TestAddBotFatherFriend_Bidirectional 测试注册用户和BotFather双向好友关系
func TestAddBotFatherFriend_Bidirectional(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	u := New(ctx)
	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	const botFatherUID = "botfather"
	testUID := "test_user_" + fmt.Sprintf("%d", time.Now().UnixNano())

	// 创建 BotFather 用户（模拟系统初始化）
	err = u.db.Insert(&Model{
		UID:      botFatherUID,
		Name:     "BotFather",
		Username: "botfather",
		ShortNo:  "bf001",
		Status:   1,
	})
	assert.NoError(t, err)

	// 创建测试用户
	err = u.db.Insert(&Model{
		UID:      testUID,
		Name:     "TestUser",
		Username: "testuser",
		ShortNo:  "tu001",
		Status:   1,
	})
	assert.NoError(t, err)

	// 调用 addBotFatherFriend
	err = u.addBotFatherFriend(testUID)
	assert.NoError(t, err)

	// 验证双向好友关系
	// 1. 用户 → BotFather
	isFriend1, err := u.friendDB.IsFriend(testUID, botFatherUID)
	assert.NoError(t, err)
	assert.True(t, isFriend1, "用户应该是BotFather的好友")

	// 2. BotFather → 用户
	isFriend2, err := u.friendDB.IsFriend(botFatherUID, testUID)
	assert.NoError(t, err)
	assert.True(t, isFriend2, "BotFather应该是用户的好友")
}

// TestSendQRCodeInfo_ConcurrentSendAndRemove 测试 QRCode 发送与删除的并发安全性
// 此测试不依赖数据库，直接操作全局 qrcodeChanMap
func TestSendQRCodeInfo_ConcurrentSendAndRemove(t *testing.T) {
	u := &User{}
	// 测试多轮并发操作，确保没有竞态条件
	for round := 0; round < 100; round++ {
		uuid := fmt.Sprintf("test-uuid-%d", round)

		// 使用 getQRCodeModelChan（buffered channel）
		qrcodeChan := u.getQRCodeModelChan(uuid)

		var wg sync.WaitGroup
		wg.Add(2)

		// Goroutine 1: 发送 QRCode 数据
		go func() {
			defer wg.Done()
			qrcodeInfo := common.NewQRCodeModel(common.QRCodeTypeScanLogin, map[string]interface{}{
				"status": 1,
			})
			SendQRCodeInfo(uuid, qrcodeInfo)
		}()

		// Goroutine 2: 移除 channel
		go func() {
			defer wg.Done()
			u.removeQRCodeChan(uuid)
		}()

		// 等待两个 goroutine 完成
		wg.Wait()

		// 尝试从 channel 接收（非阻塞），验证不会 panic
		select {
		case _, ok := <-qrcodeChan:
			if !ok {
				// channel closed by removeQRCodeChan, expected
			}
			// 收到数据或 zero value from closed channel, both OK
		default:
			// 没有数据（发送被跳过），也是正常的
		}
	}
}

// TestSendQRCodeInfo_NoReceiverDoesNotBlock 测试无接收者时发送不会阻塞
// 此测试不依赖数据库，直接操作全局 qrcodeChanMap
func TestSendQRCodeInfo_NoReceiverDoesNotBlock(t *testing.T) {
	u := &User{}
	uuid := "test-no-receiver"

	// 使用 getQRCodeModelChan（buffered channel, size=1）
	_ = u.getQRCodeModelChan(uuid)

	// 不启动接收者，直接发送
	done := make(chan bool)
	go func() {
		qrcodeInfo := common.NewQRCodeModel(common.QRCodeTypeScanLogin, map[string]interface{}{
			"status": 1,
		})
		SendQRCodeInfo(uuid, qrcodeInfo)
		done <- true
	}()

	// 验证发送不会阻塞（1秒内完成）
	select {
	case <-done:
		// 正常完成
	case <-time.After(1 * time.Second):
		t.Fatal("SendQRCodeInfo 阻塞超时，应使用非阻塞发送")
	}

	// 清理
	u.removeQRCodeChan(uuid)
}

// TestRemoveQRCodeChan_ClosesChannel 测试 removeQRCodeChan 关闭 channel 不会 panic
func TestRemoveQRCodeChan_ClosesChannel(t *testing.T) {
	u := &User{}
	uuid := "test-close-chan"

	ch := u.getQRCodeModelChan(uuid)

	// remove should close the channel
	u.removeQRCodeChan(uuid)

	// reading from closed channel should return zero value, not block
	select {
	case val, ok := <-ch:
		if ok {
			t.Fatalf("expected closed channel, got value: %v", val)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("reading from closed channel should not block")
	}

	// double remove should not panic
	u.removeQRCodeChan(uuid)
}

// TestBufferedChannel_PreventsTOCTOU 测试 buffered channel 防止 TOCTOU 消息丢失
func TestBufferedChannel_PreventsTOCTOU(t *testing.T) {
	u := &User{}
	uuid := "test-toctou"

	ch := u.getQRCodeModelChan(uuid)

	// 先发送（在接收者 select 之前），buffered channel 不会丢失
	qrcodeInfo := common.NewQRCodeModel(common.QRCodeTypeScanLogin, map[string]interface{}{
		"status": 1,
	})
	SendQRCodeInfo(uuid, qrcodeInfo)

	// 然后接收，应该能拿到数据
	select {
	case val, ok := <-ch:
		if !ok {
			t.Fatal("channel closed unexpectedly")
		}
		if val == nil {
			t.Fatal("expected qrcode data, got nil")
		}
	case <-time.After(1 * time.Second):
		t.Fatal("buffered channel should have data available immediately")
	}

	u.removeQRCodeChan(uuid)
}
