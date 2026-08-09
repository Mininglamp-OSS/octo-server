package user

import (
	"context"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/Mininglamp-OSS/octo-server/pkg/auth"
	octoredis "github.com/Mininglamp-OSS/octo-server/pkg/redis"
	"github.com/stretchr/testify/require"
)

func TestPasswordMutationAndSessionRevocationIntentAreDurableAndApplied(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	db := NewDB(ctx)
	client := octoredis.NewInstrumentedClient(ctx.GetConfig())
	prefix := "user-revocation-token:" + util.GenerUUID() + ":"
	uidPrefix := "user-revocation-uid:" + util.GenerUUID() + ":"
	store := auth.NewRedisSessionStore(client, prefix, uidPrefix, time.Hour, auth.WithSessionMode(auth.SessionModeRevoke), auth.WithSessionMaxPerUID(4))
	uid := util.GenerUUID()
	token := "token-" + util.GenerUUID()
	password := "hash-before"
	require.NoError(t, db.Insert(&Model{UID: uid, Name: "revocation-user", ShortNo: uid, Password: password, Status: 1}))
	t.Cleanup(func() {
		_, _ = ctx.DB().DeleteFrom("user_session_revocation_intent").Where("uid=?", uid).Exec()
		_, _ = ctx.DB().DeleteFrom("user_session_revocation_version").Where("uid=?", uid).Exec()
		_, _ = ctx.DB().DeleteFrom("user").Where("uid=?", uid).Exec()
		_ = store.DeleteToken(context.Background(), token)
		_ = client.Close()
	})

	fence, err := store.BeginIssue(context.Background(), uid)
	require.NoError(t, err)
	require.NoError(t, store.IssueNewSession(context.Background(), token, auth.TokenInfo{UID: uid, DeviceFlag: int(config.Web)}, fence))
	_, err = auth.NewTokenValidator(store, prefix).Validate(context.Background(), token)
	require.NoError(t, err)

	intent, err := db.updateUserFieldWithSessionRevocation(context.Background(), uid, "password", "hash-after", "password_reset")
	require.NoError(t, err)
	require.NotZero(t, intent.ID)
	require.NotEmpty(t, intent.EventID)
	require.EqualValues(t, 1, intent.EventVersion)

	updated, err := db.QueryByUID(uid)
	require.NoError(t, err)
	require.Equal(t, "hash-after", updated.Password)
	var pending int
	_, err = ctx.DB().Select("COUNT(*)").From("user_session_revocation_intent").Where("id=? AND status=?", intent.ID, sessionRevocationPending).Load(&pending)
	require.NoError(t, err)
	require.Equal(t, 1, pending)

	require.NoError(t, applyAndMarkSessionRevocation(context.Background(), db, store, intent))
	_, err = auth.NewTokenValidator(store, prefix).Validate(context.Background(), token)
	require.Error(t, err)
	var applied int
	_, err = ctx.DB().Select("COUNT(*)").From("user_session_revocation_intent").Where("id=? AND status=?", intent.ID, sessionRevocationApplied).Load(&applied)
	require.NoError(t, err)
	require.Equal(t, 1, applied)

	// An idempotent replay must not rotate away sessions created after the
	// original security event.
	postFence, err := store.BeginIssue(context.Background(), uid)
	require.NoError(t, err)
	postToken := "post-" + util.GenerUUID()
	require.NoError(t, store.IssueNewSession(context.Background(), postToken, auth.TokenInfo{UID: uid, DeviceFlag: int(config.PC)}, postFence))
	t.Cleanup(func() { _ = store.DeleteToken(context.Background(), postToken) })
	require.NoError(t, applyAndMarkSessionRevocation(context.Background(), db, store, intent))
	_, err = auth.NewTokenValidator(store, prefix, auth.WithValidatorClock(func() time.Time { return time.Now() })).Validate(context.Background(), postToken)
	require.NoError(t, err)
}

func TestAccountDisableAndRevocationIntentCommitTogether(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	db := NewDB(ctx)
	uid := util.GenerUUID()
	require.NoError(t, db.Insert(&Model{UID: uid, Name: "disable-user", ShortNo: uid, Status: 1}))
	t.Cleanup(func() {
		_, _ = ctx.DB().DeleteFrom("user_session_revocation_intent").Where("uid=?", uid).Exec()
		_, _ = ctx.DB().DeleteFrom("user_session_revocation_version").Where("uid=?", uid).Exec()
		_, _ = ctx.DB().DeleteFrom("user").Where("uid=?", uid).Exec()
	})

	intent, err := db.updateUserFieldWithSessionRevocation(context.Background(), uid, "status", "0", "account_disable")
	require.NoError(t, err)
	require.NotNil(t, intent)
	updated, err := db.QueryByUID(uid)
	require.NoError(t, err)
	require.Zero(t, updated.Status)

	var pending int
	_, err = ctx.DB().Select("COUNT(*)").From("user_session_revocation_intent").Where("id=? AND status=?", intent.ID, sessionRevocationPending).Load(&pending)
	require.NoError(t, err)
	require.Equal(t, 1, pending)
}

func TestSessionRevocationWorkerAppliesPendingIntentAndStops(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	db := NewDB(ctx)
	client := octoredis.NewInstrumentedClient(ctx.GetConfig())
	prefix := "user-revocation-worker-token:" + util.GenerUUID() + ":"
	uidPrefix := "user-revocation-worker-uid:" + util.GenerUUID() + ":"
	store := auth.NewRedisSessionStore(client, prefix, uidPrefix, time.Hour, auth.WithSessionMode(auth.SessionModeRevoke), auth.WithSessionMaxPerUID(4))
	uid := util.GenerUUID()
	token := "token-" + util.GenerUUID()
	require.NoError(t, db.Insert(&Model{UID: uid, Name: "worker-user", ShortNo: uid, Password: "before", Status: 1}))
	t.Cleanup(func() {
		_, _ = ctx.DB().DeleteFrom("user_session_revocation_intent").Where("uid=?", uid).Exec()
		_, _ = ctx.DB().DeleteFrom("user_session_revocation_version").Where("uid=?", uid).Exec()
		_, _ = ctx.DB().DeleteFrom("user").Where("uid=?", uid).Exec()
		_ = store.DeleteToken(context.Background(), token)
		_ = client.Close()
	})

	fence, err := store.BeginIssue(context.Background(), uid)
	require.NoError(t, err)
	require.NoError(t, store.IssueNewSession(context.Background(), token, auth.TokenInfo{UID: uid, DeviceFlag: int(config.Web)}, fence))
	intent, err := db.updateUserFieldWithSessionRevocation(context.Background(), uid, "password", "after", "password_reset")
	require.NoError(t, err)

	worker := &User{db: db, sessionStore: store, revocationWorkerOwner: "test-worker"}
	workerCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		worker.runSessionRevocationWorker(workerCtx, 10*time.Millisecond)
	}()
	require.Eventually(t, func() bool {
		var applied int
		_, queryErr := ctx.DB().Select("COUNT(*)").From("user_session_revocation_intent").Where("id=? AND status=?", intent.ID, sessionRevocationApplied).Load(&applied)
		return queryErr == nil && applied == 1
	}, 2*time.Second, 20*time.Millisecond)
	_, err = auth.NewTokenValidator(store, prefix).Validate(context.Background(), token)
	require.Error(t, err)

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("session revocation worker did not stop after cancellation")
	}
}
