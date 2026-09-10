package category

import (
	"time"

	"github.com/Mininglamp-OSS/octo-lib/pkg/db"
	"github.com/Mininglamp-OSS/octo-server/modules/project"
)

const (
	sidebarSectionTypeCategory = 1
	sidebarSectionTypeProject  = 2

	sidebarSectionAPITypeCategory = "category"
	sidebarSectionAPITypeProject  = "project"
)

// CategoryModel 群组类别（用户个人视图）
type CategoryModel struct {
	CategoryID string
	SpaceID    string
	UID        string
	Name       string
	Sort       int
	Status     int
	IsDefault  *int
	db.BaseModel
}

func (m *CategoryModel) isDefault() bool {
	return m.IsDefault != nil && *m.IsDefault == 1
}

func intPtr(v int) *int { return &v }

// groupSettingCategoryRow group_setting 表 category 相关字段投影
type groupSettingCategoryRow struct {
	Id           int64
	GroupNo      string
	UID          string
	CategoryID   *string
	CategorySort int
}

// userGroupInfo 用户在 Space 内群组信息（含 category 分配）
type userGroupInfo struct {
	GroupNo      string
	GroupName    string
	CategoryID   *string
	CategorySort int
}

// SidebarSectionModel is the single per-user order for category and Project
// entries in one Space. The timestamps deliberately stay application-written UTC
// values; see the migration for why MySQL timestamp defaults are forbidden here.
type SidebarSectionModel struct {
	ID          int64
	UID         string
	SpaceID     string
	SectionType int
	RefID       string
	Sort        int
	Status      int
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

type sidebarSectionOrderRow struct {
	SectionType int    `db:"section_type"`
	RefID       string `db:"ref_id"`
	Sort        int    `db:"sort"`
}

type sidebarProjectSectionModel struct {
	ProjectID        string `db:"project_id"`
	Name             string `db:"name"`
	Logo             string `db:"logo"`
	AllMemberGroupNo string `db:"all_member_group_no"`
	Sort             int    `db:"sort"`
}

// ---------- Request ----------

type createCategoryReq struct {
	Name string `json:"name"`
}

type updateCategoryReq struct {
	Name string `json:"name"`
}

type sortCategoriesReq struct {
	CategoryIDs []string `json:"category_ids"`
}

type moveGroupToCategoryReq struct {
	CategoryID string `json:"category_id"` // 空字符串表示移出分类
}

type sortSidebarSectionsReq struct {
	Items []sidebarSectionSortItem `json:"items"`
}

type sidebarSectionSortItem struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

// ---------- Response ----------

type categoryResp struct {
	CategoryID *string               `json:"category_id"`
	Name       string                `json:"name"`
	Sort       int                   `json:"sort"`
	IsDefault  bool                  `json:"is_default"`
	Groups     []groupInCategoryResp `json:"groups"`
}

type groupInCategoryResp struct {
	GroupNo      string `json:"group_no"`
	Name         string `json:"name"`
	CategorySort int    `json:"category_sort"`
}

type sidebarSectionResp struct {
	Type     string                     `json:"type"`
	ID       string                     `json:"id"`
	Sort     int                        `json:"sort"`
	Category *categoryResp              `json:"category,omitempty"`
	Project  *sidebarProjectSectionResp `json:"project,omitempty"`
}

type sidebarProjectSectionResp struct {
	ProjectID        string               `json:"project_id"`
	ProjectName      string               `json:"project_name"`
	Logo             string               `json:"logo"`
	AllMemberGroupNo string               `json:"all_member_group_no"`
	Groups           []*project.GroupResp `json:"groups"`
}
