package botfather

import (
	"errors"
	"fmt"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/Mininglamp-OSS/octo-server/modules/base/app"
	"github.com/gocraft/dbr/v2"
	"github.com/gocraft/dbr/v2/dialect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCreateBotSpaceBindingRejectsUntrustedSelector(t *testing.T) {
	ctx := newCreateBotTestContext(t)
	defer func() { require.NoError(t, testutil.CleanAllTables(ctx)) }()

	tests := []struct {
		name     string
		selector string
		seed     func(t *testing.T, ctx *config.Context, creatorUID string)
	}{
		{
			name:     "foreign Space",
			selector: "space_synthetic_foreign",
			seed: func(t *testing.T, ctx *config.Context, creatorUID string) {
				seedCreateBotSpace(t, ctx, "space_synthetic_owned", 1)
				seedCreateBotMembership(t, ctx, "space_synthetic_owned", creatorUID, 1)
				seedCreateBotSpace(t, ctx, "space_synthetic_foreign", 1)
			},
		},
		{
			name:     "nonexistent Space",
			selector: "space_synthetic_missing",
		},
		{
			name:     "inactive Space",
			selector: "space_synthetic_inactive",
			seed: func(t *testing.T, ctx *config.Context, creatorUID string) {
				seedCreateBotSpace(t, ctx, "space_synthetic_inactive", 0)
				seedCreateBotMembership(t, ctx, "space_synthetic_inactive", creatorUID, 1)
			},
		},
		{
			name:     "inactive creator",
			selector: "space_synthetic_creator_disabled",
			seed: func(t *testing.T, ctx *config.Context, creatorUID string) {
				_, err := ctx.DB().Update("user").Set("status", 0).Where("uid=?", creatorUID).Exec()
				require.NoError(t, err)
				seedCreateBotSpace(t, ctx, "space_synthetic_creator_disabled", 1)
				seedCreateBotMembership(t, ctx, "space_synthetic_creator_disabled", creatorUID, 1)
			},
		},
		{
			name:     "removed membership",
			selector: "space_synthetic_removed",
			seed: func(t *testing.T, ctx *config.Context, creatorUID string) {
				seedCreateBotSpace(t, ctx, "space_synthetic_removed", 1)
				seedCreateBotMembership(t, ctx, "space_synthetic_removed", creatorUID, 0)
			},
		},
	}

	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.NoError(t, testutil.CleanAllTables(ctx))
			creatorUID := fmt.Sprintf("user_space_guard_%d", i)
			botID := fmt.Sprintf("bot_space_guard_%d", i)
			seedCreateBotUser(t, ctx, creatorUID, 1)
			if tt.seed != nil {
				tt.seed(t, ctx, creatorUID)
			}

			cleanup := setSpaceIDFromPayload(creatorUID, tt.selector)
			err := newCommandHandler(ctx).createBot(
				creatorUID,
				creatorUID,
				"Synthetic Bot",
				botID,
				"bf_test_space_guard",
			)
			cleanup()

			assert.Error(t, err)
			assertNoCreateBotArtifacts(t, ctx, botID)
		})
	}
}

func TestCreateBotSpaceBindingMissingSelector(t *testing.T) {
	ctx := newCreateBotTestContext(t)
	defer func() { require.NoError(t, testutil.CleanAllTables(ctx)) }()

	tests := []struct {
		name       string
		spaceCount int
		wantErr    bool
	}{
		{name: "zero active Spaces", spaceCount: 0, wantErr: true},
		{name: "exactly one active Space", spaceCount: 1, wantErr: false},
		{name: "multiple active Spaces", spaceCount: 2, wantErr: true},
	}

	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.NoError(t, testutil.CleanAllTables(ctx))
			creatorUID := fmt.Sprintf("user_space_fallback_%d", i)
			botID := fmt.Sprintf("bot_space_fallback_%d", i)
			seedCreateBotUser(t, ctx, creatorUID, 1)
			for spaceIndex := 0; spaceIndex < tt.spaceCount; spaceIndex++ {
				spaceID := fmt.Sprintf("space_synthetic_%d_%d", i, spaceIndex)
				seedCreateBotSpace(t, ctx, spaceID, 1)
				seedCreateBotMembership(t, ctx, spaceID, creatorUID, 1)
			}

			err := newCommandHandler(ctx).createBot(
				creatorUID,
				creatorUID,
				"Synthetic Bot",
				botID,
				"bf_test_space_fallback",
			)

			if tt.wantErr {
				assert.Error(t, err)
				assertNoCreateBotArtifacts(t, ctx, botID)
				return
			}
			require.NoError(t, err)
			assertCreateBotArtifacts(t, ctx, botID, "space_synthetic_1_0")
		})
	}
}

