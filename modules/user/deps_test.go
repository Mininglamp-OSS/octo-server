package user_test

// 测试基础设施依赖:user 包本身不依赖这些模块,但 testutil.NewTestServer →
// module.Setup 只会跑"已被 import 模块"的迁移。某些 user 测试路径(如
// createUserWithRespAndTx 写 event 表、空间相关的 user 字段 ALTER 等)需要
// 这些模块的表/列已存在,所以这里 blank import 触发它们的 init() 注册。
import (
	_ "github.com/Mininglamp-OSS/octo-server/modules/base"
	_ "github.com/Mininglamp-OSS/octo-server/modules/botfather"
	_ "github.com/Mininglamp-OSS/octo-server/modules/group"
	_ "github.com/Mininglamp-OSS/octo-server/modules/robot"
)
