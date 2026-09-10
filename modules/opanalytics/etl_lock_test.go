package opanalytics

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestEtlRunLockKeyFor 覆盖锁 key 按目标库命名空间隔离的派生逻辑(2026-09-09 跨库饿死事故的修复)。
func TestEtlRunLockKeyFor(t *testing.T) {
	cases := []struct {
		name string
		dsn  string
		want string
	}{
		{
			name: "test env db",
			dsn:  "user:pass@tcp(10.0.0.1:3306)/im_test?charset=utf8mb4&parseTime=true&loc=Local",
			want: "opanalytics:etl:run:im_test",
		},
		{
			name: "dev env db distinct from test",
			dsn:  "user:pass@tcp(10.0.0.1:3306)/im_dev?charset=utf8mb4&parseTime=true&loc=Local",
			want: "opanalytics:etl:run:im_dev",
		},
		{
			name: "no params still parses db name",
			dsn:  "user:pass@tcp(db:3306)/im_prod",
			want: "opanalytics:etl:run:im_prod",
		},
		{
			name: "unparseable dsn falls back to bare base",
			dsn:  "not a valid dsn",
			want: etlRunLockKeyBase,
		},
		{
			name: "empty db name falls back to bare base",
			dsn:  "user:pass@tcp(db:3306)/",
			want: etlRunLockKeyBase,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, etlRunLockKeyFor(tc.dsn))
		})
	}

	// 核心不变量:不同库派生出不同 key，否则共用同一 Redis 时会互相抢锁饿死。
	assert.NotEqual(t,
		etlRunLockKeyFor("u:p@tcp(h:3306)/im_test"),
		etlRunLockKeyFor("u:p@tcp(h:3306)/im_dev"),
		"per-db lock keys must differ so co-located envs do not starve each other")
}
