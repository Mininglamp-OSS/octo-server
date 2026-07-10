package doc_binding

import (
	"time"
)

// MountType 枚举挂载点类型；OCT-130 §3.2 只允许这三种，未识别值一律 400。
const (
	MountTypeGroup  = "group"
	MountTypeThread = "thread"
	MountTypeSpace  = "space"
)

// AllowShareCode DB 里 smallint(0/1) 的语义常量，避免散落的 1==true 魔法数字。
const (
	AllowShareCodeOff int = 0
	AllowShareCodeOn  int = 1
)

// Model 对应 doc_binding 表；字段顺序与 SQL 列一致，方便和 sql/ 对读。
// gocraft/dbr AttrToUnderscore 依据字段名自动映射列（Id→id, CreatorUID→creator_uid ...）。
type Model struct {
	Id             int64
	Slug           string
	MountType      string
	GroupNo        string
	ThreadId       string
	SpaceId        string
	CreatorUID     string
	AllowShareCode int
	CreatedAt      time.Time
	UpdatedAt      time.Time
}
