package user

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	libcommon "github.com/Mininglamp-OSS/octo-lib/common"
	rd "github.com/go-redis/redis"
)

const scanLoginPendingAuthorizationPrefix = "scanlogin:pending:"

var errScanLoginAuthorizationInvalid = errors.New("scan login authorization is invalid")

type scanLoginAuthorization struct {
	ScannerUID string `json:"scaner"`
	Type       string `json:"type"`
	UUID       string `json:"uuid"`
}

// ScanLoginPendingStore is the minimal Redis capability needed by the QR-code
// module to create and clean up a pending confirmation. The redeemable
// AuthCodeCachePrefix is intentionally not written through this interface.
type ScanLoginPendingStore interface {
	SetAndExpire(key string, value interface{}, expire time.Duration) error
	Del(key string) error
}

func scanLoginPendingAuthorizationKey(authCode string) string {
	return scanLoginPendingAuthorizationPrefix + authCode
}

func scanLoginReadyAuthorizationKey(authCode string) string {
	return fmt.Sprintf("%s%s", libcommon.AuthCodeCachePrefix, authCode)
}

func encodeScanLoginAuthorization(scannerUID, uuid string) (string, error) {
	if scannerUID == "" || uuid == "" {
		return "", errScanLoginAuthorizationInvalid
	}
	payload, err := json.Marshal(scanLoginAuthorization{
		ScannerUID: scannerUID,
		Type:       string(libcommon.AuthCodeTypeScanLogin),
		UUID:       uuid,
	})
	if err != nil {
		return "", fmt.Errorf("encode scan login authorization: %w", err)
	}
	return string(payload), nil
}

// SavePendingScanLoginAuthorization creates a confirmation record that cannot
// be redeemed by login_authcode. Only grant_login can promote it into the
// established AuthCodeCachePrefix namespace.
func SavePendingScanLoginAuthorization(store ScanLoginPendingStore, authCode, scannerUID, uuid string) error {
	if store == nil || authCode == "" {
		return errScanLoginAuthorizationInvalid
	}
	encoded, err := encodeScanLoginAuthorization(scannerUID, uuid)
	if err != nil {
		return err
	}
	return store.SetAndExpire(
		scanLoginPendingAuthorizationKey(authCode),
		encoded,
		ScanLoginConfirmWindow,
	)
}

// DeletePendingScanLoginAuthorization compensates a QR-state write failure.
// The pending record is not redeemable, but retaining an orphan needlessly
// extends the scanner's confirmation surface until TTL expiry.
func DeletePendingScanLoginAuthorization(store ScanLoginPendingStore, authCode string) error {
	if store == nil || authCode == "" {
		return nil
	}
	return store.Del(scanLoginPendingAuthorizationKey(authCode))
}

type scanLoginAuthorizationStore struct {
	client *rd.Client
}

func newScanLoginAuthorizationStore(client *rd.Client) *scanLoginAuthorizationStore {
	return &scanLoginAuthorizationStore{client: client}
}

var promoteScanLoginAuthorizationScript = rd.NewScript(`
local pending = redis.call("GET", KEYS[1])
if not pending or pending ~= ARGV[1] then
  return 0
end
redis.call("SET", KEYS[2], pending, "PX", ARGV[2])
redis.call("DEL", KEYS[1])
return 1
`)

// Promote atomically moves the exact pending payload into the redeemable
// namespace. Comparing the value prevents a stale reader from promoting a
// record that changed between GET and the script.
func (s *scanLoginAuthorizationStore) Promote(authCode, expected string, ttl time.Duration) (bool, error) {
	if s == nil || s.client == nil || authCode == "" || expected == "" || ttl <= 0 {
		return false, errScanLoginAuthorizationInvalid
	}
	result, err := promoteScanLoginAuthorizationScript.Run(
		s.client,
		[]string{
			scanLoginPendingAuthorizationKey(authCode),
			scanLoginReadyAuthorizationKey(authCode),
		},
		expected,
		strconv.FormatInt(ttl.Milliseconds(), 10),
	).Int64()
	if err != nil {
		return false, fmt.Errorf("promote scan login authorization: %w", err)
	}
	return result == 1, nil
}

var consumeScanLoginAuthorizationScript = rd.NewScript(`
local ready = redis.call("GET", KEYS[1])
if not ready or ready ~= ARGV[1] then
  return 0
end
redis.call("DEL", KEYS[1])
return 1
`)

// Consume atomically deletes the exact ready authorization. At most one
// request across all replicas can return true, closing the GET-then-DEL replay
// window in the former handler implementation.
func (s *scanLoginAuthorizationStore) Consume(authCode, expected string) (bool, error) {
	if s == nil || s.client == nil || authCode == "" || expected == "" {
		return false, errScanLoginAuthorizationInvalid
	}
	result, err := consumeScanLoginAuthorizationScript.Run(
		s.client,
		[]string{scanLoginReadyAuthorizationKey(authCode)},
		expected,
	).Int64()
	if err != nil {
		return false, fmt.Errorf("consume scan login authorization: %w", err)
	}
	return result == 1, nil
}
