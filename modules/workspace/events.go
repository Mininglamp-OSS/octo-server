package workspace

import (
	"os"

	"github.com/Mininglamp-OSS/octo-server/modules/base/outbox"
	"github.com/gocraft/dbr/v2"
)

const (
	eventTypeWorkspaceCreated          = "workspace.created"
	eventTypeWorkspaceUpdated          = "workspace.updated"
	eventTypeWorkspaceMembersChanged   = "workspace.members_changed"
	eventTypeWorkspaceOwnerTransferred = "workspace.owner_transferred"
)

func enqueueWorkspaceEventTx(tx *dbr.Tx, eventType, workspaceID string) (string, error) {
	return outbox.EnqueueTx(tx, outbox.Event{
		Domain:     "workspace",
		ResourceID: workspaceID,
		EventType:  eventType,
		Data: map[string]any{
			"workspace_id": workspaceID,
		},
	})
}

// RegisterEventTargets registers the Workspace subscribers used by the event
// outbox. Every mutation is persisted for both targets; credentials gate
// delivery so temporary configuration outages remain recoverable.
func RegisterEventTargets() {
	outbox.RegisterTargets("workspace",
		outbox.Target{
			Service: "loop",
			Enabled: func() bool {
				token, err := loadLoopInternalToken(os.Getenv)
				return err == nil && token != ""
			},
		},
		outbox.Target{
			Service: "driver",
			Enabled: func() bool {
				token, err := loadDriveInternalToken(os.Getenv)
				return err == nil && token != ""
			},
		},
	)
}
