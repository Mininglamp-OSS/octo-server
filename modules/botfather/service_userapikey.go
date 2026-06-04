package botfather

import (
	"errors"
	"fmt"
	"strings"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/gocraft/dbr/v2"
	"go.uber.org/zap"
)

// clientIDBotFather labels `uk_` keys minted by botfather's own /quickstart
// flow. The integration (Octo-link) module passes other client_id labels
// (e.g. the external application id) into the same service, so every `uk_` is
// organised along the (uid, space_id, client_id) dimension.
const clientIDBotFather = "botfather"

// ErrIntegrationClientDisabled is returned when an integration-scoped key
// issuance attempts to run while the external client is missing or disabled.
var ErrIntegrationClientDisabled = errors.New("botfather: integration client disabled")

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
	// GetOrCreateForEnabledIntegrationClient is the integration-safe issuance
	// path. It serializes with manager disable by locking the integration_client
	// row and re-checking enabled state in the same transaction that creates or
	// rotates the `uk_` row.
	GetOrCreateForEnabledIntegrationClient(uid, spaceID, clientID string) (string, error)
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
		plaintext, err := s.plaintextForStoredKey(existing)
		if err != nil {
			return "", fmt.Errorf("load existing user api key: %w", err)
		}
		return plaintext, nil
	}

	apiKey, err := generateUserAPIKey()
	if err != nil {
		return "", fmt.Errorf("generate user api key: %w", err)
	}
	storedAPIKey, apiKeyHash, apiKeyCipher, err := buildUserAPIKeyStorageForClient(apiKey, clientID)
	if err != nil {
		return "", fmt.Errorf("secure user api key: %w", err)
	}

	if err := s.db.insertUserAPIKey(uid, storedAPIKey, apiKeyHash, apiKeyCipher, spaceID, clientID); err != nil {
		// Only a duplicate-key collision (a concurrent caller inserted the
		// same uk_uid_space_client triple first) is safe to recover by
		// echoing the winning row — that preserves the idempotency
		// contract. Any other insert failure (connection lost, unrelated
		// constraint) must surface, not be masked by a stale re-read.
		if isDuplicateKeyErr(err) {
			again, reErr := s.db.queryActiveUserAPIKey(uid, spaceID, clientID)
			if reErr == nil && again != nil {
				plaintext, plainErr := s.plaintextForStoredKey(again)
				if plainErr != nil {
					return "", fmt.Errorf("load concurrent user api key: %w", plainErr)
				}
				return plaintext, nil
			}
			affected, rotateErr := s.db.rotateRevokedUserAPIKey(uid, spaceID, clientID, storedAPIKey, apiKeyHash, apiKeyCipher)
			if rotateErr == nil && affected == 1 {
				return apiKey, nil
			}
			if rotateErr != nil {
				return "", fmt.Errorf("rotate revoked user api key: %w", rotateErr)
			}
			if affected == 0 {
				again, reErr := s.db.queryActiveUserAPIKey(uid, spaceID, clientID)
				if reErr != nil {
					return "", fmt.Errorf("query user api key after rotate collision: %w", reErr)
				}
				if again != nil {
					plaintext, plainErr := s.plaintextForStoredKey(again)
					if plainErr != nil {
						return "", fmt.Errorf("load rotated user api key: %w", plainErr)
					}
					return plaintext, nil
				}
			}
		}
		return "", fmt.Errorf("insert user api key: %w", err)
	}
	return apiKey, nil
}

