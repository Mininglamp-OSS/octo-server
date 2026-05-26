package user

import (
	"errors"
	"net/http"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/space"
	"github.com/gin-gonic/gin"
)

var allowedSpaceSettingFields = map[string]bool{
	"voice_input_enabled":         true,
	"voice_feedback_on":           true,
	"voice_feedback_notice_acked": true,
}

func (u *User) getSpaceSetting(c *wkhttp.Context) {
	loginUID := c.GetLoginUID()
	spaceID := space.GetSpaceID(c)
	if spaceID == "" {
		c.ResponseErrorWithStatus(errors.New("space_id is required"), http.StatusBadRequest)
		return
	}

	m, err := u.spaceSettingDB.QuerySpaceSetting(loginUID, spaceID)
	if err != nil {
		c.ResponseErrorWithStatus(errors.New("query space setting failed"), http.StatusInternalServerError)
		return
	}

	resp := gin.H{
		"voice_input_enabled":         0,
		"voice_feedback_on":           0,
		"voice_feedback_notice_acked": 0,
	}
	if m != nil {
		resp["voice_input_enabled"] = m.VoiceInputEnabled
		resp["voice_feedback_on"] = m.VoiceFeedbackOn
		resp["voice_feedback_notice_acked"] = m.VoiceFeedbackNoticeAcked
	}
	c.JSON(http.StatusOK, resp)
}

func (u *User) updateSpaceSetting(c *wkhttp.Context) {
	loginUID := c.GetLoginUID()
	spaceID := space.GetSpaceID(c)
	if spaceID == "" {
		c.ResponseErrorWithStatus(errors.New("space_id is required"), http.StatusBadRequest)
		return
	}

	var body map[string]interface{}
	if err := c.BindJSON(&body); err != nil {
		c.ResponseErrorWithStatus(errors.New("invalid request body"), http.StatusBadRequest)
		return
	}

	fields := make(map[string]interface{})
	for k, v := range body {
		if !allowedSpaceSettingFields[k] {
			c.ResponseErrorWithStatus(errors.New("invalid field: "+k), http.StatusBadRequest)
			return
		}
		val, ok := v.(float64)
		if !ok || (val != 0 && val != 1) {
			c.ResponseErrorWithStatus(errors.New("invalid value for "+k+": must be 0 or 1"), http.StatusBadRequest)
			return
		}
		fields[k] = int(val)
	}

	if len(fields) == 0 {
		c.ResponseErrorWithStatus(errors.New("no valid fields to update"), http.StatusBadRequest)
		return
	}

	if err := u.spaceSettingDB.InsertIgnoreSpaceSetting(loginUID, spaceID); err != nil {
		c.ResponseErrorWithStatus(errors.New("ensure space setting failed"), http.StatusInternalServerError)
		return
	}

	if err := u.spaceSettingDB.UpdateSpaceSetting(loginUID, spaceID, fields); err != nil {
		c.ResponseErrorWithStatus(errors.New("update space setting failed"), http.StatusInternalServerError)
		return
	}

	c.ResponseOK()
}
