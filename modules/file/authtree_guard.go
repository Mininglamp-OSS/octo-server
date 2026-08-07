package file

import (
	"errors"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"go.uber.org/zap"
)

// uploadPathField 是 getUploadCredentials 用来让调用方自选 object key 的 query 参数名。
const uploadPathField = "path"

// rejectCallerObjectKey 把 uk 树上的预签名上传收窄成「只能写服务端新铸的 object key」。
//
// getUploadCredentials 在 path 非空时用 `<type> + sanitizePath(path)` 当 object key，
// 并对它签一个 30 分钟的 PUT URL。签名过程不做任何归属校验：知道另一个租户的 object key
// 就足以拿到对它的写权限，PUT 完成即覆盖原对象。对 uk 来说这条比 human session 更值得堵——
// `uk_*` 是发给 CLI / drive 客户端的长期非交互 bearer token，不像会话 cookie 那样随浏览器
// 关闭而失效。
//
// 删而不校验做不到：path 是 object key 本身，服务端没有「这个 key 属于谁」的事实源可查
// （对象归属表属于后续独立设计）。所以这里 fail-closed 拒绝整个请求，让 object key 只能由
// 服务端生成 —— 调用方失去命名目标的能力，跨租户覆盖就不再可达，而不是依赖一个校验写对。
// 同样的姿态在 bot 树上已经是既成事实：botUploadPresigned 只收 filename + fileSize，从不
// 接受调用方给的 path。
//
// 只挂在 uk 路由上，human 路由与 handler 本身一字未改：human 侧的自定义 path 是 octo-web
// 在用的能力（file/upload/credentials?path=...），收紧它是另一件事。
//
// 下载侧不装对应 guard，那是刻意的取舍而非遗漏：桶按 public-read 建（service_minio.go 的
// readOnlyAnonymousPolicy、service_oss.go 的 oss.ACLPublicRead），对象本就可匿名 GET，
// 所以 object key 在现有部署模型下已经是读取能力本身，只在签名路由上加 Space 校验形不成真实
// 的保密边界。上传侧不同：它影响的是完整性（覆写），而完整性边界确实落在这条签名路由上，
// 堵住它就真的堵住了。详见 pkg/authtree 的 census。
func (f *File) rejectCallerObjectKey() wkhttp.HandlerFunc {
	return func(c *wkhttp.Context) {
		// 判定必须与 getUploadCredentials 里的 `uploadPath != ""` 逐字同形：那里按原始
		// query 值非空就进自选 path 分支，所以这里也不能 TrimSpace —— 否则 `?path=%20`
		// 会绕过 guard 落进同一个分支。读 c.Query 而不是 c.Request.URL.Query()，就是为了
		// 与 handler 读的是同一个值。
		if path := c.Query(uploadPathField); path != "" {
			f.Warn("uk 预签名上传不接受调用方自定义 object key", zap.String("path", path))
			c.ResponseError(errors.New("User API Key 不支持自定义上传路径，请省略 path 参数，由服务端生成对象 key"))
			c.Abort()
			return
		}
		c.Next()
	}
}
