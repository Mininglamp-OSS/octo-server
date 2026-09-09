package robot

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	spacemod "github.com/Mininglamp-OSS/octo-server/modules/space"
	"github.com/Mininglamp-OSS/octo-server/pkg/botpolicy"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	"github.com/gocraft/dbr/v2"
	"go.uber.org/zap"
)

type avatarCreateReq struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Scope       string `json:"scope"`
	SpaceID     string `json:"space_id"`
}

const (
	avatarNameMaxLength        = 64
	avatarDescriptionMaxLength = 500
)

type avatarUpdateReq struct {
	Name        *string `json:"name"`
	Description *string `json:"description"`
}

type avatarResp struct {
	RobotID          string `json:"robot_id"`
	Name             string `json:"name"`
	Description      string `json:"description"`
	Scope            string `json:"scope"`
	SpaceID          string `json:"space_id,omitempty"`
	Publication      string `json:"publication_state"`
	LifecyclePending int    `json:"lifecycle_pending"`
	CreatedBy        string `json:"created_by"`
}

func (m *Manager) avatarIdentity(robotID string) (*botpolicy.Identity, error) {
	return botpolicy.Lookup(m.ctx.DB(), robotID)
}

func (m *Manager) requireAvatarManager(c *wkhttp.Context, identity *botpolicy.Identity) bool {
	allowed, err := botpolicy.CanManageAvatar(m.ctx.DB(), identity, c.GetLoginUID(), c.CheckLoginRoleIsSuperAdmin() == nil)
	if err != nil {
		m.Error("check avatar manager failed", zap.Error(err), zap.String("robot_id", identityUID(identity)))
		httperr.ResponseErrorL(c, errcode.ErrRobotQueryFailed, nil, nil)
		return false
	}
	if !allowed {
		respondManagerForbidden(c)
		return false
	}
	return true
}

func identityUID(identity *botpolicy.Identity) string {
	if identity == nil {
		return ""
	}
	return identity.UID
}

func (m *Manager) avatarCreate(c *wkhttp.Context) {
	var req avatarCreateReq
	if err := c.BindJSON(&req); err != nil {
		respondRobotRequestInvalid(c, "")
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	req.Description = strings.TrimSpace(req.Description)
	req.Scope = strings.TrimSpace(req.Scope)
	req.SpaceID = strings.TrimSpace(req.SpaceID)
	if req.Name == "" || len(req.Name) > avatarNameMaxLength {
		respondRobotRequestInvalid(c, "name")
		return
	}
	if len(req.Description) > avatarDescriptionMaxLength {
		respondRobotRequestInvalid(c, "description")
		return
	}
	identity := &botpolicy.Identity{Kind: botpolicy.Avatar, Scope: req.Scope, SpaceID: req.SpaceID}
	if req.Scope == botpolicy.ScopePlatform {
		if req.SpaceID != "" || c.CheckLoginRoleIsSuperAdmin() != nil {
			respondManagerForbidden(c)
			return
		}
	} else if req.Scope == botpolicy.ScopeSpace && req.SpaceID != "" {
		if !m.requireAvatarManager(c, identity) {
			return
		}
	} else {
		respondRobotRequestInvalid(c, "scope")
		return
	}

	botToken, err := m.generateUniqueBotToken()
	if err != nil {
		m.Error("generate avatar token failed", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrRobotTokenGenFailed, nil, nil)
		return
	}
	robotID := "avatar_" + strings.ReplaceAll(util.GenerUUID(), "-", "")
	if len(robotID) > 40 {
		robotID = robotID[:40]
	}
	tx, err := m.ctx.DB().Begin()
	if err != nil {
		m.Error("begin avatar create failed", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrRobotStoreFailed, nil, nil)
		return
	}
	defer tx.RollbackUnlessCommitted()
	_, err = tx.InsertBySql("INSERT INTO `user` (uid,name,username,short_no,status,robot,is_destroy) VALUES (?,?,?,?,1,1,0)",
		robotID, req.Name, robotID, robotID).Exec()
	if err == nil {
		_, err = tx.InsertBySql(`INSERT INTO robot
			(robot_id,username,status,creator_uid,description,bot_token,auto_approve,kind,
			 management_scope,management_space_id,created_by,publication_state,lifecycle_pending)
			VALUES (?,?,0,'',?,?,1,'avatar',?,?,?,'draft',0)`,
			robotID, robotID, req.Description, botToken, req.Scope, req.SpaceID, c.GetLoginUID()).Exec()
	}
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		m.Error("create avatar failed", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrRobotStoreFailed, nil, nil)
		return
	}
	c.Response(map[string]interface{}{"robot_id": robotID, "bot_token": botToken, "publication_state": botpolicy.Draft})
}

