package project

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
)

var (
	collaborationRoleMaintenanceOnce    sync.Once
	collaborationRoleMaintenanceRunning atomic.Bool
	collaborationRoleBackfillCursor     atomic.Int64
	collaborationRoleIntegrityState     collaborationRoleIntegrityScanState
)

type collaborationRoleIntegrityScanState struct {
	mu        sync.Mutex
	projectID string
	uid       string
	roleID    string
	running   int
}

func (s *collaborationRoleIntegrityScanState) resume() (string, string, string, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.projectID, s.uid, s.roleID, s.running
}

func (s *collaborationRoleIntegrityScanState) save(
	projectID, uid, roleID string, running int, completed bool,
) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if completed {
		s.projectID, s.uid, s.roleID, s.running = "", "", "", 0
		return
	}
	s.projectID, s.uid, s.roleID, s.running = projectID, uid, roleID, running
}

// startCollaborationRoleMaintenance runs the restart-safe built-in backfill and
// the association-integrity check on the existing bounded reconciliation cadence.
func (p *Project) startCollaborationRoleMaintenance() {
	collaborationRoleMaintenanceOnce.Do(func() {
		p.ctx.Schedule(jitter(p.cfg.ReconcileInterval), p.runCollaborationRoleMaintenance)
	})
}

func (p *Project) runCollaborationRoleMaintenance() {
	if !collaborationRoleMaintenanceRunning.CompareAndSwap(false, true) {
		return
	}
	defer collaborationRoleMaintenanceRunning.Store(false)
	defer func() {
		if recovered := recover(); recovered != nil {
			collaborationRoleMaintenanceFailures.WithLabelValues("panic").Inc()
			p.Error("项目协作角色维护任务 panic", zap.Any("recover", recovered))
		}
	}()

	p.backfillBuiltinCollaborationRoles()
	p.scanCollaborationRoleIntegrity()
}

func (p *Project) backfillBuiltinCollaborationRoles() {
	afterID := collaborationRoleBackfillCursor.Load()
	projects, err := p.db.collaborationRoleBackfillPage(afterID, p.cfg.ReconcileLimit)
	if err != nil {
		collaborationRoleMaintenanceFailures.WithLabelValues("backfill_query").Inc()
		p.Warn("查询待补齐内置协作角色的项目失败", zap.Error(err))
		return
	}
	missing := 0
	for _, project := range projects {
		changed := false
		err := retryOnLockConflict(func() error {
			var attemptErr error
			changed, attemptErr = p.backfillBuiltinCollaborationRolesOnce(project.ProjectID)
			return attemptErr
		})
		if err != nil {
			missing++
			collaborationRoleMaintenanceFailures.WithLabelValues("backfill_write").Inc()
			p.Warn("补齐项目内置协作角色失败", zap.Error(err), zap.String("projectId", project.ProjectID))
			continue
		}
		if changed {
			missing++
		}
	}
	collaborationRoleBackfillPending.Set(float64(missing))
	if len(projects) < p.cfg.ReconcileLimit {
		collaborationRoleBackfillCursor.Store(0)
	} else {
		collaborationRoleBackfillCursor.Store(projects[len(projects)-1].ID)
	}
}

func (p *Project) backfillBuiltinCollaborationRolesOnce(projectID string) (bool, error) {
	tx, err := p.db.session.Begin()
	if err != nil {
		return false, fmt.Errorf("project: begin collaboration role backfill: %w", err)
	}
	defer tx.RollbackUnlessCommitted()

	project, err := p.db.lockActiveProjectTx(tx, projectID)
	if err != nil {
		return false, err
	}
	if project == nil {
		return false, nil
	}
	changed, err := p.db.seedBuiltinCollaborationRolesTx(tx, projectID, time.Now().UTC())
	if err != nil {
		return false, err
	}
	if changed {
		if err := p.db.bumpCollaborationRoleEpochTx(tx, projectID); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("project: commit collaboration role backfill: %w", err)
	}
	if changed {
		collaborationRoleBackfilled.Inc()
	}
	return changed, nil
}

func (p *Project) scanCollaborationRoleIntegrity() {
	projectID, uid, roleID, total := collaborationRoleIntegrityState.resume()
	rows, err := p.db.collaborationRoleIntegrityPage(
		projectID, uid, roleID, p.cfg.ReconcileLimit)
	if err != nil {
		collaborationRoleMaintenanceFailures.WithLabelValues("integrity_scan").Inc()
		p.Warn("扫描项目协作角色关联完整性失败", zap.Error(err))
		return
	}
	for _, row := range rows {
		if row.Violating {
			total++
		}
	}
	completed := len(rows) < p.cfg.ReconcileLimit
	if len(rows) > 0 {
		last := rows[len(rows)-1]
		projectID, uid, roleID = last.ProjectID, last.UID, last.RoleID
	}
	if completed {
		collaborationRoleIntegrityViolations.Set(float64(total))
	}
	collaborationRoleIntegrityState.save(projectID, uid, roleID, total, completed)
	if completed && total > 0 {
		p.Error("发现项目协作角色孤儿关联或非活跃成员关联",
			zap.Int("violations", total), zap.Int("page_limit", p.cfg.ReconcileLimit))
	}
}
