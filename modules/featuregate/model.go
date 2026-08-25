package featuregate

import "github.com/Mininglamp-OSS/octo-lib/pkg/db"

// gateModel 对应 octo_feature_gate 表（每个功能 key 一行规则）。
// 嵌入 BaseModel 提供 id/created_at/updated_at；util.AttrToUnderscore 会跳过
// 嵌入的 struct，故 INSERT 仅写顶层标量列，时间戳交给 DB 默认值。
type gateModel struct {
	FeatureKey  string
	Mode        string
	Percent     int
	BucketBy    string
	Description string
	db.BaseModel
}

// scopeModel 对应 octo_feature_gate_scope 表（白名单条目，逐条加删）。
type scopeModel struct {
	FeatureKey string
	ScopeType  string
	ScopeID    string
	db.BaseModel
}
