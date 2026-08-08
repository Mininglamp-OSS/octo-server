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
// 下载侧不装对应 guard，是「另一层的问题，本次不解」而不是「这里没有问题」—— 措辞要准，
// 因为本注释的初版拿一个代码并不具备的性质当理由。对象保密在本仓今天哪一层都没有强制：
// modules/file 的六个后端里只有 minio 与 oss 会加 public-read，且只在自己建桶那条路径上
// （service_minio.go 的 ensureBucket 在 BucketExists 为真时直接 return，SetBucketPolicy
// 对运维预建的桶根本不会执行；service_oss.go 的 CreateBucket(oss.ACLPublicRead) 挂在
// `bucket == nil` 之下，而 client.Bucket 不会返回 nil），cos / s3 / qiniu / seaweedfs
// 一个都不加。service_s3.go 自己的 DownloadURL 注释写的正是反面：private bucket 是
// 「the AWS default with Block Public Access on」，并点名 /v1/file/download/url 就是为
// 这种部署存在的签名路由。
//
// 也就是说私有桶部署下，签名 URL 就是读取能力本身，绑定 A 的 key 拿 B 的 object key 能换到
// 一个可用的 GET。真正压住它的不是这条路由上的边界：object key 是
// <type>/<ts>/<uuid>/<uuid><ext>，高熵不可枚举，需要 key 先从别处泄漏；本 PR 加的消息读
// 路由都是 Space-confined，A 的 key 无法通过它们收割 B 的 key；暴露面与已在生产的 human
// session 路由等同。要真堵得靠归属表 / capability token / bucket 策略——在这条路由之下的
// 一层，且同样覆盖 human 路由——属于独立追踪的工作。上传侧不同：它影响的是完整性（覆写），
// 而完整性边界确实落在这条签名路由上，堵住它就真的堵住了。详见 pkg/authtree 的 census。
func (f *File) rejectCallerObjectKey() wkhttp.HandlerFunc {
	return func(c *wkhttp.Context) {
		// 判定必须与 getUploadCredentials 里的 `uploadPath != ""` 逐字同形：那里按原始
		// query 值非空就进自选 path 分支，所以这里也不能 TrimSpace —— 否则 `?path=%20`
		// 会绕过 guard 落进同一个分支。读 c.Query 而不是 c.Request.URL.Query()，就是为了
		// 与 handler 读的是同一个值。
		if path := c.Query(uploadPathField); path != "" {
			f.Warn("uk 预签名上传不接受调用方自定义 object key", zap.String("path", path))
			// 刻意沿用本模块的 legacy 错误格式（c.ResponseError）而不是 errcode +
			// httperr.ResponseErrorL：同一个 endpoint 的其它拒绝（fileSize 非法、扩展名
			// 不允许、sticker 走预签名）全部是这个格式，调用方按它解析。这条 guard 单独
			// 换成本地化 envelope，只会让同一路由的错误形状自相矛盾。等 modules/file
			// 整体迁移时一并换。
			c.ResponseError(errors.New("User API Key 不支持自定义上传路径，请省略 path 参数，由服务端生成对象 key"))
			c.Abort()
			return
		}
		c.Next()
	}
}
