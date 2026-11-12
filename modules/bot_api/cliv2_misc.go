package bot_api

import (
	"net/http"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/modules/file"
	"github.com/Mininglamp-OSS/octo-server/pkg/cliv2"
	"github.com/Mininglamp-OSS/octo-server/pkg/space"
	"github.com/gin-gonic/gin"
)

// registerCLIMiscRoutes contributes the CLI v2 file handlers.
func (ba *BotAPI) registerCLIMiscRoutes(source *wkhttp.WKHttp) {
	// Intentional Space-isolation exception: preserve legacy object-key download
	// semantics; object ownership is not checked against the token's Space.
	cliv2.Add(source, cliv2.Route{
		Method:      http.MethodGet,
		Path:        "/v2/cli/files/_download",
		Action:      cliv2.ActionFilesRead,
		User:        ba.cliFileDownload,
		Bot:         ba.cliFileDownload,
		Middlewares: []wkhttp.HandlerFunc{ignoreCLIDownloadSpaceSelectors, space.SpaceMiddleware(ba.ctx)},
	})
	cliv2.Add(source, cliv2.Route{
		Method: http.MethodPost,
		Path:   "/v2/cli/files",
		Action: cliv2.ActionFilesWrite,
		Bot:    file.CLIV2UploadHandler(ba.ctx, ba.fileService),
	})
	cliv2.Add(source, cliv2.Route{
		Method: http.MethodGet,
		Path:   "/v2/cli/uploads/credentials",
		Action: cliv2.ActionFilesWrite,
		Bot:    file.CLIV2CredentialsHandler(ba.ctx),
	})
	cliv2.Add(source, cliv2.Route{
		Method: http.MethodGet,
		Path:   "/v2/cli/uploads/_presigned",
		Action: cliv2.ActionFilesWrite,
		Bot:    file.CLIV2PresignedUploadHandler(ba.ctx, ba.fileService),
	})
}

// The legacy download ignores caller-selected Space; the SDK already supplies
// the token's Space, so do not let untrusted selectors alter this request.
func ignoreCLIDownloadSpaceSelectors(c *wkhttp.Context) {
	query := c.Request.URL.Query()
	if query.Has("space_id") {
		query.Del("space_id")
		c.Request.URL.RawQuery = query.Encode()
	}
	c.Request.Header.Del("X-Space-ID")
	c.Next()
}

func (ba *BotAPI) cliFileDownload(c *wkhttp.Context) {
	c.Params = append(c.Params, gin.Param{Key: "path", Value: c.Query("path")})
	ba.botFileDownload(c)
}
