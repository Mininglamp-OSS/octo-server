package project

import (
	"sync"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"go.uber.org/zap"
)

// SidebarSectionProvisioner creates one user's personal Project section after a
// Project seat or personal pin commits. category registers the implementation so
// project stays independent of category and no import cycle is introduced.
type SidebarSectionProvisioner func(ctx *config.Context, uid, spaceID, projectID string) error

// SidebarSectionRemover hides one user's personal Project section after an
// explicit unpin. Keeping this separate from provisioning makes the two user
// intents visible at call sites and avoids teaching project about category's
// storage table.
type SidebarSectionRemover func(ctx *config.Context, uid, spaceID, projectID string) error

var (
	sidebarSectionProvisionerMu sync.RWMutex
	sidebarSectionProvisionerFn SidebarSectionProvisioner
	sidebarSectionRemoverFn     SidebarSectionRemover
)

func RegisterSidebarSectionProvisioner(fn SidebarSectionProvisioner) {
	sidebarSectionProvisionerMu.Lock()
	defer sidebarSectionProvisionerMu.Unlock()
	sidebarSectionProvisionerFn = fn
}

func projectSidebarSectionProvisioner() SidebarSectionProvisioner {
	sidebarSectionProvisionerMu.RLock()
	defer sidebarSectionProvisionerMu.RUnlock()
	return sidebarSectionProvisionerFn
}

func RegisterSidebarSectionRemover(fn SidebarSectionRemover) {
	sidebarSectionProvisionerMu.Lock()
	defer sidebarSectionProvisionerMu.Unlock()
	sidebarSectionRemoverFn = fn
}

func projectSidebarSectionRemover() SidebarSectionRemover {
	sidebarSectionProvisionerMu.RLock()
	defer sidebarSectionProvisionerMu.RUnlock()
	return sidebarSectionRemoverFn
}

// provisionSidebarSection is intentionally post-commit and best-effort. The
// sidebar list read path backfills any missed Project entry, so a transient write
// failure must not undo a committed project, member seat, or personal pin.
func (p *Project) provisionSidebarSection(projectID, spaceID, uid string) {
	fn := projectSidebarSectionProvisioner()
	if fn == nil {
		return
	}
	if err := fn(p.ctx, uid, spaceID, projectID); err != nil {
		p.Warn("初始化项目侧边栏分区失败（降级继续）",
			zap.Error(err), zap.String("projectId", projectID), zap.String("spaceId", spaceID), zap.String("uid", uid))
	}
}

// removeSidebarSection mirrors provisionSidebarSection for an explicit unpin.
// The pinned=0 setting is the durable read-side opt-out, so a transient hook
// failure cannot make the next Follow read surface the Project again. Hiding the
// ordering row remains best-effort because the pin write has already committed.
func (p *Project) removeSidebarSection(projectID, spaceID, uid string) {
	fn := projectSidebarSectionRemover()
	if fn == nil {
		return
	}
	if err := fn(p.ctx, uid, spaceID, projectID); err != nil {
		p.Warn("移除项目侧边栏分区失败（降级继续）",
			zap.Error(err), zap.String("projectId", projectID), zap.String("spaceId", spaceID), zap.String("uid", uid))
	}
}