func TestCreateBotSpaceBindingAuthorizedSelector(t *testing.T) {
	ctx := newCreateBotTestContext(t)
	defer func() { require.NoError(t, testutil.CleanAllTables(ctx)) }()
	require.NoError(t, testutil.CleanAllTables(ctx))

	const (
		creatorUID  = "user_space_authorized"
		botID       = "bot_space_authorized"
		targetSpace = "space_synthetic_target"
		otherSpace  = "space_synthetic_other"
	)
	seedCreateBotUser(t, ctx, creatorUID, 1)
	seedCreateBotSpace(t, ctx, targetSpace, 1)
	seedCreateBotMembership(t, ctx, targetSpace, creatorUID, 1)
	seedCreateBotSpace(t, ctx, otherSpace, 1)
	seedCreateBotMembership(t, ctx, otherSpace, creatorUID, 1)

	cleanup := setSpaceIDFromPayload(creatorUID, targetSpace)
	err := newCommandHandler(ctx).createBot(
		creatorUID,
		creatorUID,
		"Synthetic Bot",
		botID,
		"bf_test_space_authorized",
	)
	cleanup()

	require.NoError(t, err)
	assertCreateBotArtifacts(t, ctx, botID, targetSpace)
	assertCreateBotMembershipCount(t, ctx, botID, otherSpace, 0)
}

func TestCreateBotSpaceBindingRechecksAuthorityBeforeCommit(t *testing.T) {
	ctx := newCreateBotTestContext(t)
	defer func() { require.NoError(t, testutil.CleanAllTables(ctx)) }()

	tests := []struct {
		name   string
		mutate func(t *testing.T, ctx *config.Context, creatorUID, spaceID string)
	}{
		{
			name: "creator disabled after preflight",
			mutate: func(t *testing.T, ctx *config.Context, creatorUID, _ string) {
				_, err := ctx.DB().Update("user").Set("status", 0).
					Where("uid=?", creatorUID).Exec()
				require.NoError(t, err)
			},
		},
		{
			name: "creator destroyed after preflight",
			mutate: func(t *testing.T, ctx *config.Context, creatorUID, _ string) {
				_, err := ctx.DB().Update("user").Set("is_destroy", 2).
					Where("uid=?", creatorUID).Exec()
				require.NoError(t, err)
			},
		},
		{
			name: "creator deleted after preflight",
			mutate: func(t *testing.T, ctx *config.Context, creatorUID, _ string) {
				_, err := ctx.DB().DeleteFrom("user").Where("uid=?", creatorUID).Exec()
				require.NoError(t, err)
			},
		},
		{
			name: "membership removed after preflight",
			mutate: func(t *testing.T, ctx *config.Context, creatorUID, spaceID string) {
				_, err := ctx.DB().Update("space_member").Set("status", 0).
					Where("space_id=? AND uid=?", spaceID, creatorUID).Exec()
				require.NoError(t, err)
			},
		},
		{
			name: "membership deleted after preflight",
			mutate: func(t *testing.T, ctx *config.Context, creatorUID, spaceID string) {
				_, err := ctx.DB().DeleteFrom("space_member").
					Where("space_id=? AND uid=?", spaceID, creatorUID).Exec()
				require.NoError(t, err)
			},
		},
		{
			name: "Space disabled after preflight",
			mutate: func(t *testing.T, ctx *config.Context, _, spaceID string) {
				_, err := ctx.DB().Update("space").Set("status", 0).
					Where("space_id=?", spaceID).Exec()
				require.NoError(t, err)
			},
		},
		{
			name: "Space deleted after preflight",
			mutate: func(t *testing.T, ctx *config.Context, _, spaceID string) {
				_, err := ctx.DB().DeleteFrom("space").Where("space_id=?", spaceID).Exec()
				require.NoError(t, err)
			},
		},
	}

	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.NoError(t, testutil.CleanAllTables(ctx))
			creatorUID := fmt.Sprintf("user_space_race_%d", i)
			botID := fmt.Sprintf("bot_space_race_%d", i)
			spaceID := fmt.Sprintf("space_synthetic_race_%d", i)
			seedCreateBotUser(t, ctx, creatorUID, 1)
			seedCreateBotSpace(t, ctx, spaceID, 1)
			seedCreateBotMembership(t, ctx, spaceID, creatorUID, 1)

			handler := newCommandHandler(ctx)
			created := make(chan struct{})
			resume := make(chan struct{})
			handler.appService = &blockingCreateAppService{
				IService: handler.appService,
				created:  created,
				resume:   resume,
			}
			cleanupSelector := setSpaceIDFromPayload(creatorUID, spaceID)
			result := make(chan error, 1)
			go func() {
				result <- handler.createBot(
					creatorUID,
					creatorUID,
					"Synthetic Bot",
					botID,
					"bf_test_space_race",
				)
			}()

			<-created
			tt.mutate(t, ctx, creatorUID, spaceID)
			close(resume)
			err := <-result
			cleanupSelector()

			assert.Error(t, err)
			assertNoCreateBotArtifacts(t, ctx, botID)
		})
	}
}

