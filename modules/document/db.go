package document

import (
	"fmt"
	"strings"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/gocraft/dbr/v2"
)

type documentDB struct {
	ctx     *config.Context
	session *dbr.Session
}

func newDocumentDB(ctx *config.Context) *documentDB {
	return &documentDB{
		ctx:     ctx,
		session: ctx.DB(),
	}
}

func (d *documentDB) ListSpaces(uid, tenantSpaceID string) ([]*DocumentSpaceModel, error) {
	var spaces []*DocumentSpaceModel
	_, err := d.session.SelectBySql(
		`SELECT ds.*
		 FROM document_space ds
		 WHERE ds.tenant_space_id=? AND ds.status=1
		   AND (
		     ds.owner_uid=?
		     OR EXISTS (
		       SELECT 1
		       FROM document_space_member dsm
		       WHERE dsm.tenant_space_id=ds.tenant_space_id
		         AND dsm.document_space_id=ds.space_id
		         AND dsm.uid=?
		         AND dsm.status=1
		     )
		   )
		 ORDER BY ds.created_at DESC`,
		tenantSpaceID,
		uid,
		uid,
	).Load(&spaces)
	return spaces, err
}

func (d *documentDB) ListAllSpaces(uid, tenantSpaceID string) ([]*DocumentSpaceModel, error) {
	var spaces []*DocumentSpaceModel
	_, err := d.session.Select("*").From("document_space").
		Where("tenant_space_id=?", tenantSpaceID).
		OrderDesc("created_at").
		Load(&spaces)
	return spaces, err
}

func (d *documentDB) EnsureDefaultSpace(uid, tenantSpaceID string) (*DocumentSpaceModel, error) {
	spaces, err := d.ListSpaces(uid, tenantSpaceID)
	if err != nil {
		return nil, err
	}
	if len(spaces) > 0 {
		return spaces[0], nil
	}
	now := nowDBTime()
	space := &DocumentSpaceModel{
		SpaceID:       "DOCSPACE-" + util.GenerUUID(),
		Name:          "团队文档空间",
		Description:   "默认文档空间",
		OwnerUID:      uid,
		TenantSpaceID: tenantSpaceID,
		Status:        1,
	}
	space.CreatedAt = now
	space.UpdatedAt = now
	if _, err := d.session.InsertInto("document_space").Columns(util.AttrToUnderscore(space)...).Record(space).Exec(); err != nil {
		return nil, err
	}
	return space, nil
}

func (d *documentDB) GetSpace(spaceID, uid, tenantSpaceID string) (*DocumentSpaceModel, error) {
	var space *DocumentSpaceModel
	_, err := d.session.Select("*").From("document_space").
		Where("space_id=? and tenant_space_id=? and status=1", spaceID, tenantSpaceID).
		Load(&space)
	return space, err
}

func (d *documentDB) SaveSpace(space *DocumentSpaceModel) error {
	_, err := d.session.InsertInto("document_space").Columns(util.AttrToUnderscore(space)...).Record(space).Exec()
	return err
}

func (d *documentDB) UpdateSpace(space *DocumentSpaceModel) error {
	_, err := d.session.Update("document_space").
		Set("name", space.Name).
		Set("description", space.Description).
		Set("owner_uid", space.OwnerUID).
		Set("status", space.Status).
		Set("updated_at", time.Now()).
		Where("space_id=? and tenant_space_id=?", space.SpaceID, space.TenantSpaceID).
		Exec()
	return err
}

func (d *documentDB) ListSpaceBindings(uid, tenantSpaceID string) ([]*DocumentSpaceBindingModel, error) {
	var bindings []*DocumentSpaceBindingModel
	_, err := d.session.Select("*").From("document_space_binding").
		Where("tenant_space_id=? and status=1", tenantSpaceID).
		OrderDesc("created_at").
		Load(&bindings)
	return bindings, err
}

