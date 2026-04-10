package voice

import (
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-server/pkg/space"
	"github.com/gocraft/dbr/v2"
)

// VoiceStore abstracts database operations for the voice module.
type VoiceStore interface {
	QueryVoiceContext(uid, spaceID string) (*UserVoiceContextModel, error)
	CheckSpaceMembership(spaceID, uid string) (bool, error)
}

// VoiceDB handles database operations for the voice module
type VoiceDB struct {
	session *dbr.Session
	ctx     *config.Context
}

// NewVoiceDB creates a new VoiceDB instance
func NewVoiceDB(ctx *config.Context) *VoiceDB {
	return &VoiceDB{
		session: ctx.DB(),
		ctx:     ctx,
	}
}

// UserVoiceContextModel represents a user's voice correction context
type UserVoiceContextModel struct {
	ID                int64     `db:"id"`
	UID               string    `db:"uid"`
	SpaceID           string    `db:"space_id"`
	ASRCorrectContext string    `db:"asr_correct_context" json:"context"`
	CreatedAt         time.Time `db:"created_at"`
	UpdatedAt         time.Time `db:"updated_at"`
	UpdatedBy         string    `db:"updated_by"`
}

// CheckSpaceMembership checks if the user is a member of the given space.
func (d *VoiceDB) CheckSpaceMembership(spaceID, uid string) (bool, error) {
	return space.CheckMembership(d.session, spaceID, uid)
}

// QueryVoiceContext returns the user's voice correction context for a given space.
// Returns nil if not set.
func (d *VoiceDB) QueryVoiceContext(uid, spaceID string) (*UserVoiceContextModel, error) {
	var m *UserVoiceContextModel
	_, err := d.session.Select("*").From("user_voice_context").
		Where("uid=? AND space_id=?", uid, spaceID).Load(&m)
	return m, err
}

// UpsertVoiceContext inserts or updates the voice correction context (PUT idempotent).
// Uses MySQL ON DUPLICATE KEY UPDATE on the (uid, space_id) unique index.
func (d *VoiceDB) UpsertVoiceContext(uid, spaceID, asrCorrectContext, updatedBy string) error {
	_, err := d.session.InsertBySql(
		"INSERT INTO user_voice_context (uid, space_id, asr_correct_context, updated_by) VALUES (?, ?, ?, ?) "+
			"ON DUPLICATE KEY UPDATE asr_correct_context=VALUES(asr_correct_context), updated_by=VALUES(updated_by)",
		uid, spaceID, asrCorrectContext, updatedBy,
	).Exec()
	return err
}

// DeleteVoiceContext deletes the voice correction context (hard delete, idempotent).
func (d *VoiceDB) DeleteVoiceContext(uid, spaceID string) error {
	_, err := d.session.DeleteFrom("user_voice_context").
		Where("uid=? AND space_id=?", uid, spaceID).Exec()
	return err
}
