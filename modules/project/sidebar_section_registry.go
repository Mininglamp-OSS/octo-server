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

var (
	sidebarSectionProvisionerMu sync.RWMutex
	sidebarSectionProvisionerFn SidebarSectionProvisioner
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