func (d *documentDB) SaveSpaceBinding(binding *DocumentSpaceBindingModel) error {
	tx, err := d.session.Begin()
	if err != nil {
		return err
	}
	defer tx.RollbackUnlessCommitted()

	if _, err := tx.Update("document_space_binding").
		Set("status", 0).
		Set("updated_at", time.Now()).
		Where("tenant_space_id=? and source_channel_id=? and source_channel_type=? and document_space_id<>?",
			binding.TenantSpaceID,
			binding.SourceChannelID,
			binding.SourceChannelType,
			binding.DocumentSpaceID,
		).
		Exec(); err != nil {
		return err
	}
	_, err = tx.InsertBySql(
		`INSERT INTO document_space_binding
			(binding_id, document_space_id, source_channel_id, source_channel_type, source_name, created_by, tenant_space_id, status, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON DUPLICATE KEY UPDATE
			source_name=VALUES(source_name),
			created_by=VALUES(created_by),
			status=VALUES(status),
			updated_at=VALUES(updated_at)`,
		binding.BindingID,
		binding.DocumentSpaceID,
		binding.SourceChannelID,
		binding.SourceChannelType,
		binding.SourceName,
		binding.CreatedBy,
		binding.TenantSpaceID,
		binding.Status,
		binding.CreatedAt.String(),
		binding.UpdatedAt.String(),
	).Exec()
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (d *documentDB) RemoveSpaceBinding(spaceID, bindingID, tenantSpaceID string) error {
	_, err := d.session.Update("document_space_binding").
		Set("status", 0).
		Set("updated_at", time.Now()).
		Where("document_space_id=? and binding_id=? and tenant_space_id=?", spaceID, bindingID, tenantSpaceID).
		Exec()
	return err
}

func (d *documentDB) ListSpaceMembers(uid, tenantSpaceID string) ([]*DocumentSpaceMemberModel, error) {
	var members []*DocumentSpaceMemberModel
	_, err := d.session.Select("*").From("document_space_member").
		Where("tenant_space_id=? and status=1", tenantSpaceID).
		OrderDesc("created_at").
		Load(&members)
	return members, err
}

func (d *documentDB) SearchUsers(uid, tenantSpaceID, keyword string, limit int) ([]*DocumentUserCandidateModel, error) {
	keyword = strings.TrimSpace(keyword)
	if keyword == "" {
		return []*DocumentUserCandidateModel{}, nil
	}
	if limit <= 0 || limit > 20 {
		limit = 20
	}
	like := "%" + keyword + "%"
	var users []*DocumentUserCandidateModel
	_, err := d.session.SelectBySql(
		`SELECT DISTINCT
			u.uid,
			IFNULL(u.name, '') AS name,
			IFNULL(u.username, '') AS username,
			IFNULL(u.email, '') AS email,
			IFNULL(u.phone, '') AS phone
		 FROM `+"`user`"+` u
		 INNER JOIN space_member sm ON sm.uid=u.uid AND sm.space_id=? AND sm.status=1
		 WHERE u.status=1
		   AND IFNULL(u.robot,0)=0
		   AND (u.uid LIKE ? OR u.name LIKE ? OR u.username LIKE ? OR u.email LIKE ? OR u.phone LIKE ?)
		 ORDER BY name ASC, username ASC
		 LIMIT ?`,
		tenantSpaceID,
		like,
		like,
		like,
		like,
		like,
		limit,
	).Load(&users)
	return users, err
}

func (d *documentDB) SearchGroups(uid, tenantSpaceID, keyword string, limit int) ([]*DocumentGroupCandidateModel, error) {
	keyword = strings.TrimSpace(keyword)
	if keyword == "" {
		return []*DocumentGroupCandidateModel{}, nil
	}
	if limit <= 0 || limit > 20 {
		limit = 20
	}
	like := "%" + keyword + "%"
	var groups []*DocumentGroupCandidateModel
	_, err := d.session.SelectBySql(
		`SELECT g.group_no, IFNULL(g.name, '') AS name, IFNULL(g.space_id, '') AS space_id
		 FROM group_member gm
		 INNER JOIN `+"`group`"+` g ON g.group_no=gm.group_no AND g.status=1
		 WHERE gm.uid=? AND gm.is_deleted=0 AND gm.status=1
		   AND (g.space_id=? OR IFNULL(g.space_id, '')='' OR IFNULL(gm.source_space_id, '')=?)
		   AND (g.group_no LIKE ? OR g.name LIKE ?)
		 ORDER BY g.updated_at DESC
		 LIMIT ?`,
		uid,
		tenantSpaceID,
		tenantSpaceID,
		like,
		like,
		limit,
	).Load(&groups)
	return groups, err
}

func (d *documentDB) SaveSpaceMember(member *DocumentSpaceMemberModel) error {
	_, err := d.session.InsertBySql(
		`INSERT INTO document_space_member
			(member_id, document_space_id, uid, name, role, source, created_by, tenant_space_id, status, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON DUPLICATE KEY UPDATE
			name=VALUES(name),
			role=VALUES(role),
			status=VALUES(status),
			updated_at=VALUES(updated_at)`,
		member.MemberID,
		member.DocumentSpaceID,
		member.UID,
		member.Name,
		member.Role,
		member.Source,
		member.CreatedBy,
		member.TenantSpaceID,
		member.Status,
		member.CreatedAt.String(),
		member.UpdatedAt.String(),
	).Exec()
	return err
}

func (d *documentDB) RemoveSpaceMember(spaceID, memberUID, tenantSpaceID string) error {
	_, err := d.session.Update("document_space_member").
		Set("status", 0).
		Set("updated_at", time.Now()).
		Where("document_space_id=? and uid=? and tenant_space_id=?", spaceID, memberUID, tenantSpaceID).
		Exec()
	return err
}

func (d *documentDB) ListAssets(uid, tenantSpaceID string) ([]*DocumentAssetModel, error) {
	var assets []*DocumentAssetModel
	_, err := d.session.Select("*").From("document_asset").
		Where("tenant_space_id=?", tenantSpaceID).
		OrderDesc("last_access_at").
		OrderDesc("created_at").
		Load(&assets)
	if err == nil {
		d.populateSourceMessageSeqs(assets)
	}
	return assets, err
}

func (d *documentDB) GetAsset(assetID, uid, tenantSpaceID string) (*DocumentAssetModel, error) {
	var asset *DocumentAssetModel
	_, err := d.session.Select("*").From("document_asset").
		Where("asset_id=? and tenant_space_id=?", assetID, tenantSpaceID).
		Load(&asset)
	if err == nil && asset != nil {
		d.populateSourceMessageSeqs([]*DocumentAssetModel{asset})
	}
	return asset, err
}

type documentMessageSeqRow struct {
	MessageID   string `db:"message_id"`
	ClientMsgNo string `db:"client_msg_no"`
	MessageSeq  uint32 `db:"message_seq"`
}

func (d *documentDB) populateSourceMessageSeqs(assets []*DocumentAssetModel) {
	messageIDs := make([]string, 0, len(assets))
	for _, asset := range assets {
		if asset.SourceMessageID != "" {
			messageIDs = append(messageIDs, asset.SourceMessageID)
		}
	}
	if len(messageIDs) == 0 {
		return
	}

	seqs := make(map[string]uint32)
	for _, table := range []string{"message", "message1", "message2", "message3", "message4"} {
		rows := make([]*documentMessageSeqRow, 0)
		_, err := d.session.SelectBySql(
			fmt.Sprintf("SELECT message_id, client_msg_no, message_seq FROM `%s` WHERE message_id IN ? OR client_msg_no IN ?", table),
			messageIDs,
			messageIDs,
		).Load(&rows)
		if err != nil {
			continue
		}
		for _, row := range rows {
			if row.MessageID != "" {
				seqs[row.MessageID] = row.MessageSeq
			}
			if row.ClientMsgNo != "" {
				seqs[row.ClientMsgNo] = row.MessageSeq
			}
		}
	}
	for _, asset := range assets {
		asset.SourceMessageSeq = seqs[asset.SourceMessageID]
	}
}

func (d *documentDB) SaveAsset(asset *DocumentAssetModel) error {
	var lastAccessAt interface{}
	if asset.LastAccessAt != nil {
		lastAccessAt = asset.LastAccessAt.String()
	}
	_, err := d.session.InsertBySql(
		`INSERT INTO document_asset
			(asset_id, name, kind, extension, size, storage_path,
			 source_type, source_channel_id, source_channel_type, source_message_id, source_name,
			 uploader_uid, uploader_name, owner_uid, owner_name, tenant_space_id,
			 document_space_id, original_space_id, visibility, status, downloads, previewable,
			 last_access_at, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?,
			 ?, ?, ?, ?, ?,
			 ?, ?, ?, ?, ?,
			 ?, ?, ?, ?, ?, ?,
			 ?, ?, ?)`,
		asset.AssetID,
		asset.Name,
		asset.Kind,
		asset.Extension,
		asset.Size,
		asset.StoragePath,
		asset.SourceType,
		asset.SourceChannelID,
		asset.SourceChannelType,
		asset.SourceMessageID,
		asset.SourceName,
		asset.UploaderUID,
		asset.UploaderName,
		asset.OwnerUID,
		asset.OwnerName,
		asset.TenantSpaceID,
		asset.DocumentSpaceID,
		asset.OriginalSpaceID,
		asset.Visibility,
		asset.Status,
		asset.Downloads,
		asset.Previewable,
		lastAccessAt,
		asset.CreatedAt.String(),
		asset.UpdatedAt.String(),
	).Exec()
	return err
}

func (d *documentDB) UpdateAsset(asset *DocumentAssetModel) error {
	var lastAccessAt interface{}
	if asset.LastAccessAt != nil {
		lastAccessAt = asset.LastAccessAt.String()
	}
	_, err := d.session.Update("document_asset").
		Set("name", asset.Name).
		Set("kind", asset.Kind).
		Set("extension", asset.Extension).
		Set("document_space_id", asset.DocumentSpaceID).
		Set("original_space_id", asset.OriginalSpaceID).
		Set("visibility", asset.Visibility).
		Set("status", asset.Status).
		Set("downloads", asset.Downloads).
		Set("last_access_at", lastAccessAt).
		Set("updated_at", time.Now()).
		Where("asset_id=? and tenant_space_id=?", asset.AssetID, asset.TenantSpaceID).
		Exec()
	return err
}

func (d *documentDB) DeleteAsset(assetID, tenantSpaceID string) error {
	_, err := d.session.DeleteFrom("document_asset").
		Where("asset_id=? and tenant_space_id=?", assetID, tenantSpaceID).
		Exec()
	return err
}

func (d *documentDB) AddEvent(event *DocumentEventModel) error {
	_, err := d.session.InsertInto("document_asset_event").Columns(util.AttrToUnderscore(event)...).Record(event).Exec()
	return err
}

func (d *documentDB) ListEvents(uid, tenantSpaceID string, limit int) ([]*DocumentEventModel, error) {
	var events []*DocumentEventModel
	query := d.session.Select("*").From("document_asset_event").
		Where("tenant_space_id=?", tenantSpaceID).
		OrderDesc("created_at").
		OrderDesc("id")
	if limit > 0 {
		query = query.Limit(uint64(limit))
	}
	_, err := query.Load(&events)
	return events, err
}

func (d *documentDB) CanAccessSource(uid, tenantSpaceID, sourceChannelID string, sourceChannelType uint8) (bool, error) {
	if sourceChannelID == "" {
		return false, nil
	}
	if sourceChannelType == 2 {
		var count int
		_, err := d.session.SelectBySql(
			`SELECT count(*)
			 FROM group_member gm
			 INNER JOIN `+"`group`"+` g ON g.group_no=gm.group_no AND g.status=1 AND g.space_id=?
			 WHERE gm.group_no=? AND gm.uid=? AND gm.status=1 AND gm.is_deleted=0`,
			tenantSpaceID,
			sourceChannelID,
			uid,
		).Load(&count)
		return count > 0, err
	}
	return sourceChannelID == uid, nil
}

func (d *documentDB) CanAccessSources(uid, tenantSpaceID string, sources []documentSourceRef) (map[string]bool, error) {
	result := make(map[string]bool, len(sources))
	groupIDs := make([]string, 0, len(sources))
	seenGroups := make(map[string]struct{}, len(sources))
	for _, source := range sources {
		if strings.TrimSpace(source.ChannelID) == "" {
			continue
		}
		if source.ChannelType != 2 {
			if source.ChannelID == uid {
				result[documentSourceKey(source.ChannelID, source.ChannelType)] = true
			}
			continue
		}
		if _, ok := seenGroups[source.ChannelID]; ok {
			continue
		}
		seenGroups[source.ChannelID] = struct{}{}
		groupIDs = append(groupIDs, source.ChannelID)
	}
	if len(groupIDs) == 0 {
		return result, nil
	}

	rows := make([]struct {
		GroupNo string `db:"group_no"`
	}, 0, len(groupIDs))
	_, err := d.session.SelectBySql(
		`SELECT DISTINCT gm.group_no
		 FROM group_member gm
		 INNER JOIN `+"`group`"+` g ON g.group_no=gm.group_no AND g.status=1 AND g.space_id=?
		 WHERE gm.group_no IN ?
		   AND gm.uid=?
		   AND gm.status=1
		   AND gm.is_deleted=0`,
		tenantSpaceID,
		groupIDs,
		uid,
	).Load(&rows)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		result[documentSourceKey(row.GroupNo, 2)] = true
	}
	return result, nil
}