func (m *Manager) avatarList(c *wkhttp.Context) {
	scope, spaceID := strings.TrimSpace(c.Query("scope")), strings.TrimSpace(c.Query("space_id"))
	identity := &botpolicy.Identity{Kind: botpolicy.Avatar, Scope: scope, SpaceID: spaceID}
	if scope == botpolicy.ScopePlatform {
		if spaceID != "" || c.CheckLoginRoleIsSuperAdmin() != nil {
			respondManagerForbidden(c)
			return
		}
	} else if scope == botpolicy.ScopeSpace && spaceID != "" {
		if !m.requireAvatarManager(c, identity) {
			return
		}
	} else {
		respondRobotRequestInvalid(c, "scope")
		return
	}
	var rows []*struct {
		RobotID          string `db:"robot_id"`
		Name             string `db:"name"`
		Description      string `db:"description"`
		Scope            string `db:"management_scope"`
		SpaceID          string `db:"management_space_id"`
		Publication      string `db:"publication_state"`
		LifecyclePending int    `db:"lifecycle_pending"`
		CreatedBy        string `db:"created_by"`
	}
	_, err := m.ctx.DB().SelectBySql(`SELECT r.robot_id,u.name,r.description,r.management_scope,
		r.management_space_id,r.publication_state,r.lifecycle_pending,r.created_by
		FROM robot r JOIN user u ON u.uid=r.robot_id
		WHERE r.kind='avatar' AND r.management_scope=? AND r.management_space_id=?
		AND r.publication_state<>'deleted' ORDER BY r.created_at DESC`, scope, spaceID).Load(&rows)
	if err != nil {
		m.Error("list avatars failed", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrRobotQueryFailed, nil, nil)
		return
	}
	result := make([]avatarResp, 0, len(rows))
	for _, row := range rows {
		result = append(result, avatarResp{RobotID: row.RobotID, Name: row.Name, Description: row.Description,
			Scope: row.Scope, SpaceID: row.SpaceID, Publication: row.Publication,
			LifecyclePending: row.LifecyclePending, CreatedBy: row.CreatedBy})
	}
	c.Response(result)
}

func (m *Manager) loadManagedAvatar(c *wkhttp.Context) (*botpolicy.Identity, *robot, bool) {
	robotID := strings.TrimSpace(c.Param("robot_id"))
	identity, err := m.avatarIdentity(robotID)
	if err != nil {
		m.Error("query avatar failed", zap.Error(err), zap.String("robot_id", robotID))
		httperr.ResponseErrorL(c, errcode.ErrRobotQueryFailed, nil, nil)
		return nil, nil, false
	}
	if identity == nil || identity.Kind != botpolicy.Avatar {
		httperr.ResponseErrorL(c, errcode.ErrRobotNotFound, nil, nil)
		return nil, nil, false
	}
	if !m.requireAvatarManager(c, identity) {
		return nil, nil, false
	}
	row, err := m.db.queryRobotWithRobtID(robotID)
	if err != nil || row == nil {
		if err != nil {
			m.Error("query avatar detail failed", zap.Error(err), zap.String("robot_id", robotID))
		}
		httperr.ResponseErrorL(c, errcode.ErrRobotQueryFailed, nil, nil)
		return nil, nil, false
	}
	return identity, row, true
}

func (m *Manager) avatarDetail(c *wkhttp.Context) {
	identity, row, ok := m.loadManagedAvatar(c)
	if !ok {
		return
	}
	var name string
	_ = m.ctx.DB().Select("name").From("user").Where("uid=?", row.RobotID).LoadOne(&name)
	c.Response(avatarResp{RobotID: row.RobotID, Name: name, Description: row.Description,
		Scope: identity.Scope, SpaceID: identity.SpaceID, Publication: identity.PublicationState,
		LifecyclePending: identity.LifecyclePending, CreatedBy: identity.CreatedBy})
}

