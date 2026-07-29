package card_template_catalog

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl"
	"go.uber.org/zap"
)

const (
	runtimeInitializationRetryDelay = 30 * time.Second
	startupActiveTargetLimit        = 128
	startupTargetValidationTimeout  = 10 * time.Second
)

type runtimeCatalogReadiness struct {
	mu        sync.RWMutex
	ready     bool
	integrity error
}

func newRuntimeCatalogReadiness() *runtimeCatalogReadiness {
	return &runtimeCatalogReadiness{}
}

func (r *runtimeCatalogReadiness) Check() error {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.integrity != nil {
		return fmt.Errorf("%w: startup validation: %v", cardtmpl.ErrRuntimeCatalogIntegrity, r.integrity)
	}
	if !r.ready {
		return cardtmpl.ErrRuntimeCatalogUnavailable
	}
	return nil
}

func (r *runtimeCatalogReadiness) MarkReady() {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.integrity == nil {
		r.ready = true
	}
}

func (r *runtimeCatalogReadiness) MarkIntegrity(err error) {
	if r == nil || err == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.integrity == nil {
		r.integrity = err
		r.ready = false
	}
}

type startupActivationTarget struct {
	ID      cardtmpl.ID
	Version string
}

type runtimeStartupStore interface {
	catalogStore
	ListActiveTargets(context.Context) ([]startupActivationTarget, error)
}

func (a *API) initializeRuntimeCatalog(ctx context.Context) error {
	if a == nil || a.registry == nil {
		return fmt.Errorf("%w: built-in Registry is unavailable", ErrCatalogIntegrity)
	}
	store, ok := a.store.(runtimeStartupStore)
	if !ok || store == nil {
		return fmt.Errorf("%w: runtime startup store is unavailable", ErrCatalogIntegrity)
	}
	reconcileCtx, cancel := context.WithTimeout(ctx, reconcileTimeout)
	err := reconcileStaticInventory(reconcileCtx, store, a.registry)
	cancel()
	if err != nil {
		return err
	}
	listCtx, cancel := context.WithTimeout(ctx, reconcileTimeout)
	targets, err := store.ListActiveTargets(listCtx)
	cancel()
	if a.metrics != nil {
		a.metrics.setActiveTargetCount(len(targets))
	}
	if err != nil {
		return err
	}
	if a.validateStateTarget == nil {
		return fmt.Errorf("%w: active target validator is unavailable", ErrCatalogIntegrity)
	}
	for _, target := range targets {
		targetCtx, targetCancel := context.WithTimeout(ctx, startupTargetValidationTimeout)
		_, err := a.validateStateTarget(targetCtx, target.ID, target.Version)
		targetCancel()
		if err != nil {
			if isRuntimeStartupIntegrityError(err) {
				return fmt.Errorf("%w: active target %s@%s: %v", ErrCatalogIntegrity, target.ID, target.Version, err)
			}
			return fmt.Errorf("validate active target %s@%s: %w", target.ID, target.Version, err)
		}
	}
	return nil
}

func isRuntimeStartupIntegrityError(err error) bool {
	return errors.Is(err, ErrCatalogIntegrity) ||
		errors.Is(err, ErrVersionClaimConflict) ||
		errors.Is(err, ErrActivationTargetInvalid) ||
		errors.Is(err, ErrRollbackTargetInvalid) ||
		errors.Is(err, ErrBlockTargetInvalid) ||
		errors.Is(err, ErrArtifactBlocked) ||
		errors.Is(err, cardtmpl.ErrRuntimeCatalogIntegrity) ||
		errors.Is(err, cardtmpl.ErrTemplateUnknown)
}

// Start begins catalog startup reconciliation without blocking module setup.
// Static exact-version reads remain available while DB-backed catalog paths
// fail closed through runtimeCatalogReadiness.
func (a *API) Start() error {
	if a == nil || a.readiness == nil {
		return nil
	}
	a.startupMu.Lock()
	defer a.startupMu.Unlock()
	if a.startupDone != nil {
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	a.startupCancel = cancel
	a.startupDone = make(chan struct{})
	go a.runRuntimeInitialization(ctx, a.startupDone)
	return nil
}

func (a *API) runRuntimeInitialization(ctx context.Context, done chan<- struct{}) {
	defer close(done)
	for {
		err := a.initializeRuntimeCatalog(ctx)
		if err == nil {
			a.readiness.MarkReady()
			if a.metrics != nil {
				a.metrics.observeDB("startup_validate", "ok")
			}
			return
		}
		if ctx.Err() != nil {
			return
		}
		if isRuntimeStartupIntegrityError(err) {
			a.readiness.MarkIntegrity(err)
			if a.metrics != nil {
				a.metrics.observeDB("startup_validate", "integrity_error")
			}
			if a.logger != nil {
				a.logger.Error("card template catalog startup integrity failure", zap.Error(err))
			}
			return
		}
		if a.metrics != nil {
			a.metrics.observeDB("startup_validate", "error")
		}
		if a.logger != nil {
			a.logger.Error("card template catalog startup validation unavailable; retrying", zap.Error(err))
		}
		timer := time.NewTimer(runtimeInitializationRetryDelay)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return
		case <-timer.C:
		}
	}
}

func (a *API) Stop() error {
	if a == nil {
		return nil
	}
	a.startupMu.Lock()
	cancel, done := a.startupCancel, a.startupDone
	a.startupMu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
	return nil
}
