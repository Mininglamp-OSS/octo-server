package user

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	rd "github.com/go-redis/redis"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newScanLoginAuthorizationStoreForTest(t *testing.T) *scanLoginAuthorizationStore {
	t.Helper()
	client := rd.NewClient(&rd.Options{
		Addr:         "127.0.0.1:6399",
		MaxRetries:   1,
		DialTimeout:  2 * time.Second,
		ReadTimeout:  2 * time.Second,
		WriteTimeout: 2 * time.Second,
	})
	if err := client.Ping().Err(); err != nil {
		_ = client.Close()
		t.Skipf("test Redis unavailable: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return newScanLoginAuthorizationStore(client)
}

func TestScanLoginAuthorization_PromoteAndConsumeAreAtomic(t *testing.T) {
	store := newScanLoginAuthorizationStoreForTest(t)
	authCode := "scan-auth-" + time.Now().Format("150405.000000000")
	pendingKey := scanLoginPendingAuthorizationKey(authCode)
	readyKey := scanLoginReadyAuthorizationKey(authCode)
	t.Cleanup(func() {
		_ = store.client.Del(pendingKey, readyKey).Err()
	})

	expected, err := encodeScanLoginAuthorization("scanner-1", "uuid-1")
	require.NoError(t, err)
	require.NoError(t, store.client.Set(pendingKey, expected, time.Minute).Err())

	wrong, err := store.Promote(authCode, expected+"-wrong", time.Minute)
	require.NoError(t, err)
	assert.False(t, wrong)
	assert.Equal(t, expected, store.client.Get(pendingKey).Val())
	assert.Equal(t, rd.Nil, store.client.Get(readyKey).Err())

	const workers = 32
	var promoted int32
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			ok, promoteErr := store.Promote(authCode, expected, time.Minute)
			if promoteErr != nil {
				t.Errorf("promote: %v", promoteErr)
				return
			}
			if ok {
				atomic.AddInt32(&promoted, 1)
			}
		}()
	}
	wg.Wait()
	assert.Equal(t, int32(1), promoted)
	assert.Equal(t, rd.Nil, store.client.Get(pendingKey).Err())
	assert.Equal(t, expected, store.client.Get(readyKey).Val())

	consumedWrong, err := store.Consume(authCode, expected+"-wrong")
	require.NoError(t, err)
	assert.False(t, consumedWrong)
	assert.Equal(t, expected, store.client.Get(readyKey).Val())

	var consumed int32
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			ok, consumeErr := store.Consume(authCode, expected)
			if consumeErr != nil {
				t.Errorf("consume: %v", consumeErr)
				return
			}
			if ok {
				atomic.AddInt32(&consumed, 1)
			}
		}()
	}
	wg.Wait()
	assert.Equal(t, int32(1), consumed)
	assert.Equal(t, rd.Nil, store.client.Get(readyKey).Err())
}
