package botfather

import (
	"fmt"
	"strings"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
)

// clientIDBotFather labels `uk_` keys minted by botfather's own /quickstart
// flow. The integration (Octo-link) module passes other client_id labels
// (e.g. the external application id) into the same service, so every `uk_` is
// organised along the (uid, space_id, client_id) dimension.
const clientIDBotFather = "botfather"

// user_api_key.status values.
const (
	userAPIKeyStatusRevoked = 0
	userAPIKeyStatusActive  = 1
)

// UserAPIKey is the resolved, active `uk_` record exposed to callers. It
// deliberately omits storage-only columns (hash/cipher placeholders, audit
// fields) so consumers only see what they need.
type UserAPIKey struct {
	ID       int64
	UID      string
	SpaceID  string
	ClientID string
	APIKey   string
}

// UserAPIKeyService owns the get-or-create and authenticate semantics for
// `uk_` user API keys. botfather (/quickstart) and the integration module
// (OIDC exchange) share one implementation so both stay consistent on the
// (uid, space_id, client_id) dimension and the plaintext-echo idempotency
// contract.
type UserAPIKeyService interface {
	// GetOrCreate returns the active plaintext `uk_` for (uid, spaceID,
	// clientID), creating one when none exists. Repeated calls return the
	// same key (idempotent plaintext echo). A blank clientID defaults to
	// the botfather client.
	GetOrCreate(uid, spaceID, clientID string) (string, error)
	// AuthByKey resolves an active key by its plaintext value. It returns
	// (nil, nil) when the key is unknown or revoked.
	AuthByKey(plaintext string) (*UserAPIKey, error)
}

type userAPIKeyService struct {
	db *botfatherDB
	log.Log
}

// NewUserAPIKeyService builds a UserAPIKeyService. botfather uses it for
// /quickstart; the integration module uses it to mint `uk_` for external
// clients.
func NewUserAPIKeyService(ctx *config.Context) UserAPIKeyService {
	return &userAPIKeyService{
		db:  newBotfatherDB(ctx),
		Log: log.NewTLog("UserAPIKeyService"),
	}
}

func (s *userAPIKeyService) GetOrCreate(uid, spaceID, clientID string) (string, error) {
	if strings.TrimSpace(clientID) == "" {
		clientID = clientIDBotFather
	}

	existing, err := s.db.queryActiveUserAPIKey(uid, spaceID, clientID)
	if err != nil {
		return "", fmt.Errorf("query user api key: %w", err)
	}
	if existing != nil {
		return existing.APIKey, nil
	}

	apiKey, err := generateUserAPIKey()
	if err != nil {
		return "", fmt.Errorf("generate user api key: %w", err)
	}

	if err := s.db.insertUserAPIKey(uid, apiKey, spaceID, clientID); err != nil {
		// Only a duplicate-key collision (a concurrent caller inserted the
		// same uk_uid_space_client triple first) is safe to recover by
		// echoing the winning row — that preserves the idempotency
		// contract. Any other insert failure (connection lost, unrelated
		// constraint) must surface, not be masked by a stale re-read.
		if isDuplicateKeyErr(err) {
			again, reErr := s.db.queryActiveUserAPIKey(uid, spaceID, clientID)
			if reErr == nil && again != nil {
				return again.APIKey, nil
			}
		}
		return "", fmt.Errorf("insert user api key: %w", err)
	}
	return apiKey, nil
}

// isDuplicateKeyErr reports whether err is a MySQL duplicate-key violation
// (error 1062). Matched by substring to avoid coupling this package to a
// specific driver error type; the message is stable across go-sql-driver and
// dbr-wrapped errors.
func isDuplicateKeyErr(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "Duplicate entry") || strings.Contains(err.Error(), "1062")
}

func (s *userAPIKeyService) AuthByKey(plaintext string) (*UserAPIKey, error) {
	m, err := s.db.queryActiveUserAPIKeyByKey(plaintext)
	if err != nil {
		return nil, fmt.Errorf("query user api key by key: %w", err)
	}
	if m == nil {
		return nil, nil
	}
	return &UserAPIKey{
		ID:       m.ID,
		UID:      m.UID,
		SpaceID:  m.SpaceID,
		ClientID: m.ClientID,
		APIKey:   m.APIKey,
	}, nil
}
