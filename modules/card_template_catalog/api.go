package card_template_catalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl"
	appwkhttp "github.com/Mininglamp-OSS/octo-server/pkg/wkhttp"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"
)

const (
	controlBodyMaxBytes = 2 << 20
	compileTimeout      = 10 * time.Second
	reconcileTimeout    = 30 * time.Second
	reconcileAttempts   = 3
	reconcileRetryDelay = 250 * time.Millisecond
)

type catalogStore interface {
	Publish(context.Context, PublishRequest) (PublishResult, error)
	ReconcileStatic(context.Context, []cardtmpl.TemplateMeta) error
}

type compileArtifactFunc func(context.Context, cardtmpl.Bundle, cardtmpl.CompileLimits) (*cardtmpl.CompiledArtifact, error)

type API struct {
	ctx     *config.Context
	store   catalogStore
	compile compileArtifactFunc
	logger  log.Log
	metrics *catalogMetrics
}

func New(ctx *config.Context) *API {
	metrics := newCatalogMetrics(prometheus.DefaultRegisterer)
	api := &API{
		ctx:     ctx,
		store:   newStore(ctx.DB().DB),
		compile: cardtmpl.CompileJSONArtifact,
		logger:  log.NewTLog("CardTemplateCatalog"),
		metrics: metrics,
	}
	registry := cardtmpl.DefaultRegistry()
	if registry == nil {
		if !ctx.GetConfig().Test {
			panic("card template catalog: built-in Registry is not installed")
		}
		return api
	}
	reconcileCtx, cancel := context.WithTimeout(context.Background(), reconcileTimeout)
	defer cancel()
	if err := reconcileStaticInventory(reconcileCtx, api.store, registry); err != nil {
		api.metrics.observeDB("reconcile_static", "error")
		panic(fmt.Sprintf("card template catalog: reconcile static inventory: %v", err))
	}
	api.metrics.observeDB("reconcile_static", "ok")
	return api
}

func reconcileStaticInventory(ctx context.Context, store catalogStore, registry *cardtmpl.Registry) error {
	if store == nil || registry == nil {
		return fmt.Errorf("%w: store and Registry are required", ErrCatalogIntegrity)
	}
	inventory := registry.List()
	var lastErr error
	for attempt := 1; attempt <= reconcileAttempts; attempt++ {
		lastErr = store.ReconcileStatic(ctx, inventory)
		if lastErr == nil {
			return nil
		}
		if errors.Is(lastErr, ErrCatalogIntegrity) || errors.Is(lastErr, ErrVersionClaimConflict) {
			return lastErr
		}
		if attempt == reconcileAttempts {
			break
		}
		delay := reconcileRetryDelay * time.Duration(1<<(attempt-1))
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return ctx.Err()
		case <-timer.C:
		}
	}
	return fmt.Errorf("reconcile static inventory failed after %d attempts: %w", reconcileAttempts, lastErr)
}

// Route installs a global super-admin control plane. It intentionally does not
// use Space middleware: target Space grants are a later, explicitly scoped PR.
func (a *API) Route(r *wkhttp.WKHttp) {
	manager := r.Group(
		"/v1/manager/card-templates",
		a.ctx.AuthMiddleware(r),
		appwkhttp.SharedUIDRateLimiter(r, a.ctx),
	)
	manager.POST("/validate", a.validate)
	manager.POST("/publish", a.publish)
}

type controlRequest struct {
	Bundle       json.RawMessage `json:"bundle"`
	Reason       string          `json:"reason,omitempty"`
	ChangeTicket string          `json:"change_ticket,omitempty"`
}

type controlResponse struct {
	Hash       string `json:"hash"`
	TemplateID string `json:"template_id"`
	Version    string `json:"version"`
	Owner      string `json:"owner"`
	Engine     string `json:"engine"`
	Visibility string `json:"visibility"`
	Active     bool   `json:"active"`
	Published  bool   `json:"published"`
	Created    bool   `json:"created,omitempty"`
	Idempotent bool   `json:"idempotent,omitempty"`
	Blocked    bool   `json:"blocked,omitempty"`
}

func (a *API) validate(c *wkhttp.Context) {
	operationResult := "rejected"
	defer func() { a.metrics.observeOperation("validate", operationResult) }()
	if !a.requireSuperAdmin(c) {
		return
	}
	_, bundle, ok := readControlRequest(c)
	if !ok {
		return
	}
	artifact, ok := a.compileRequest(c, bundle)
	if !ok {
		return
	}
	operationResult = "ok"
	c.Response(responseForArtifact(artifact, false, PublishResult{}))
}

