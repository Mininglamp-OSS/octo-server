package workspace

import (
	"crypto/subtle"
	"errors"
	"os"

	"go.uber.org/zap"

	"github.com/Mininglamp-OSS/octo-server/modules/internal_resolve"
)

const (
	// LoopInternalTokenEnv gates the loop-facing Workspace internal API.
	LoopInternalTokenEnv = "OCTO_LOOP_INTERNAL_TOKEN"

	// DriveInternalTokenEnv is shared with the drive-facing resolve API. Keep
	// the source of truth in internal_resolve so the two modules cannot drift.
	DriveInternalTokenEnv = internal_resolve.DriveInternalTokenEnv

	workspaceInternalTokenHeader = "X-Internal-Token"
	workspaceMinInternalTokenLen = 32

	notifyInternalTokenEnv     = "NOTIFY_INTERNAL_TOKEN"
	docsNotifyInternalTokenEnv = "OCTO_DOCS_NOTIFY_TOKEN"
	botMentionInternalTokenEnv = "OCTO_DOCS_BOT_MENTION_TOKEN"
)

// loadLoopInternalToken loads the loop credential at API construction time.
// An unset value disables only the loop capability; malformed configured values
// disable it as well and return a logger-safe error.
func loadLoopInternalToken(getenv func(string) string) (string, error) {
	return loadWorkspaceInternalToken(LoopInternalTokenEnv, getenv)
}

// loadDriveInternalToken mirrors internal_resolve's drive credential checks for
// the Workspace internal API. Keeping this validation local avoids making the
// resolve module expose an HTTP-specific configuration helper.
func loadDriveInternalToken(getenv func(string) string) (string, error) {
	return loadWorkspaceInternalToken(DriveInternalTokenEnv, getenv)
}

func loadWorkspaceInternalToken(env string, getenv func(string) string) (string, error) {
	if getenv == nil {
		return "", errors.New(env + " lookup unavailable; Workspace internal API disabled")
	}
	token := getenv(env)
	if token == "" {
		return "", nil
	}
	if len(token) < workspaceMinInternalTokenLen {
		return "", errors.New(env + " must be at least 32 bytes; Workspace internal API capability disabled")
	}
	for _, sibling := range []string{
		LoopInternalTokenEnv,
		DriveInternalTokenEnv,
		notifyInternalTokenEnv,
		docsNotifyInternalTokenEnv,
		botMentionInternalTokenEnv,
	} {
		if sibling == env {
			continue
		}
		if sameInternalToken(token, getenv(sibling)) {
			return "", errors.New(env + " must differ from " + sibling + "; Workspace internal API capability disabled")
		}
	}
	return token, nil
}

func sameInternalToken(left, right string) bool {
	if left == "" || right == "" || len(left) != len(right) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(left), []byte(right)) == 1
}

func (a *API) initInternalTokens() {
	if a == nil {
		return
	}
	loopToken, loopErr := loadLoopInternalToken(os.Getenv)
	if loopErr != nil {
		a.Error("workspace loop internal API disabled", zap.Error(loopErr))
	}
	driveToken, driveErr := loadDriveInternalToken(os.Getenv)
	if driveErr != nil {
		a.Error("workspace drive internal API disabled", zap.Error(driveErr))
	}
	a.loopToken = loopToken
	a.driveToken = driveToken
}