func TestCreateBotSpaceBindingInsertFailureCompensatesCoreArtifacts(t *testing.T) {
	ctx := newCreateBotTestContext(t)
	defer func() { require.NoError(t, testutil.CleanAllTables(ctx)) }()
	require.NoError(t, testutil.CleanAllTables(ctx))

	const (
		creatorUID = "user_space_insert_failure"
		botID      = "bot_space_insert_failure"
		spaceID    = "space_synthetic_insert_failure"
		trigger    = "botfather_test_reject_bot_membership"
	)
	seedCreateBotUser(t, ctx, creatorUID, 1)
	seedCreateBotSpace(t, ctx, spaceID, 1)
	seedCreateBotMembership(t, ctx, spaceID, creatorUID, 1)

	_, err := ctx.DB().Exec("DROP TRIGGER IF EXISTS " + trigger)
	require.NoError(t, err)
	_, err = ctx.DB().Exec(`
		CREATE TRIGGER ` + trigger + `
		BEFORE INSERT ON space_member
		FOR EACH ROW
		BEGIN
			IF NEW.uid = '` + botID + `' THEN
				SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'synthetic Bot membership insert failure';
			END IF;
		END`)
	require.NoError(t, err)
	defer func() {
		_, dropErr := ctx.DB().Exec("DROP TRIGGER IF EXISTS " + trigger)
		require.NoError(t, dropErr)
	}()

	cleanupSelector := setSpaceIDFromPayload(creatorUID, spaceID)
	err = newCommandHandler(ctx).createBot(
		creatorUID,
		creatorUID,
		"Synthetic Bot",
		botID,
		"bf_test_space_insert_failure",
	)
	cleanupSelector()

	assert.Error(t, err)
	assertNoCreateBotArtifacts(t, ctx, botID)
}

func TestCreateBotCoreFailureLeavesNoGeneratedArtifacts(t *testing.T) {
	ctx := newCreateBotTestContext(t)
	defer func() { require.NoError(t, testutil.CleanAllTables(ctx)) }()
	require.NoError(t, testutil.CleanAllTables(ctx))

	const (
		creatorUID = "user_core_insert_failure"
		spaceID    = "space_synthetic_core_failure"
		trigger    = "botfather_test_reject_robot_insert"
	)
	seedCreateBotUser(t, ctx, creatorUID, 1)
	seedCreateBotSpace(t, ctx, spaceID, 1)
	seedCreateBotMembership(t, ctx, spaceID, creatorUID, 1)

	_, err := ctx.DB().Exec("DROP TRIGGER IF EXISTS " + trigger)
	require.NoError(t, err)
	_, err = ctx.DB().Exec(`
		CREATE TRIGGER ` + trigger + `
		BEFORE INSERT ON robot
		FOR EACH ROW
		SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'synthetic robot insert failure'`)
	require.NoError(t, err)
	defer func() {
		_, dropErr := ctx.DB().Exec("DROP TRIGGER IF EXISTS " + trigger)
		require.NoError(t, dropErr)
	}()

	cleanupSelector := setSpaceIDFromPayload(creatorUID, spaceID)
	err = newCommandHandler(ctx).createBot(
		creatorUID,
		creatorUID,
		"Synthetic Bot",
		"",
		"bf_test_core_insert_failure",
	)
	cleanupSelector()

	assert.ErrorIs(t, err, errCreateBotPersistenceFailed)
	assertCreateBotCount(t, ctx, "SELECT COUNT(*) FROM app WHERE app_id<>?", "synthetic_nonexistent", 0)
	assertCreateBotCount(t, ctx, "SELECT COUNT(*) FROM robot WHERE creator_uid=?", creatorUID, 0)
	assertCreateBotCount(t, ctx, "SELECT COUNT(*) FROM user WHERE robot=1 AND uid<>?", creatorUID, 0)
	assertCreateBotCount(t, ctx, "SELECT COUNT(*) FROM friend WHERE uid=? OR to_uid=?", creatorUID, 0, creatorUID)
	assertCreateBotCount(t, ctx, "SELECT COUNT(*) FROM space_member WHERE uid<>?", creatorUID, 0)
}