func (a *API) publish(c *wkhttp.Context) {
	operationResult := "rejected"
	defer func() { a.metrics.observeOperation("publish", operationResult) }()
	if !a.requireSuperAdmin(c) {
		return
	}
	request, bundle, ok := readControlRequest(c)
	if !ok {
		return
	}
	if !boundedRequiredText(request.Reason, maxReasonRunes) ||
		!boundedRequiredText(request.ChangeTicket, maxChangeTicketRunes) {
		respondCatalogRequestInvalid(c, ErrPublishRequestInvalid)
		return
	}
	actorUID := c.GetLoginUID()
	if !boundedRequiredText(actorUID, maxActorUIDRunes) {
		respondCatalogForbidden(c)
		return
	}
	artifact, ok := a.compileRequest(c, bundle)
	if !ok {
		return
	}
	result, err := a.store.Publish(c.Request.Context(), PublishRequest{
		Artifact:     artifact,
		ActorUID:     actorUID,
		Reason:       request.Reason,
		ChangeTicket: request.ChangeTicket,
	})
	if err != nil {
		switch {
		case errors.Is(err, ErrImmutableVersionConflict), errors.Is(err, ErrVersionClaimConflict):
			operationResult = "conflict"
			a.metrics.observeDB("publish", "conflict")
			respondCatalogConflict(c)
		case errors.Is(err, ErrPublishRequestInvalid):
			a.metrics.observeDB("publish", "rejected")
			respondCatalogRequestInvalid(c, err)
		default:
			operationResult = "error"
			a.metrics.observeDB("publish", "error")
			a.logError("publish card template failed", err, artifact)
			respondCatalogUnavailable(c)
		}
		return
	}
	a.metrics.observeDB("publish", "ok")
	operationResult = "ok"
	c.Response(responseForArtifact(artifact, true, result))
}

func (a *API) requireSuperAdmin(c *wkhttp.Context) bool {
	if err := c.CheckLoginRoleIsSuperAdmin(); err != nil {
		respondCatalogForbidden(c)
		return false
	}
	return true
}

func readControlRequest(c *wkhttp.Context) (controlRequest, cardtmpl.Bundle, bool) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, controlBodyMaxBytes)
	raw, err := io.ReadAll(c.Request.Body)
	if err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			respondCatalogContentTooLarge(c)
			return controlRequest{}, cardtmpl.Bundle{}, false
		}
		respondCatalogRequestInvalid(c, err)
		return controlRequest{}, cardtmpl.Bundle{}, false
	}
	var request controlRequest
	if err := cardtmpl.DecodeStrictJSON(raw, &request); err != nil {
		respondCatalogRequestInvalid(c, err)
		return controlRequest{}, cardtmpl.Bundle{}, false
	}
	if len(request.Bundle) == 0 || string(request.Bundle) == "null" {
		respondCatalogRequestInvalid(c, errors.New("bundle is required"))
		return controlRequest{}, cardtmpl.Bundle{}, false
	}
	bundle, err := cardtmpl.DecodeBundleJSON(request.Bundle)
	if err != nil {
		respondCatalogRequestInvalid(c, err)
		return controlRequest{}, cardtmpl.Bundle{}, false
	}
	return request, bundle, true
}

func (a *API) compileRequest(c *wkhttp.Context, bundle cardtmpl.Bundle) (*cardtmpl.CompiledArtifact, bool) {
	started := time.Now()
	compileResult := "error"
	defer func() { a.metrics.observeCompile(compileResult, time.Since(started)) }()
	ctx, cancel := context.WithTimeout(c.Request.Context(), compileTimeout)
	defer cancel()
	artifact, err := a.compile(ctx, bundle, cardtmpl.DefaultCompileLimits())
	if err == nil {
		compileResult = "ok"
		return artifact, true
	}
	var validationErr *cardtmpl.ArtifactValidationError
	if errors.As(err, &validationErr) {
		compileResult = "validation_error"
		respondCatalogRequestInvalid(c, err)
		return nil, false
	}
	a.logError("compile card template failed", err, nil)
	respondCatalogUnavailable(c)
	return nil, false
}

func responseForArtifact(artifact *cardtmpl.CompiledArtifact, published bool, result PublishResult) controlResponse {
	return controlResponse{
		Hash:       artifact.Hash,
		TemplateID: string(artifact.Meta.ID),
		Version:    artifact.Meta.Version,
		Owner:      artifact.Owner,
		Engine:     artifact.Engine,
		Visibility: artifact.Visibility,
		Active:     false,
		Published:  published,
		Created:    result.Created,
		Idempotent: result.Idempotent,
		Blocked:    result.Blocked,
	}
}

func (a *API) logError(message string, err error, artifact *cardtmpl.CompiledArtifact) {
	if a.logger == nil {
		return
	}
	fields := []zap.Field{zap.Error(err)}
	if artifact != nil {
		fields = append(fields,
			zap.String("template_id", string(artifact.Meta.ID)),
			zap.String("version", artifact.Meta.Version),
			zap.String("content_sha256", artifact.Hash),
		)
	}
	a.logger.Error(message, fields...)
}