func (m *Manager) avatarUpdate(c *wkhttp.Context) {
	identity, row, ok := m.loadManagedAvatar(c)
	if !ok {
		return
	}
	if identity.PublicationState == botpolicy.Deleted {
		httperr.ResponseErrorL(c, errcode.ErrRobotNotFound, nil, nil)
		return
	}
	var req avatarUpdateReq
	if err := c.BindJSON(&req); err != nil || (req.Name == nil && req.Description == nil) {
		respondRobotRequestInvalid(c, "")
		return
	}
	tx, err := m.ctx.DB().Begin()
	if err == nil {
		defer tx.RollbackUnlessCommitted()
		var publicationState string
		err = tx.SelectBySql(`SELECT publication_state FROM robot
			WHERE robot_id=? AND kind='avatar' FOR UPDATE`, row.RobotID).LoadOne(&publicationState)
		if err == nil && publicationState == botpolicy.Deleted {
			_ = tx.Rollback()
			httperr.ResponseErrorL(c, errcode.ErrRobotNotFound, nil, nil)
			return
		}
	}
	if err == nil {
		if req.Name != nil {
			name := strings.TrimSpace(*req.Name)
			if name == "" || len(name) > avatarNameMaxLength {
				respondRobotRequestInvalid(c, "name")
				_ = tx.Rollback()
				return
			}
			_, err = tx.Update("user").Set("name", name).Where("uid=?", row.RobotID).Exec()
		}
		if err == nil && req.Description != nil {
			description := strings.TrimSpace(*req.Description)
			if len(description) > avatarDescriptionMaxLength {
				respondRobotRequestInvalid(c, "description")
				_ = tx.Rollback()
				return
			}
			_, err = tx.Update("robot").Set("description", description).Where("robot_id=? AND kind='avatar'", row.RobotID).Exec()
		}
		if err == nil {
			err = tx.Commit()
		}
	}
	if err != nil {
		m.Error("update avatar failed", zap.Error(err), zap.String("robot_id", row.RobotID))
		httperr.ResponseErrorL(c, errcode.ErrRobotStoreFailed, nil, nil)
		return
	}
	c.ResponseOK()
}

func (m *Manager) avatarPublish(c *wkhttp.Context) {
	identity, _, ok := m.loadManagedAvatar(c)
	if !ok {
		return
	}
	if identity.PublicationState == botpolicy.Deleted {
		httperr.ResponseErrorL(c, errcode.ErrRobotNotFound, nil, nil)
		return
	}
	// Cleanup from an earlier unpublish/delete must converge before authority is
	// reopened. Otherwise a delayed worker could remove newly-created relations
	// or disconnect a freshly registered runtime after this publish succeeds.
	if identity.LifecyclePending != 0 {
		respondRobotRequestInvalid(c, "lifecycle_pending")
		return
	}
	tx, err := m.ctx.DB().Begin()
	if err == nil {
		err = botpolicy.LockEnrollmentTx(tx)
	}
	var locked struct {
		PublicationState string `db:"publication_state"`
		LifecyclePending int    `db:"lifecycle_pending"`
	}
	if err == nil {
		err = tx.SelectBySql(`SELECT publication_state,lifecycle_pending FROM robot
			WHERE robot_id=? AND kind='avatar' FOR UPDATE`, identity.UID).LoadOne(&locked)
	}
	if err == nil && (locked.PublicationState == botpolicy.Deleted || locked.LifecyclePending != 0) {
		err = errors.New("avatar is not publishable")
	}
	if err == nil {
		_, err = tx.Update("robot").SetMap(map[string]interface{}{
			"status": 1, "publication_state": botpolicy.Published,
		}).Where("robot_id=? AND kind='avatar'", identity.UID).Exec()
	}
	if err == nil && identity.Scope == botpolicy.ScopePlatform {
		_, err = tx.InsertBySql(`INSERT INTO space_member (space_id,uid,role,status)
			SELECT space_id,?,0,1 FROM space WHERE status=1
			ON DUPLICATE KEY UPDATE status=1,updated_at=NOW()`, identity.UID).Exec()
	} else if err == nil {
		_, err = tx.InsertBySql(`INSERT INTO space_member (space_id,uid,role,status) VALUES (?,?,0,1)
			ON DUPLICATE KEY UPDATE status=1,updated_at=NOW()`, identity.SpaceID, identity.UID).Exec()
	}
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		m.Error("publish avatar failed", zap.Error(err), zap.String("robot_id", identity.UID))
		httperr.ResponseErrorL(c, errcode.ErrRobotStoreFailed, nil, nil)
		return
	}
	c.ResponseOK()
}

func (m *Manager) avatarUnpublish(c *wkhttp.Context) {
	m.avatarDeactivate(c, botpolicy.Unpublished, "unpublish")
}

