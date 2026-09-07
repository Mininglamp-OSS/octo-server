package space

import (
	"sync"

	"github.com/Mininglamp-OSS/octo-lib/config"
)

// PresetGroupAdmitter admits uid into a Space's preset group.
//
// # Why a registry instead of a call
//
// modules/group imports modules/space, so modules/space cannot import
// modules/group — an import cycle. joinPresetGroups worked around that by doing
// raw DML against group_member, and the workaround is what this file replaces.
// The pattern is the one already used for the other direction of the same
// problem: RegisterMemberRemovalCleanupStep, and hooks.go's
// DefaultCategoryProvisioner.
//
// # What the raw INSERT actually cost
//
// `INSERT INTO group_member (group_no, uid) VALUES (?, ?)` — two columns, no
// transaction, launched as a bare goroutine, every failure a Warn + continue.
// Four defects in one statement, all of which the admission funnel fixes as a
// side effect of being the funnel:
//
//  1. The presence check filtered is_deleted = 0, so a uid who had previously
//     LEFT the group passed it. The INSERT then hit `unique index group_no_uid`,
//     MySQL returned 1062, the error was logged as a warning, and that user was
//     PERMANENTLY never re-added to that preset group — on every subsequent
//     Space join, forever. Two places in this tree already document the defect
//     (modules/space/db_manager.go:219 and P0's migration).
//  2. No `version`, so incremental member sync never sees the row: the member is
//     in the group in the database and missing from every client's member list
//     until something else bumps the group's member version.
//  3. No `vercode`, `role`, `invite_uid`, `is_external` or `source_space_id`.
//  4. No IMAddSubscriber, so the member joins and receives nothing over
//     WuKongIM.
//
// # Fail-closed when unregistered
//
// A binary that contains modules/space but not modules/group leaves this nil.
// joinPresetGroups then SKIPS, and does not fall back to writing the row itself.
// Falling back would reintroduce all four defects and put a group_member write
// back outside the admission funnel, which is precisely what the source guard
// forbids — and the guard would not catch it, because it would be inside the
// allowlisted call path. Skipping is visible in the log; a defective row is not.
type PresetGroupAdmitter func(ctx *config.Context, spaceID, groupNo, uid string) error

var (
	presetGroupAdmitterMu sync.RWMutex
	presetGroupAdmitterFn PresetGroupAdmitter
)

// RegisterPresetGroupAdmitter is called by modules/group at construction.
// Re-registration replaces (latest wins), which is what lets a test install a
// stand-in.
func RegisterPresetGroupAdmitter(fn PresetGroupAdmitter) {
	presetGroupAdmitterMu.Lock()
	defer presetGroupAdmitterMu.Unlock()
	presetGroupAdmitterFn = fn
}

func presetGroupAdmitter() PresetGroupAdmitter {
	presetGroupAdmitterMu.RLock()
	defer presetGroupAdmitterMu.RUnlock()
	return presetGroupAdmitterFn
}
