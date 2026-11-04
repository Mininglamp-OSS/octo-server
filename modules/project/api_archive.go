package project

import (
	"errors"
	"strconv"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	spacepkg "github.com/Mininglamp-OSS/octo-server/pkg/space"
	"go.uber.org/zap"
)

func (p *Project) rejectArchivedWrite(c *wkhttp.Context, entry string, row *Model) bool {
	if row == nil || !row.ArchivedAt.Valid {
		return false
	}
	p.respondProjectArchivedError(c, entry)
	return true
}

func (p *Project) respondProjectArchivedError(c *wkhttp.Context, entry string) {
	observeRejected(entry, reasonProjectArchived)
	httperr.ResponseErrorL(c, errcode.ErrProjectArchived, nil, nil)
}

func (p *Project) archiveProjectHandler(c *wkhttp.Context) {
	if !p.requireWriteEnabled(c, entryProjectArchive) {
		return
	}
	row := requestProject(c)
	if row == nil {
		p.Error("projectMiddleware 未注入项目行", zap.String("path", c.FullPath()))
		respondQueryFailed(c)
		return
	}
	var req archiveReq
	if err := c.ShouldBindJSON(&req); err != nil || req.Archived == nil {
		respondProjectRequestInvalid(c, "archived")
		return
	}
	updated, actorRole, err := p.setProjectArchived(row.ProjectID, row.SpaceID, c.GetLoginUID(), *req.Archived)
	switch {
	case err == nil:
	case errors.Is(err, errProjectGone):
		respondProjectNotFound(c)
		return
	case errors.Is(err, errActorNotSpaceMember), errors.Is(err, errNotSpaceMember):
		observeRejected(entryProjectArchive, reasonNotSpaceMember)
		httperr.ResponseErrorL(c, errcode.ErrProjectActorNotSpaceMember, nil, nil)
		return
	case errors.Is(err, errPermissionDenied):
		observeRejected(entryProjectArchive, reasonPermissionDenied)
		httperr.ResponseErrorL(c, errcode.ErrProjectPermissionDenied, nil, nil)
		return
	case errors.Is(err, errProjectLifecycleDisabled):
		observeRejected(entryProjectArchive, reasonFlagOff)
		httperr.ResponseErrorL(c, errcode.ErrProjectDisabled, nil, nil)
		return
	case errors.Is(err, errProjectNotReady):
		observeRejected(entryProjectArchive, reasonProjectNotReady)
		httperr.ResponseErrorL(c, errcode.ErrProjectNotReady, nil, nil)
		return
	case errors.Is(err, errProjectArchived):
		p.respondProjectArchivedError(c, entryProjectArchive)
		return
	default:
		p.Error("归档项目失败", zap.Error(err), zap.String("projectId", row.ProjectID))
		respondStoreFailed(c)
		return
	}
	humans, agents := p.splitSeatCounts(updated.ProjectID)
	c.Response(p.toResp(updated, actorRole, requestSpaceRole(c), humans, agents,
		p.pinnedOrFalse(updated.ProjectID, c.GetLoginUID())))
}

func (p *Project) listArchiveNoticesHandler(c *wkhttp.Context) {
	spaceID := spacepkg.GetSpaceID(c)
	uid := c.GetLoginUID()
	result, err := p.readArchiveNotices(spaceID, uid, projectReadPageParams(c))
	if err != nil {
		p.respondProjectReadError(c, err, "查询项目归档提醒失败", spaceID, uid)
		return
	}
	c.Writer.Header().Set("X-Total-Count", strconv.FormatInt(result.Total, 10))
	notices := make([]archiveNoticeResp, 0, len(result.Rows))
	for _, row := range result.Rows {
		archivedAt := ""
		if row.ArchivedAt.Valid {
			archivedAt = formatTime(utcFromColumn(row.ArchivedAt.Time))
		}
		notices = append(notices, archiveNoticeResp{
			ProjectID: row.ProjectID, Name: row.Name, ArchiveVersion: row.ArchiveVersion,
			ArchivedAt: archivedAt,
		})
	}
	c.Response(notices)
}