func TestResolveAuthorizedCreationSpaceQueryFailureFailsClosed(t *testing.T) {
	tests := []struct {
		name       string
		selector   string
		queryMatch string
	}{
		{
			name:       "explicit selector",
			selector:   "space_synthetic_query_error",
			queryMatch: `(?s)SELECT sm\.space_id.*AND sm\.space_id = 'space_synthetic_query_error'`,
		},
		{
			name:       "missing selector",
			queryMatch: `(?s)SELECT sm\.space_id.*LIMIT 2`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rawDB, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer rawDB.Close()
			conn := &dbr.Connection{
				DB:            rawDB,
				EventReceiver: &dbr.NullEventReceiver{},
				Dialect:       dialect.MySQL,
			}
			db := &botfatherDB{session: conn.NewSession(nil)}
			mock.ExpectQuery(tt.queryMatch).WillReturnError(errors.New("synthetic database failure"))

			spaceID, resolveErr := db.resolveAuthorizedCreationSpace("user_synthetic_query_error", tt.selector)

			assert.Empty(t, spaceID)
			assert.Error(t, resolveErr)
			assert.NotErrorIs(t, resolveErr, errCreateBotSpaceDenied)
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestDeleteCreatedBotArtifactsFallsBackToCredentialRevocation(t *testing.T) {
	rawDB, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer rawDB.Close()
	conn := &dbr.Connection{
		DB:            rawDB,
		EventReceiver: &dbr.NullEventReceiver{},
		Dialect:       dialect.MySQL,
	}
	db := &botfatherDB{session: conn.NewSession(nil)}
	deleteErr := errors.New("synthetic cleanup delete failure")
	const (
		creatorUID = "user_synthetic_cleanup_creator"
		botID      = "bot_synthetic_cleanup"
	)

	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(
		"DELETE FROM `friend` WHERE ((uid='user_synthetic_cleanup_creator' AND to_uid='bot_synthetic_cleanup') OR (uid='bot_synthetic_cleanup' AND to_uid='user_synthetic_cleanup_creator'))",
	)).WillReturnError(deleteErr)
	mock.ExpectRollback()
	mock.ExpectExec("UPDATE robot SET status=0").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("UPDATE user SET status=0").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("UPDATE space_member SET status=0").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("UPDATE app SET status=0").WillReturnResult(sqlmock.NewResult(0, 1))

	cleanupErr := db.deleteCreatedBotArtifacts(creatorUID, botID)

	assert.ErrorIs(t, cleanupErr, deleteErr)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestDeleteCreatedBotArtifactsDeletesOnlyCreatorFriendPairs(t *testing.T) {
	rawDB, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer rawDB.Close()
	conn := &dbr.Connection{
		DB:            rawDB,
		EventReceiver: &dbr.NullEventReceiver{},
		Dialect:       dialect.MySQL,
	}
	db := &botfatherDB{session: conn.NewSession(nil)}
	const (
		creatorUID = "user_synthetic_cleanup_creator"
		botID      = "bot_synthetic_cleanup"
	)

	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(
		"DELETE FROM `friend` WHERE ((uid='user_synthetic_cleanup_creator' AND to_uid='bot_synthetic_cleanup') OR (uid='bot_synthetic_cleanup' AND to_uid='user_synthetic_cleanup_creator'))",
	)).WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectExec("DELETE FROM .*space_member.*").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("DELETE FROM .*user.*").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("DELETE FROM .*robot.*").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("DELETE FROM .*app.*").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	require.NoError(t, db.deleteCreatedBotArtifacts(creatorUID, botID))
	require.NoError(t, mock.ExpectationsWereMet())
}

type blockingCreateAppService struct {
	app.IService
	created chan struct{}
	resume  chan struct{}
}

func (s *blockingCreateAppService) CreateApp(req app.Req) (*app.Resp, error) {
	resp, err := s.IService.CreateApp(req)
	if err != nil {
		return nil, err
	}
	close(s.created)
	<-s.resume
	return resp, nil
}

func newCreateBotTestContext(t *testing.T) *config.Context {
	t.Helper()
	t.Setenv("OCTO_MASTER_KEY", "0123456789abcdef0123456789abcdef")
	_, ctx := testutil.NewTestServer()
	return ctx
}

func seedCreateBotUser(t *testing.T, ctx *config.Context, uid string, status int) {
	t.Helper()
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO user (uid, name, username, short_no, status) VALUES (?, ?, ?, ?, ?)",
		uid,
		"Synthetic User",
		uid,
		"sn_"+util.GenerUUID()[:12],
		status,
	).Exec()
	require.NoError(t, err)
}

func seedCreateBotSpace(t *testing.T, ctx *config.Context, spaceID string, status int) {
	t.Helper()
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO space (space_id, name, status) VALUES (?, ?, ?)",
		spaceID,
		"Synthetic Space",
		status,
	).Exec()
	require.NoError(t, err)
}

