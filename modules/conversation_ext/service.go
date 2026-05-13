package conversation_ext

import (
	"errors"
	"fmt"
	"strings"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/gocraft/dbr/v2"
)

// target_type constants — kept package-private; callers use the Service API.
const (
	targetTypeDM     uint8 = 1
	targetTypeGroup  uint8 = 2
	targetTypeThread uint8 = 5
)

// threadSeparator is the fixed four-underscore delimiter used in thread
// channel IDs: "{groupNo}____{shortID}".
const threadSeparator = "____"

// Service encapsulates composite operations on user_conversation_ext that
// require a single transaction boundary.  It intentionally avoids importing
// modules/group, modules/user, or modules/thread to prevent circular
// dependencies.
type Service struct {
	db      *DB
	session *dbr.Session
	log.Log
}

// NewService creates a Service.
func NewService(ctx *config.Context) *Service {
	return &Service{
		db:      NewDB(ctx),
		session: ctx.DB(),
		Log:     log.NewTLog("ConvExtService"),
	}
}

// ---------------------------------------------------------------------------
// Input validation helpers
// ---------------------------------------------------------------------------

func validateBase(uid, spaceID string) error {
	if uid == "" {
		return errors.New("uid must not be empty")
	}
	if spaceID == "" {
		return errors.New("space_id must not be empty")
	}
	return nil
}

// parseThreadChannelID splits a thread channel ID of the form
// "{groupNo}____{shortID}" and returns groupNo, shortID.
// Returns an error if the format is invalid.
func parseThreadChannelID(threadChannelID string) (groupNo, shortID string, err error) {
	parts := strings.SplitN(threadChannelID, threadSeparator, 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("thread_channel_id %q is invalid: expected format {groupNo}____{shortID}", threadChannelID)
	}
	return parts[0], parts[1], nil
}

// escapeLike escapes LIKE special characters for use with ESCAPE '|'.
// The pipe character is chosen as the escape character because it never
// appears in snowflake IDs or our thread channel IDs, avoiding the
// double-backslash quoting problem when passing '\' through the Go MySQL driver.
func escapeLike(s string) string {
	s = strings.ReplaceAll(s, `|`, `||`)
	s = strings.ReplaceAll(s, `%`, `|%`)
	s = strings.ReplaceAll(s, `_`, `|_`)
	return s
}

// ---------------------------------------------------------------------------
// FollowChannel — clear group-blacklist flag (re-follow a previously unfollowed group)
// ---------------------------------------------------------------------------

