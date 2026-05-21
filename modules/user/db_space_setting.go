package user

import (
	"fmt"

	"github.com/gocraft/dbr/v2"
)

type SpaceSettingModel struct {
	UID                     string `db:"uid"`
	SpaceID                 string `db:"space_id"`
	VoiceFeedbackOn         int    `db:"voice_feedback_on"`
	VoiceFeedbackNoticeAcked int    `db:"voice_feedback_notice_acked"`
}

type SpaceSettingDB struct {
	session *dbr.Session
}

func NewSpaceSettingDB(session *dbr.Session) *SpaceSettingDB {
	return &SpaceSettingDB{session: session}
}

func (d *SpaceSettingDB) QuerySpaceSetting(uid, spaceID string) (*SpaceSettingModel, error) {
	var m *SpaceSettingModel
	_, err := d.session.Select("uid", "space_id", "voice_feedback_on", "voice_feedback_notice_acked").
		From("user_space_setting").
		Where("uid=? AND space_id=?", uid, spaceID).
		Load(&m)
	return m, err
}

func (d *SpaceSettingDB) InsertIgnoreSpaceSetting(uid, spaceID string) error {
	_, err := d.session.InsertBySql(
		"INSERT IGNORE INTO user_space_setting (uid, space_id) VALUES (?, ?)",
		uid, spaceID,
	).Exec()
	return err
}

func (d *SpaceSettingDB) UpdateSpaceSetting(uid, spaceID string, fields map[string]interface{}) error {
	if len(fields) == 0 {
		return nil
	}
	stmt := d.session.Update("user_space_setting")
	for k, v := range fields {
		switch k {
		case "voice_feedback_on", "voice_feedback_notice_acked":
			stmt = stmt.Set(k, v)
		default:
			return fmt.Errorf("field %q is not allowed for update", k)
		}
	}
	_, err := stmt.Where("uid=? AND space_id=?", uid, spaceID).Exec()
	return err
}