func (m *Manager) avatarDelete(c *wkhttp.Context) {
	m.avatarDeactivate(c, botpolicy.Deleted, "delete")
}

func (m *Manager) avatarDeactivate(c *wkhttp.Context, state, operation string) {
	identity, _, ok := m.loadManagedAvatar(c)
	if !ok {
		return
	}
	if identity.PublicationState == botpolicy.Deleted {
		if state != botpolicy.Deleted {
			httperr.ResponseErrorL(c, errcode.ErrRobotNotFound, nil, nil)
			return
		}
		if err := m.runAvatarCleanup(identity.UID); err != nil {
			m.Error("retry deleted avatar cleanup failed", zap.Error(err), zap.String("robot_id", identity.UID))
			httperr.ResponseErrorL(c, errcode.ErrRobotStoreFailed, nil, nil)
			return
		}
		c.ResponseOK()
		return
	}
	if identity.PublicationState == state {
		if identity.LifecyclePending != 0 {
			if err := m.runAvatarCleanup(identity.UID); err != nil {
				m.Warn("avatar cleanup remains pending", zap.Error(err), zap.String("robot_id", identity.UID))
			}
		}
		c.ResponseOK()
		return
	}
	revokedToken := ""
	if state != botpolicy.Deleted {
		var tokenErr error
		revokedToken, tokenErr = m.generateUniqueBotToken()
		if tokenErr != nil {
			m.Error("rotate avatar token during deactivation failed", zap.Error(tokenErr), zap.String("robot_id", identity.UID))
			httperr.ResponseErrorL(c, errcode.ErrRobotTokenGenFailed, nil, nil)
			return
		}
	}
	tx, err := m.ctx.DB().Begin()
	if err == nil {
		var resultErr error
		result, updateErr := tx.Update("robot").SetMap(map[string]interface{}{
			"status": 0, "publication_state": state, "lifecycle_pending": 1, "bot_token": revokedToken,
		}).Where("robot_id=? AND kind='avatar' AND publication_state=?", identity.UID, identity.PublicationState).Exec()
		err = updateErr
		if err == nil {
			var affected int64
			affected, resultErr = result.RowsAffected()
			if resultErr != nil {
				err = resultErr
			} else if affected != 1 {
				err = errors.New("avatar lifecycle state changed concurrently")
			}
		}
	}
	if err == nil {
		err = revokeAvatarRelationshipsTx(tx, identity.UID)
	}
	if err == nil {
		_, err = tx.InsertInto("avatar_cleanup_job").Columns("robot_id", "operator_uid", "operation", "state").
			Values(identity.UID, c.GetLoginUID(), operation, "pending").Exec()
	}
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		m.Error("deactivate avatar failed", zap.Error(err), zap.String("robot_id", identity.UID))
		httperr.ResponseErrorL(c, errcode.ErrRobotStoreFailed, nil, nil)
		return
	}
	if cleanupErr := m.runAvatarCleanup(identity.UID); cleanupErr != nil {
		m.Warn("avatar external cleanup pending retry", zap.Error(cleanupErr), zap.String("robot_id", identity.UID))
	}
	c.ResponseOK()
}

func revokeAvatarRelationshipsTx(tx *dbr.Tx, robotID string) error {
	statements := []struct {
		query string
		args  []interface{}
	}{
		{"UPDATE ai_team_agent SET is_added=0,updated_at=NOW() WHERE bot_id=? AND is_added=1", []interface{}{robotID}},
		{"UPDATE friend SET is_deleted=1,updated_at=NOW() WHERE (uid=? OR to_uid=?) AND is_deleted=0", []interface{}{robotID, robotID}},
	}
	for _, statement := range statements {
		if _, err := tx.UpdateBySql(statement.query, statement.args...).Exec(); err != nil {
			return err
		}
	}
	return nil
}

type avatarCleanupJob struct {
	ID        int64  `db:"id"`
	RobotID   string `db:"robot_id"`
	Operator  string `db:"operator_uid"`
	Operation string `db:"operation"`
}

func (m *Manager) runAvatarCleanup(robotID string) error {
	var jobs []*avatarCleanupJob
	_, err := m.ctx.DB().SelectBySql(`SELECT id,robot_id,operator_uid,operation FROM avatar_cleanup_job
		WHERE robot_id=? AND state IN ('pending','running') ORDER BY id`, robotID).Load(&jobs)
	if err != nil {
		return err
	}
	var result error
	for _, job := range jobs {
		if err := m.runAvatarCleanupJob(job); err != nil {
			result = errors.Join(result, err)
		}
	}
	return result
}