// FollowChannel marks the group as followed (group_unfollowed=0) for the given
// user and space.  If no ext row exists it is created with the default values.
func (s *Service) FollowChannel(uid, spaceID, groupNo string) error {
	if err := validateBase(uid, spaceID); err != nil {
		return err
	}
	if groupNo == "" {
		return errors.New("group_no must not be empty")
	}
	zero := int8(0)
	if err := s.db.Upsert(uid, spaceID, targetTypeGroup, groupNo, ConvExtFields{
		GroupUnfollowed: &zero,
	}); err != nil {
		return fmt.Errorf("FollowChannel upsert: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// UnfollowChannel — blacklist a group + cascade-delete its thread ext rows
// ---------------------------------------------------------------------------

// UnfollowChannel marks the group as unfollowed (group_unfollowed=1) and, in
// the same transaction, deletes all thread (target_type=5) ext rows whose
// target_id starts with "{groupNo}____" for this user+space.
func (s *Service) UnfollowChannel(uid, spaceID, groupNo string) error {
	if err := validateBase(uid, spaceID); err != nil {
		return err
	}
	if groupNo == "" {
		return errors.New("group_no must not be empty")
	}

	tx, err := s.session.Begin()
	if err != nil {
		return fmt.Errorf("UnfollowChannel begin tx: %w", err)
	}
	defer tx.RollbackUnlessCommitted()

	// 1. Upsert the group row with group_unfollowed=1.
	one := int8(1)
	if err := upsertTx(tx, uid, spaceID, targetTypeGroup, groupNo, ConvExtFields{
		GroupUnfollowed: &one,
	}); err != nil {
		return fmt.Errorf("UnfollowChannel upsert group: %w", err)
	}

	// 2. Delete all thread ext rows for this group.
	//    Pattern: target_id LIKE '{escaped_groupNo}____%' ESCAPE '\'
	//    The separator is exactly 4 underscores; escaping groupNo's own
	//    underscores ensures we don't inadvertently match other groups.
	prefix := escapeLike(groupNo) + threadSeparator + "%"
	if _, err := tx.DeleteBySql(
		"DELETE FROM "+table+
			" WHERE uid=? AND space_id=? AND target_type=? AND target_id LIKE ? ESCAPE '|'",
		uid, spaceID, targetTypeThread, prefix,
	).Exec(); err != nil {
		return fmt.Errorf("UnfollowChannel delete threads: %w", err)
	}

	return tx.Commit()
}

// ---------------------------------------------------------------------------
// FollowThread — re-follow parent group (implicit) + upsert thread ext row
// ---------------------------------------------------------------------------

// FollowThread creates (or ensures) an ext row for the given thread channel,
// and simultaneously clears the parent group's unfollowed flag so that
// following a specific thread implicitly re-follows its parent group.
//
// threadChannelID must have the format "{groupNo}____{shortID}".
func (s *Service) FollowThread(uid, spaceID, threadChannelID string) error {
	if err := validateBase(uid, spaceID); err != nil {
		return err
	}
	groupNo, _, err := parseThreadChannelID(threadChannelID)
	if err != nil {
		return err
	}

	tx, err := s.session.Begin()
	if err != nil {
		return fmt.Errorf("FollowThread begin tx: %w", err)
	}
	defer tx.RollbackUnlessCommitted()

	// 1. Clear parent group's unfollowed flag.
	zero := int8(0)
	if err := upsertTx(tx, uid, spaceID, targetTypeGroup, groupNo, ConvExtFields{
		GroupUnfollowed: &zero,
	}); err != nil {
		return fmt.Errorf("FollowThread clear parent group: %w", err)
	}

	// 2. Upsert thread ext row (no additional fields — default values suffice).
	if err := upsertTx(tx, uid, spaceID, targetTypeThread, threadChannelID, ConvExtFields{}); err != nil {
		return fmt.Errorf("FollowThread upsert thread: %w", err)
	}

	return tx.Commit()
}

// ---------------------------------------------------------------------------
// UnfollowThread — delete thread ext row only
// ---------------------------------------------------------------------------

// UnfollowThread removes the ext row for the given thread channel.
// It does NOT touch the parent group's unfollowed flag.
//
// threadChannelID must have the format "{groupNo}____{shortID}".
func (s *Service) UnfollowThread(uid, spaceID, threadChannelID string) error {
	if err := validateBase(uid, spaceID); err != nil {
		return err
	}
	if _, _, err := parseThreadChannelID(threadChannelID); err != nil {
		return err
	}
	if err := s.db.Delete(uid, spaceID, targetTypeThread, threadChannelID); err != nil {
		return fmt.Errorf("UnfollowThread delete: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// FollowDM — upsert ext row with followed_dm=1
// ---------------------------------------------------------------------------

// FollowDM marks the DM conversation with peerUID as followed (followed_dm=1).
// If categoryID is non-nil the DM is placed into that category.
func (s *Service) FollowDM(uid, spaceID, peerUID string, categoryID *int64) error {
	if err := validateBase(uid, spaceID); err != nil {
		return err
	}
	if peerUID == "" {
		return errors.New("peer_uid must not be empty")
	}
	one := int8(1)
	fields := ConvExtFields{
		FollowedDM:   &one,
		DMCategoryID: categoryID,
	}
	if err := s.db.Upsert(uid, spaceID, targetTypeDM, peerUID, fields); err != nil {
		return fmt.Errorf("FollowDM upsert: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// UnfollowDM — delete ext row
// ---------------------------------------------------------------------------

// UnfollowDM removes the ext row for the DM conversation with peerUID.
// Deleting is cleaner than setting followed_dm=0 because it frees the row
// and avoids stale dm_category_id values.
func (s *Service) UnfollowDM(uid, spaceID, peerUID string) error {
	if err := validateBase(uid, spaceID); err != nil {
		return err
	}
	if peerUID == "" {
		return errors.New("peer_uid must not be empty")
	}
	if err := s.db.Delete(uid, spaceID, targetTypeDM, peerUID); err != nil {
		return fmt.Errorf("UnfollowDM delete: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Internal transaction helpers
// ---------------------------------------------------------------------------

// upsertTx is the transaction-scoped counterpart of DB.Upsert.
// It reuses buildUpsertParts so the SQL construction logic stays in one place.
func upsertTx(tx *dbr.Tx, uid, spaceID string, targetType uint8, targetID string, fields ConvExtFields) error {
	extraCols, extraVals, setClauses, setArgs := buildUpsertParts(fields)

	if len(setClauses) == 0 {
		_, err := tx.InsertBySql(
			"INSERT IGNORE INTO "+table+
				" (uid, space_id, target_type, target_id) VALUES (?, ?, ?, ?)",
			uid, spaceID, targetType, targetID,
		).Exec()
		return err
	}

	colsSQL := "uid, space_id, target_type, target_id"
	if len(extraCols) > 0 {
		colsSQL += ", " + strings.Join(extraCols, ", ")
	}
	placeholders := "?, ?, ?, ?"
	if len(extraVals) > 0 {
		placeholders += strings.Repeat(", ?", len(extraVals))
	}
	setSQL := strings.Join(setClauses, ", ")
	query := fmt.Sprintf(
		"INSERT INTO %s (%s) VALUES (%s) ON DUPLICATE KEY UPDATE %s",
		table, colsSQL, placeholders, setSQL,
	)
	insertArgs := append([]interface{}{uid, spaceID, targetType, targetID}, extraVals...)
	insertArgs = append(insertArgs, setArgs...)
	_, err := tx.InsertBySql(query, insertArgs...).Exec()
	return err
}