func seedCreateBotMembership(t *testing.T, ctx *config.Context, spaceID, uid string, status int) {
	t.Helper()
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO space_member (space_id, uid, role, status) VALUES (?, ?, 0, ?)",
		spaceID,
		uid,
		status,
	).Exec()
	require.NoError(t, err)
}

func assertNoCreateBotArtifacts(t *testing.T, ctx *config.Context, botID string) {
	t.Helper()
	assertCreateBotCount(t, ctx, "SELECT COUNT(*) FROM app WHERE app_id=?", botID, 0)
	assertCreateBotCount(t, ctx, "SELECT COUNT(*) FROM robot WHERE robot_id=?", botID, 0)
	assertCreateBotCount(t, ctx, "SELECT COUNT(*) FROM user WHERE uid=?", botID, 0)
	assertCreateBotCount(t, ctx, "SELECT COUNT(*) FROM friend WHERE uid=? OR to_uid=?", botID, 0, botID)
	assertCreateBotCount(t, ctx, "SELECT COUNT(*) FROM space_member WHERE uid=?", botID, 0)
}

func assertCreateBotArtifacts(t *testing.T, ctx *config.Context, botID, spaceID string) {
	t.Helper()
	assertCreateBotCount(t, ctx, "SELECT COUNT(*) FROM app WHERE app_id=? AND status=1", botID, 1)
	assertCreateBotCount(t, ctx, "SELECT COUNT(*) FROM robot WHERE robot_id=? AND status=1", botID, 1)
	assertCreateBotCount(t, ctx, "SELECT COUNT(*) FROM user WHERE uid=? AND status=1 AND robot=1", botID, 1)
	assertCreateBotCount(t, ctx, "SELECT COUNT(*) FROM friend WHERE (uid=? OR to_uid=?) AND is_deleted=0", botID, 2, botID)
	assertCreateBotMembershipCount(t, ctx, botID, spaceID, 1)
	assertCreateBotCount(t, ctx, "SELECT COUNT(*) FROM space_member WHERE uid=?", botID, 1)
}

func assertCreateBotMembershipCount(t *testing.T, ctx *config.Context, botID, spaceID string, want int) {
	t.Helper()
	assertCreateBotCount(
		t,
		ctx,
		"SELECT COUNT(*) FROM space_member WHERE uid=? AND space_id=? AND status=1",
		botID,
		want,
		spaceID,
	)
}

func assertCreateBotCount(
	t *testing.T,
	ctx *config.Context,
	query string,
	firstArg interface{},
	want int,
	rest ...interface{},
) {
	t.Helper()
	args := append([]interface{}{firstArg}, rest...)
	var got int
	require.NoError(t, ctx.DB().SelectBySql(query, args...).LoadOne(&got))
	assert.Equal(t, want, got)
}