func (s *userAPIKeyService) GetOrCreateForEnabledIntegrationClient(uid, spaceID, clientID string) (string, error) {
	clientID = strings.TrimSpace(clientID)
	if clientID == "" || clientID == clientIDBotFather {
		return s.GetOrCreate(uid, spaceID, clientID)
	}

	tx, err := s.db.session.Begin()
	if err != nil {
		return "", fmt.Errorf("begin user api key issuance: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	enabled, err := s.db.lockIntegrationClientEnabledTx(tx, clientID)
	if err != nil {
		return "", fmt.Errorf("lock integration client: %w", err)
	}
	if !enabled {
		return "", ErrIntegrationClientDisabled
	}
	if err := ValidateUserAPIKeySecret(); err != nil {
		return "", fmt.Errorf("validate integration user api key secret: %w", err)
	}

	existing, err := s.db.queryActiveUserAPIKeyTx(tx, uid, spaceID, clientID)
	if err != nil {
		return "", fmt.Errorf("query user api key: %w", err)
	}
	if existing != nil {
		plaintext, err := s.plaintextForStoredKeyTx(tx, existing)
		if err != nil {
			return "", fmt.Errorf("load existing user api key: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return "", fmt.Errorf("commit existing user api key issuance: %w", err)
		}
		committed = true
		return plaintext, nil
	}

	apiKey, err := generateUserAPIKey()
	if err != nil {
		return "", fmt.Errorf("generate user api key: %w", err)
	}
	storedAPIKey, apiKeyHash, apiKeyCipher, err := buildUserAPIKeyStorageForClient(apiKey, clientID)
	if err != nil {
		return "", fmt.Errorf("secure user api key: %w", err)
	}

	if err := s.db.insertUserAPIKeyTx(tx, uid, storedAPIKey, apiKeyHash, apiKeyCipher, spaceID, clientID); err != nil {
		if isDuplicateKeyErr(err) {
			again, reErr := s.db.queryActiveUserAPIKeyTx(tx, uid, spaceID, clientID)
			if reErr == nil && again != nil {
				plaintext, plainErr := s.plaintextForStoredKeyTx(tx, again)
				if plainErr != nil {
					return "", fmt.Errorf("load concurrent user api key: %w", plainErr)
				}
				if err := tx.Commit(); err != nil {
					return "", fmt.Errorf("commit concurrent user api key issuance: %w", err)
				}
				committed = true
				return plaintext, nil
			}
			affected, rotateErr := s.db.rotateRevokedUserAPIKeyTx(tx, uid, spaceID, clientID, storedAPIKey, apiKeyHash, apiKeyCipher)
			if rotateErr == nil && affected == 1 {
				if err := tx.Commit(); err != nil {
					return "", fmt.Errorf("commit rotated user api key issuance: %w", err)
				}
				committed = true
				return apiKey, nil
			}
			if rotateErr != nil {
				return "", fmt.Errorf("rotate revoked user api key: %w", rotateErr)
			}
			if affected == 0 {
				again, reErr := s.db.queryActiveUserAPIKeyTx(tx, uid, spaceID, clientID)
				if reErr != nil {
					return "", fmt.Errorf("query user api key after rotate collision: %w", reErr)
				}
				if again != nil {
					plaintext, plainErr := s.plaintextForStoredKeyTx(tx, again)
					if plainErr != nil {
						return "", fmt.Errorf("load rotated user api key: %w", plainErr)
					}
					if err := tx.Commit(); err != nil {
						return "", fmt.Errorf("commit rotated collision user api key issuance: %w", err)
					}
					committed = true
					return plaintext, nil
				}
			}
		}
		return "", fmt.Errorf("insert user api key: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("commit user api key issuance: %w", err)
	}
	committed = true
	return apiKey, nil
}

func buildUserAPIKeyStorageForClient(plaintext, clientID string) (stored, hash, cipherText string, err error) {
	if strings.TrimSpace(plaintext) == "" {
		return "", "", "", errors.New("empty user api key")
	}
	if clientID == "" || clientID == clientIDBotFather {
		return plaintext, "", "", nil
	}
	return buildUserAPIKeyStorage(plaintext)
}

func (s *userAPIKeyService) plaintextForStoredKey(m *userAPIKeyModel) (string, error) {
	if m == nil {
		return "", nil
	}
	if m.APIKeyCipher != "" {
		return decryptUserAPIKey(m.APIKeyCipher)
	}
	if strings.HasPrefix(m.APIKey, UserAPIKeyPrefix) {
		if m.ClientID != "" && m.ClientID != clientIDBotFather {
			storedAPIKey, apiKeyHash, apiKeyCipher, err := buildUserAPIKeyStorage(m.APIKey)
			if err != nil {
				return "", err
			}
			if err := s.db.secureLegacyUserAPIKey(m.ID, m.APIKey, storedAPIKey, apiKeyHash, apiKeyCipher); err != nil {
				s.Warn("legacy user api key secure upgrade failed; continuing with matched key", zap.Int64("keyID", m.ID), zap.Error(err))
			}
		}
		return m.APIKey, nil
	}
	return "", fmt.Errorf("user api key id=%d has no decryptable cipher", m.ID)
}

func (s *userAPIKeyService) plaintextForStoredKeyTx(tx *dbr.Tx, m *userAPIKeyModel) (string, error) {
	if m == nil {
		return "", nil
	}
	if m.APIKeyCipher != "" {
		return decryptUserAPIKey(m.APIKeyCipher)
	}
	if strings.HasPrefix(m.APIKey, UserAPIKeyPrefix) {
		if m.ClientID != "" && m.ClientID != clientIDBotFather {
			storedAPIKey, apiKeyHash, apiKeyCipher, err := buildUserAPIKeyStorage(m.APIKey)
			if err != nil {
				return "", err
			}
			if err := s.db.secureLegacyUserAPIKeyTx(tx, m.ID, m.APIKey, storedAPIKey, apiKeyHash, apiKeyCipher); err != nil {
				s.Warn("legacy user api key secure upgrade failed in transaction; continuing with matched key", zap.Int64("keyID", m.ID), zap.Error(err))
			}
		}
		return m.APIKey, nil
	}
	return "", fmt.Errorf("user api key id=%d has no decryptable cipher", m.ID)
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
	enabled, err := s.db.isIntegrationClientEnabled(m.ClientID)
	if err != nil {
		return nil, fmt.Errorf("query integration client status: %w", err)
	}
	if !enabled {
		return nil, nil
	}
	activeUser, err := s.db.isActiveUser(m.UID)
	if err != nil {
		return nil, fmt.Errorf("query user status: %w", err)
	}
	if !activeUser {
		return nil, nil
	}
	if m.ClientID != "" && m.ClientID != clientIDBotFather && m.APIKeyCipher == "" && strings.HasPrefix(m.APIKey, UserAPIKeyPrefix) {
		storedAPIKey, apiKeyHash, apiKeyCipher, err := buildUserAPIKeyStorage(m.APIKey)
		if err != nil {
			return nil, fmt.Errorf("secure legacy user api key: %w", err)
		}
		if err := s.db.secureLegacyUserAPIKey(m.ID, m.APIKey, storedAPIKey, apiKeyHash, apiKeyCipher); err != nil {
			s.Warn("legacy user api key secure upgrade failed during auth; continuing with matched key", zap.Int64("keyID", m.ID), zap.Error(err))
		}
	}
	return &UserAPIKey{
		ID:       m.ID,
		UID:      m.UID,
		SpaceID:  m.SpaceID,
		ClientID: m.ClientID,
		APIKey:   plaintext,
	}, nil
}
