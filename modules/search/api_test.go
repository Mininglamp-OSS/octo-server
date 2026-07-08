package search

import (
	"database/sql"
	"fmt"
	"os"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-server/modules/document"
	dbbase "github.com/Mininglamp-OSS/octo-server/pkg/db"
	mysqldriver "github.com/go-sql-driver/mysql"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestShouldIncludeGroupForSpace(t *testing.T) {
	tests := []struct {
		name          string
		groupSpaceID  string
		searchSpaceID string
		groupNo       string
		externalMap   map[string]string
		want          bool
	}{
		{"no_space_context_excludes_all", "spaceA", "", "g1", nil, false},
		{"no_space_context_excludes_groups_without_space", "", "", "g1", nil, false},
		{"same_space_included", "spaceA", "spaceA", "g1", nil, true},
		{"different_space_excluded", "spaceB", "spaceA", "g1", nil, false},
		{"group_without_space_excluded_when_filtering", "", "spaceA", "g1", nil, false},
		{"external_group_visible_in_source_space", "spaceA", "spaceB", "g1", map[string]string{"g1": "spaceB"}, true},
		{"external_group_hidden_in_unrelated_space", "spaceA", "spaceC", "g1", map[string]string{"g1": "spaceB"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := shouldIncludeGroupForSpace(tt.groupSpaceID, tt.searchSpaceID, tt.groupNo, tt.externalMap)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestBuildDocumentSearchRespHighlightsNameAndCarriesSourceTrace(t *testing.T) {
	createdAt := dbbase.Time{}
	asset := &document.DocumentAssetModel{
		AssetID:           "doc-1",
		Name:              "Q3 客户现场实施计划.pdf",
		Kind:              document.KindPDF,
		Extension:         ".pdf",
		Size:              2048,
		SourceType:        document.SourceTypeGroup,
		SourceName:        "华东项目交付群",
		SourceChannelID:   "group-east",
		SourceChannelType: common.ChannelTypeGroup.Uint8(),
		SourceMessageID:   "2406171002",
		UploaderName:      "周岚",
		Status:            document.StatusConversation,
	}
	asset.CreatedAt = createdAt

	resp := buildDocumentSearchResp(asset, "华东交付部空间", 91002, "客户")

	assert.Equal(t, "doc-1", resp.ID)
	assert.Equal(t, "Q3 <mark>客户</mark>现场实施计划.pdf", resp.Name)
	assert.Equal(t, document.SourceTypeGroup, resp.SourceType)
	assert.Equal(t, "华东项目交付群", resp.SourceName)
	assert.Equal(t, "group-east", resp.SourceChannelID)
	assert.Equal(t, common.ChannelTypeGroup.Uint8(), resp.SourceChannelType)
	assert.Equal(t, "2406171002", resp.SourceMessageID)
	assert.Equal(t, uint32(91002), resp.SourceMessageSeq)
	assert.Equal(t, "华东交付部空间", resp.SpaceName)
	assert.Equal(t, "周岚", resp.Uploader)
}

func TestDocumentSearchSQLGatesSpaceVisibleDocsBySpaceAccess(t *testing.T) {
	sql := documentSearchSQL()

	assert.Contains(t, sql, "da.document_space_id<>''")
	assert.Contains(t, sql, "ds.status=1")
	assert.Contains(t, sql, "ds.owner_uid=?")
	assert.Contains(t, sql, "document_space_member dsm")
	assert.Contains(t, sql, "dsm.uid=?")
	assert.NotContains(t, sql, "da.visibility=?\n\t\t     OR")
}

func TestSearchDocumentsExecutesSpaceIsolationSQL(t *testing.T) {
	t.Setenv("OCTO_MASTER_KEY", "0123456789abcdef0123456789abcdef")
	ctx := newSearchDocumentDBContext(t)

	_, err := ctx.DB().InsertInto("document_space").
		Columns("space_id", "name", "owner_uid", "tenant_space_id", "status").
		Values("space-visible", "可见空间", "owner", "tenant-a", 1).
		Values("space-disabled", "停用空间", "owner", "tenant-a", 0).
		Values("space-other", "其他空间", "owner", "tenant-a", 1).
		Values("space-cross", "跨租户空间", "owner", "tenant-b", 1).
		Exec()
	require.NoError(t, err)

	_, err = ctx.DB().InsertInto("document_space_member").
		Columns("member_id", "document_space_id", "uid", "name", "role", "tenant_space_id", "status").
		Values("member-visible", "space-visible", "reader", "Reader", document.SpaceRoleViewer, "tenant-a", 1).
		Exec()
	require.NoError(t, err)

	insertAsset := func(assetID, name, tenantSpaceID, documentSpaceID string) {
		t.Helper()
		_, insertErr := ctx.DB().InsertInto("document_asset").
			Columns("asset_id", "name", "kind", "extension", "size", "storage_path", "uploader_uid", "owner_uid", "tenant_space_id", "document_space_id", "visibility", "status").
			Values(assetID, name, document.KindPDF, ".pdf", 1024, "common/"+assetID+".pdf", "owner", "owner", tenantSpaceID, documentSpaceID, document.VisibilitySpace, document.StatusArchived).
			Exec()
		require.NoError(t, insertErr)
	}
	insertAsset("asset-visible", "Quarterly visible.pdf", "tenant-a", "space-visible")
	insertAsset("asset-disabled", "Quarterly disabled.pdf", "tenant-a", "space-disabled")
	insertAsset("asset-other", "Quarterly other.pdf", "tenant-a", "space-other")
	insertAsset("asset-cross", "Quarterly cross.pdf", "tenant-b", "space-cross")

	results, err := New(ctx).searchDocuments("reader", "tenant-a", "Quarterly", 20)
	require.NoError(t, err)

	require.Len(t, results, 1)
	assert.Equal(t, "asset-visible", results[0].ID)
	assert.Equal(t, "可见空间", results[0].SpaceName)
}

func newSearchDocumentDBContext(t *testing.T) *config.Context {
	t.Helper()
	addr := os.Getenv("OCTO_TEST_MYSQL_ADDR")
	if addr == "" {
		addr = "root:demo@tcp(127.0.0.1)/octo_search_document_test?charset=utf8mb4&parseTime=true"
	}
	ensureSearchDocumentDB(t, addr)

	cfg := config.New()
	cfg.Test = true
	cfg.DB.MySQLAddr = addr
	cfg.DB.Migration = false
	ctx := config.NewContext(cfg)

	for _, stmt := range []string{
		"DROP TABLE IF EXISTS `group_member`",
		"DROP TABLE IF EXISTS `group`",
		"DROP TABLE IF EXISTS `document_space_member`",
		"DROP TABLE IF EXISTS `document_asset`",
		"DROP TABLE IF EXISTS `document_space`",
		`CREATE TABLE ` + "`group`" + ` (
			id BIGINT PRIMARY KEY AUTO_INCREMENT,
			group_no VARCHAR(64) NOT NULL,
			space_id VARCHAR(64) NOT NULL DEFAULT '',
			status TINYINT NOT NULL DEFAULT 1
		)`,
		`CREATE TABLE group_member (
			id BIGINT PRIMARY KEY AUTO_INCREMENT,
			group_no VARCHAR(64) NOT NULL,
			uid VARCHAR(64) NOT NULL,
			status TINYINT NOT NULL DEFAULT 1,
			is_deleted TINYINT NOT NULL DEFAULT 0
		)`,
		`CREATE TABLE document_space (
			id BIGINT PRIMARY KEY AUTO_INCREMENT,
			space_id VARCHAR(64) NOT NULL,
			name VARCHAR(128) NOT NULL,
			owner_uid VARCHAR(64) NOT NULL,
			tenant_space_id VARCHAR(64) NOT NULL DEFAULT '',
			status TINYINT NOT NULL DEFAULT 1
		)`,
		`CREATE TABLE document_space_member (
			id BIGINT PRIMARY KEY AUTO_INCREMENT,
			member_id VARCHAR(64) NOT NULL,
			document_space_id VARCHAR(64) NOT NULL,
			uid VARCHAR(64) NOT NULL,
			name VARCHAR(128) NOT NULL DEFAULT '',
			role VARCHAR(32) NOT NULL DEFAULT 'viewer',
			tenant_space_id VARCHAR(64) NOT NULL DEFAULT '',
			status TINYINT NOT NULL DEFAULT 1
		)`,
		`CREATE TABLE document_asset (
			id BIGINT PRIMARY KEY AUTO_INCREMENT,
			asset_id VARCHAR(64) NOT NULL,
			name VARCHAR(255) NOT NULL,
			kind VARCHAR(32) NOT NULL DEFAULT 'doc',
			extension VARCHAR(32) NOT NULL DEFAULT '',
			size BIGINT NOT NULL DEFAULT 0,
			storage_path TEXT,
			source_type VARCHAR(32) NOT NULL DEFAULT '',
			source_channel_id VARCHAR(128) NOT NULL DEFAULT '',
			source_channel_type TINYINT NOT NULL DEFAULT 0,
			source_message_id VARCHAR(64) NOT NULL DEFAULT '',
			source_name VARCHAR(255) NOT NULL DEFAULT '',
			uploader_uid VARCHAR(64) NOT NULL DEFAULT '',
			uploader_name VARCHAR(128) NOT NULL DEFAULT '',
			owner_uid VARCHAR(64) NOT NULL DEFAULT '',
			owner_name VARCHAR(128) NOT NULL DEFAULT '',
			tenant_space_id VARCHAR(64) NOT NULL DEFAULT '',
			document_space_id VARCHAR(64) NOT NULL DEFAULT '',
			visibility VARCHAR(32) NOT NULL DEFAULT 'space',
			status VARCHAR(32) NOT NULL DEFAULT 'archived',
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			last_access_at DATETIME NULL
		)`,
	} {
		_, err := ctx.DB().UpdateBySql(stmt).Exec()
		require.NoError(t, err, "exec schema statement: %s", stmt)
	}
	return ctx
}

func ensureSearchDocumentDB(t *testing.T, addr string) {
	t.Helper()
	parsed, err := mysqldriver.ParseDSN(addr)
	require.NoError(t, err)
	dbName := parsed.DBName
	require.NotEmpty(t, dbName, "test DSN must include a database name")
	parsed.DBName = ""
	db, err := sql.Open("mysql", parsed.FormatDSN())
	require.NoError(t, err)
	defer db.Close()
	_, err = db.Exec(fmt.Sprintf("CREATE DATABASE IF NOT EXISTS `%s` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci", dbName))
	require.NoError(t, err)
}

func TestCollectChannelIDs_ThreadMessage(t *testing.T) {
	tests := []struct {
		name            string
		messages        []*config.MessageResp
		expectGroupIDs  []string
		expectUIDs      []string
		expectFromUIDs  []string
		expectThreadMap map[string]string
	}{
		{
			name: "private_message",
			messages: []*config.MessageResp{
				{ChannelID: "uid_a", ChannelType: common.ChannelTypePerson.Uint8(), FromUID: "uid_b"},
			},
			expectGroupIDs:  []string{},
			expectUIDs:      []string{"uid_a"},
			expectFromUIDs:  []string{"uid_b"},
			expectThreadMap: map[string]string{},
		},
		{
			name: "group_message",
			messages: []*config.MessageResp{
				{ChannelID: "group123", ChannelType: common.ChannelTypeGroup.Uint8(), FromUID: "uid_a"},
			},
			expectGroupIDs:  []string{"group123"},
			expectUIDs:      []string{},
			expectFromUIDs:  []string{"uid_a"},
			expectThreadMap: map[string]string{},
		},
		{
			name: "thread_message_extracts_parent_group",
			messages: []*config.MessageResp{
				{ChannelID: "group123____2044239261124792320", ChannelType: common.ChannelTypeCommunityTopic.Uint8(), FromUID: "uid_a"},
			},
			expectGroupIDs:  []string{"group123"},
			expectUIDs:      []string{},
			expectFromUIDs:  []string{"uid_a"},
			expectThreadMap: map[string]string{"group123____2044239261124792320": "group123"},
		},
		{
			name: "thread_invalid_format_skipped",
			messages: []*config.MessageResp{
				{ChannelID: "no_separator", ChannelType: common.ChannelTypeCommunityTopic.Uint8(), FromUID: "uid_a"},
			},
			expectGroupIDs:  []string{},
			expectUIDs:      []string{},
			expectFromUIDs:  []string{"uid_a"},
			expectThreadMap: map[string]string{},
		},
		{
			name: "mixed_messages",
			messages: []*config.MessageResp{
				{ChannelID: "uid_x", ChannelType: common.ChannelTypePerson.Uint8(), FromUID: "uid_y"},
				{ChannelID: "grp1", ChannelType: common.ChannelTypeGroup.Uint8(), FromUID: "uid_z"},
				{ChannelID: "grp2____20441234", ChannelType: common.ChannelTypeCommunityTopic.Uint8(), FromUID: "uid_w"},
			},
			expectGroupIDs:  []string{"grp1", "grp2"},
			expectUIDs:      []string{"uid_x"},
			expectFromUIDs:  []string{"uid_y", "uid_z", "uid_w"},
			expectThreadMap: map[string]string{"grp2____20441234": "grp2"},
		},
		{
			name:            "empty_messages",
			messages:        []*config.MessageResp{},
			expectGroupIDs:  []string{},
			expectUIDs:      []string{},
			expectFromUIDs:  []string{},
			expectThreadMap: map[string]string{},
		},
		{
			name: "from_uid_empty_not_collected",
			messages: []*config.MessageResp{
				{ChannelID: "uid_a", ChannelType: common.ChannelTypePerson.Uint8(), FromUID: ""},
			},
			expectGroupIDs:  []string{},
			expectUIDs:      []string{"uid_a"},
			expectFromUIDs:  []string{},
			expectThreadMap: map[string]string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			groupIDs, uids, fromUIDs, threadMap := collectChannelIDs(tt.messages)
			assert.Equal(t, tt.expectGroupIDs, groupIDs)
			assert.Equal(t, tt.expectUIDs, uids)
			assert.Equal(t, tt.expectFromUIDs, fromUIDs)
			assert.Equal(t, tt.expectThreadMap, threadMap)
		})
	}
}