func (m *Manager) runAvatarCleanupJob(job *avatarCleanupJob) error {
	if job == nil {
		return nil
	}
	claimed, err := m.ctx.DB().Update("avatar_cleanup_job").Set("state", "running").
		Where("id=? AND (state='pending' OR (state='running' AND updated_at < DATE_SUB(NOW(), INTERVAL 1 MINUTE)))", job.ID).Exec()
	if err != nil {
		return err
	}
	rows, err := claimed.RowsAffected()
	if err != nil || rows == 0 {
		return err
	}

	var cleanupErr error
	if job.Operation != "rotate" {
		// Space seats own the project/group cascade. Closing them through the
		// transactional outbox is mandatory: direct member-table updates skip the
		// project's two-phase removal and can strand IM subscribers. robot.status
		// has already revoked authority, so retries cannot reopen it.
		_, closeErr := m.closeSeatsFn(m.ctx, job.RobotID, job.Operator, spacemod.MemberRemoveReasonBotDeleted)
		cleanupErr = errors.Join(cleanupErr, closeErr)
	}
	cleanupConnection := m.cleanupConnectionFn
	if cleanupConnection == nil {
		cleanupConnection = m.cleanupBotConnection
	}
	cleanupErr = errors.Join(cleanupErr, cleanupConnection(job.RobotID))
	if job.Operation != "rotate" && cleanupErr == nil {
		pending, abandoned, progressErr := spacemod.MemberRemovalCleanupProgress(
			m.ctx, job.RobotID, spacemod.MemberRemoveReasonBotDeleted,
		)
		cleanupErr = errors.Join(cleanupErr, progressErr)
		if progressErr == nil && (pending > 0 || abandoned > 0) {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("space cleanup pending=%d abandoned=%d", pending, abandoned))
		}
		if progressErr == nil && pending == 0 && abandoned == 0 {
			// Defensive convergence for legacy or inconsistent group rows that had
			// no live Space seat and therefore could not produce a Space outbox job.
			cleanupErr = errors.Join(cleanupErr, m.groupService.RemoveUserFromGroupsForLifecycleCleanup(job.RobotID))
		}
	}

	state, lastError := "done", ""
	if cleanupErr != nil {
		state, lastError = "pending", cleanupErr.Error()
		if len(lastError) > 500 {
			lastError = lastError[:500]
		}
	}
	_, updateErr := m.ctx.DB().Update("avatar_cleanup_job").SetMap(map[string]interface{}{
		"state": state, "attempts": dbr.Expr("attempts+1"), "last_error": lastError,
	}).Where("id=? AND state='running'", job.ID).Exec()
	if updateErr == nil && cleanupErr == nil {
		_, updateErr = m.ctx.DB().Update("robot").Set("lifecycle_pending", 0).
			Where(`robot_id=? AND kind='avatar' AND NOT EXISTS (
				SELECT 1 FROM avatar_cleanup_job j WHERE j.robot_id=? AND j.state IN ('pending','running'))`, job.RobotID, job.RobotID).Exec()
	}
	return errors.Join(cleanupErr, updateErr)
}

// StartAvatarCleanupWorker retries durable external side effects. Authorization
// is always revoked in the creating transaction; this worker only converges IM,
// Redis and group projections and can never reopen access.
func (m *Manager) StartAvatarCleanupWorker() error {
	m.cleanupOnce.Do(func() {
		go func() {
			defer close(m.cleanupDone)
			ticker := time.NewTicker(30 * time.Second)
			defer ticker.Stop()
			m.runPendingAvatarCleanupJobs()
			for {
				select {
				case <-ticker.C:
					m.runPendingAvatarCleanupJobs()
				case <-m.cleanupStop:
					return
				}
			}
		}()
	})
	return nil
}

func (m *Manager) StopAvatarCleanupWorker() error {
	select {
	case <-m.cleanupDone:
		return nil
	default:
	}
	select {
	case <-m.cleanupStop:
	default:
		close(m.cleanupStop)
	}
	// Partial startup and tests can stop before Start. Launching after closing
	// cleanupStop makes the worker exit immediately and closes cleanupDone.
	_ = m.StartAvatarCleanupWorker()
	<-m.cleanupDone
	return nil
}

