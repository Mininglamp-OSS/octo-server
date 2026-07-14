package resource_share

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/internal/resourceshare"
	"github.com/Mininglamp-OSS/octo-server/pkg/reqid"
	spacepkg "github.com/Mininglamp-OSS/octo-server/pkg/space"
	appwkhttp "github.com/Mininglamp-OSS/octo-server/pkg/wkhttp"
	"go.uber.org/zap"
)

const maxRequestBodyBytes = 128 << 10

type shareService interface {
	Share(ctx context.Context, loginUID, spaceID, compactIntent, requestID string) (*resourceshare.ShareResult, error)
}

type API struct {
	ctx      *config.Context
	service  shareService
	verifier *resourceshare.ProofVerifier
	log.Log
}

func New(ctx *config.Context) *API {
	enabled, err := featureEnabledFromEnv()
	if err != nil {
		panic(err)
	}
	service, err := resourceshare.NewShareService(resourceshare.ShareServiceDependencies{
		FeatureEnabled: func() bool { return enabled },
	})
	if err != nil {
		// The foundation intentionally ships with no enabled providers or proof
		// signer. Enabling the route before provider onboarding is a startup
		// configuration error, not a degraded mode.
		panic(err)
	}
	return newAPI(ctx, service, nil)
}

func newAPI(ctx *config.Context, service shareService, verifier *resourceshare.ProofVerifier) *API {
	return &API{
		ctx: ctx, service: service, verifier: verifier,
		Log: log.NewTLog("resource_share"),
	}
}

func featureEnabledFromEnv() (bool, error) {
	raw := strings.TrimSpace(os.Getenv("DM_RESOURCE_SHARE_ENABLED"))
	if raw == "" {
		return false, nil
	}
	enabled, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("resource share: invalid DM_RESOURCE_SHARE_ENABLED: %w", err)
	}
	return enabled, nil
}

func (a *API) Route(r *wkhttp.WKHttp) {
	// Public verification material must remain readable while new sharing is
	// rolled back, otherwise already-persisted cards would become unverifiable.
	r.GET("/v1/resource-shares/proof-jwks", a.proofJWKS)

	authenticated := r.Group("/v1/resource-shares",
		a.ctx.AuthMiddleware(r),
		appwkhttp.SharedUIDRateLimiter(r, a.ctx),
		spacepkg.SpaceMiddleware(a.ctx),
	)
	authenticated.POST("", a.share)
}

func (a *API) share(c *wkhttp.Context) {
	spaceHeader := c.GetHeader("X-Space-ID")
	spaceID := spacepkg.GetSpaceID(c)
	if c.GetLoginUID() == "" || !validSpaceHeader(spaceHeader) || spaceID != spaceHeader {
		respondRequestInvalid(c, "space")
		return
	}

	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxRequestBodyBytes)
	decoder := json.NewDecoder(c.Request.Body)
	decoder.DisallowUnknownFields()
	var request struct {
		Intent string `json:"intent"`
	}
	if err := decoder.Decode(&request); err != nil {
		respondDecodeError(c, err)
		return
	}
	if strings.TrimSpace(request.Intent) == "" || request.Intent != strings.TrimSpace(request.Intent) {
		respondRequestInvalid(c, "intent")
		return
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err != nil {
			respondDecodeError(c, err)
		} else {
			respondRequestInvalid(c, "body")
		}
		return
	}

	requestID := reqid.FromContext(c.Request.Context())
	if requestID == "" {
		requestID = reqid.Sanitize(c.GetString(reqid.GinKey))
	}
	if requestID == "" {
		requestID = reqid.New()
	}
	result, err := a.service.Share(
		c.Request.Context(), c.GetLoginUID(), spaceID, request.Intent, requestID,
	)
	if err != nil {
		a.logShareFailure(requestID, err)
		respondShareError(c, err)
		return
	}
	if result == nil {
		a.Error("resource share returned a nil result", zap.String("request_id", requestID))
		respondUnavailable(c)
		return
	}
	c.Response(result)
}

func (a *API) proofJWKS(c *wkhttp.Context) {
	c.Header("Cache-Control", "public, max-age=300, must-revalidate")
	if a.verifier == nil {
		c.Response(map[string]interface{}{"keys": []interface{}{}})
		return
	}
	c.Response(a.verifier.JWKS())
}

func (a *API) logShareFailure(requestID string, err error) {
	if isExpectedShareRejection(err) {
		a.Warn("resource share rejected",
			zap.String("request_id", requestID),
			zap.String("error_class", shareErrorClass(err)))
		return
	}
	a.Error("resource share failed",
		zap.String("request_id", requestID),
		zap.String("error_class", shareErrorClass(err)),
		zap.Error(err))
}

func validSpaceHeader(spaceID string) bool {
	if spaceID == "" || len(spaceID) > 128 || spaceID != strings.TrimSpace(spaceID) {
		return false
	}
	for _, character := range spaceID {
		if character < 0x20 || character == 0x7f {
			return false
		}
	}
	return true
}
