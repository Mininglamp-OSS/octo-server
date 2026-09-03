package bot_api

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"regexp"

	"github.com/gocraft/dbr/v2"
)

const (
	botKindUser = "user"
	botKindApp  = "app"
)

var (
	errBotInstanceConflict = errors.New("bot token is bound to another instance")
	instanceIDPattern      = regexp.MustCompile(`^[A-Za-z0-9._:-]{16,128}$`)
)

type botInstanceBinding struct {
	TokenFingerprint []byte `db:"token_fingerprint"`
	BotKind          string `db:"bot_kind"`
	RobotID          string `db:"robot_id"`
	InstanceID       string `db:"instance_id"`
	IMToken          string `db:"im_token"`
}

func validInstanceID(instanceID string) bool {
	return instanceIDPattern.MatchString(instanceID)
}

func botBindingFingerprint(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

func generateBindingIMToken() (string, error) {
	random := make([]byte, 32)
	if _, err := rand.Read(random); err != nil {
		return "", err
	}
	return "im_" + hex.EncodeToString(random), nil
}

// resolveRegistrationIMToken atomically claims a Bot Token for instanceID.
//
// The unique token fingerprint is the cross-replica lock: concurrent INSERTs
// can create only one row, and every contender then reads the winning owner.
// A legacy request remains compatible while no binding exists, but cannot
// bypass a binding after a modern client has claimed the token.
func (d *botAPIDB) resolveRegistrationIMToken(
	botToken string,
	botKind string,
	robotID string,
	instanceID string,
) (string, bool, error) {
	fingerprint := botBindingFingerprint(botToken)
	if instanceID == "" {
		binding, err := d.queryBotInstanceBinding(fingerprint)
		if errors.Is(err, dbr.ErrNotFound) {
			return botToken, false, nil
		}
		if err != nil {
			return "", false, err
		}
		if binding != nil {
			return "", false, errBotInstanceConflict
		}
		return botToken, false, nil
	}

	candidate, err := generateBindingIMToken()
	if err != nil {
		return "", false, err
	}
	_, err = d.session.InsertBySql(
		"INSERT INTO bot_instance_binding (token_fingerprint, bot_kind, robot_id, instance_id, im_token) "+
			"VALUES (?, ?, ?, ?, ?) ON DUPLICATE KEY UPDATE token_fingerprint=token_fingerprint",
		fingerprint, botKind, robotID, instanceID, candidate,
	).Exec()
	if err != nil {
		return "", false, err
	}

	binding, err := d.queryBotInstanceBinding(fingerprint)
	if err != nil {
		return "", false, err
	}
	if binding == nil || binding.BotKind != botKind || binding.RobotID != robotID || binding.InstanceID != instanceID {
		return "", false, errBotInstanceConflict
	}
	return binding.IMToken, true, nil
}

func (d *botAPIDB) queryBotInstanceBinding(fingerprint []byte) (*botInstanceBinding, error) {
	var binding botInstanceBinding
	err := d.session.Select("token_fingerprint", "bot_kind", "robot_id", "instance_id", "im_token").
		From("bot_instance_binding").
		Where("token_fingerprint=?", fingerprint).
		LoadOne(&binding)
	if err != nil {
		return nil, err
	}
	return &binding, nil
}