func (m *Manager) runPendingAvatarCleanupJobs() {
	var jobs []*avatarCleanupJob
	_, err := m.ctx.DB().SelectBySql(`SELECT id,robot_id,operator_uid,operation FROM avatar_cleanup_job
		WHERE state='pending' OR (state='running' AND updated_at < DATE_SUB(NOW(), INTERVAL 1 MINUTE))
		ORDER BY id LIMIT 50`).Load(&jobs)
	if err != nil {
		m.Warn("query pending avatar cleanup jobs failed", zap.Error(err))
		return
	}
	for _, job := range jobs {
		if err := m.runAvatarCleanupJob(job); err != nil {
			m.Warn("avatar cleanup job remains pending", zap.Error(err), zap.Int64("job_id", job.ID), zap.String("robot_id", job.RobotID))
		}
	}
}

func (m *Manager) avatarRetryCleanup(c *wkhttp.Context) {
	identity, _, ok := m.loadManagedAvatar(c)
	if !ok {
		return
	}
	if _, err := spacemod.RetryAbandonedMemberRemovalCleanups(
		m.ctx, identity.UID, spacemod.MemberRemoveReasonBotDeleted,
	); err != nil {
		m.Error("retry downstream avatar cleanup failed", zap.Error(err), zap.String("robot_id", identity.UID))
		httperr.ResponseErrorL(c, errcode.ErrRobotStoreFailed, nil, nil)
		return
	}
	if err := m.runAvatarCleanup(identity.UID); err != nil {
		m.Error("retry avatar cleanup failed", zap.Error(err), zap.String("robot_id", identity.UID))
		httperr.ResponseErrorL(c, errcode.ErrRobotStoreFailed, nil, nil)
		return
	}
	c.ResponseOK()
}

func (m *Manager) avatarRevealToken(c *wkhttp.Context) {
	identity, row, ok := m.loadManagedAvatar(c)
	if !ok {
		return
	}
	if identity.PublicationState == botpolicy.Deleted {
		httperr.ResponseErrorL(c, errcode.ErrRobotNotFound, nil, nil)
		return
	}
	c.Response(map[string]string{"bot_token": row.BotToken})
}

func (m *Manager) avatarRotateToken(c *wkhttp.Context) {
	identity, _, ok := m.loadManagedAvatar(c)
	if !ok {
		return
	}
	if identity.PublicationState == botpolicy.Deleted {
		httperr.ResponseErrorL(c, errcode.ErrRobotNotFound, nil, nil)
		return
	}
	token, err := m.generateUniqueBotToken()
	var jobID int64
	if err == nil {
		tx, beginErr := m.ctx.DB().Begin()
		err = beginErr
		if err == nil {
			defer tx.RollbackUnlessCommitted()
			var publicationState string
			err = tx.SelectBySql(`SELECT publication_state FROM robot
				WHERE robot_id=? AND kind='avatar' FOR UPDATE`, identity.UID).LoadOne(&publicationState)
			if err == nil && publicationState == botpolicy.Deleted {
				err = errors.New("deleted avatar cannot rotate credentials")
			}
			var updated sql.Result
			if err == nil {
				updated, err = tx.Update("robot").SetMap(map[string]interface{}{
					"bot_token": token, "lifecycle_pending": 1,
				}).Where("robot_id=? AND kind='avatar' AND publication_state<>'deleted'", identity.UID).Exec()
			}
			if err == nil {
				var affected int64
				affected, err = updated.RowsAffected()
				if err == nil && affected != 1 {
					err = errors.New("avatar credential state changed concurrently")
				}
			}
			if err == nil {
				result, insertErr := tx.InsertInto("avatar_cleanup_job").Columns("robot_id", "operator_uid", "operation", "state").
					Values(identity.UID, c.GetLoginUID(), "rotate", "pending").Exec()
				err = insertErr
				if err == nil {
					jobID, err = result.LastInsertId()
				}
			}
			if err == nil {
				err = tx.Commit()
			}
		}
	}
	if err != nil {
		m.Error("rotate avatar token failed", zap.Error(err), zap.String("robot_id", identity.UID))
		httperr.ResponseErrorL(c, errcode.ErrRobotStoreFailed, nil, nil)
		return
	}
	if cleanupErr := m.runAvatarCleanupJob(&avatarCleanupJob{ID: jobID, RobotID: identity.UID, Operator: c.GetLoginUID(), Operation: "rotate"}); cleanupErr != nil {
		m.Warn("rotated avatar token; connection cleanup remains pending", zap.Error(cleanupErr), zap.String("robot_id", identity.UID))
	}
	c.Response(map[string]string{"bot_token": token})
}
