package agentmailgateway

import (
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n/codes"
)

func respondGatewayError(c *wkhttp.Context, code codes.Code) {
	httperr.ResponseErrorLWithStatus(c, code, nil, nil)
}
