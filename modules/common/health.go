package common

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	octoredis "github.com/Mininglamp-OSS/octo-server/pkg/redis"
	rd "github.com/go-redis/redis"
	"go.uber.org/zap"
)

const (
	healthStatusUp   = "up"
	healthStatusDown = "down"

	readinessProbeTimeout     = 500 * time.Millisecond
	readinessRedisPoolTimeout = 100 * time.Millisecond
)

type readinessResult struct {
	Status       string            `json:"status"`
	Dependencies map[string]string `json:"dependencies,omitempty"`
	Errors       map[string]error  `json:"-"`
}

type readinessChecker interface {
	Check(ctx context.Context) readinessResult
}

type dependencyReadinessChecker struct {
	db          *db
	redisClient *rd.Client
}

func newDependencyReadinessChecker(ctx *config.Context, db *db) readinessChecker {
	return &dependencyReadinessChecker{
		db:          db,
		redisClient: rd.NewClient(readinessRedisOptions(ctx.GetConfig())),
	}
}

func readinessRedisOptions(cfg *config.Config) *rd.Options {
	return octoredis.MustBuildOptions(cfg, func(o *rd.Options) {
		o.MaxRetries = 0
		o.PoolSize = 2
		o.DialTimeout = readinessProbeTimeout
		o.ReadTimeout = readinessProbeTimeout
		o.WriteTimeout = readinessProbeTimeout
		o.PoolTimeout = readinessRedisPoolTimeout
	})
}

func (cn *Common) health(c *wkhttp.Context) {
	// db/redis are static legacy-shape fields; liveness must stay dependency-free.
	c.JSON(http.StatusOK, map[string]string{
		"status": healthStatusUp,
		"db":     healthStatusUp,
		"redis":  healthStatusUp,
	})
}

func (cn *Common) ready(c *wkhttp.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), readinessProbeTimeout)
	defer cancel()

	checker := cn.readinessChecker
	if checker == nil {
		result := readinessResult{
			Status:       healthStatusDown,
			Dependencies: map[string]string{"db": healthStatusDown, "redis": healthStatusDown},
			Errors:       map[string]error{"readiness": errors.New("readiness checker unavailable")},
		}
		cn.logReadinessErrors(result.Errors)
		// Readiness is an infra probe contract, not a user-facing business error.
		// Keep the wire body small and safe instead of returning the i18n envelope.
		c.JSON(http.StatusServiceUnavailable, result)
		return
	}

	resultCh := make(chan readinessResult, 1)
	go func() {
		resultCh <- checker.Check(ctx)
	}()

	var result readinessResult
	select {
	case result = <-resultCh:
	case <-ctx.Done():
		result = readinessResult{
			Status: healthStatusDown,
			Dependencies: map[string]string{
				"db":    healthStatusDown,
				"redis": healthStatusDown,
			},
			Errors: map[string]error{"readiness": ctx.Err()},
		}
	}
	cn.logReadinessErrors(result.Errors)
	statusCode := http.StatusOK
	if result.Status != healthStatusUp {
		statusCode = http.StatusServiceUnavailable
	}
	// Readiness is an infra probe contract, not a user-facing business error.
	// Keep the wire body small and safe instead of returning the i18n envelope.
	c.JSON(statusCode, result)
}

func (cn *Common) logReadinessErrors(errs map[string]error) {
	if len(errs) == 0 || cn == nil || cn.Log == nil {
		return
	}
	for dependency, err := range errs {
		if err == nil {
			continue
		}
		cn.Error("readiness dependency check failed", zap.String("dependency", dependency), zap.Error(err))
	}
}

func (c *dependencyReadinessChecker) Check(ctx context.Context) readinessResult {
	result := readinessResult{
		Status: healthStatusUp,
		Dependencies: map[string]string{
			"db":    healthStatusUp,
			"redis": healthStatusUp,
		},
		Errors: map[string]error{},
	}

	var mu sync.Mutex
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		if err := c.pingDB(ctx); err != nil {
			mu.Lock()
			result.Dependencies["db"] = healthStatusDown
			result.Errors["db"] = err
			mu.Unlock()
		}
	}()
	go func() {
		defer wg.Done()
		if err := c.pingRedis(ctx); err != nil {
			mu.Lock()
			result.Dependencies["redis"] = healthStatusDown
			result.Errors["redis"] = err
			mu.Unlock()
		}
	}()
	wg.Wait()

	if len(result.Errors) > 0 {
		result.Status = healthStatusDown
	}
	return result
}

func (c *dependencyReadinessChecker) pingDB(ctx context.Context) error {
	if c == nil || c.db == nil || c.db.session == nil || c.db.session.DB == nil {
		return errors.New("db session unavailable")
	}
	return c.db.session.DB.PingContext(ctx)
}

func (c *dependencyReadinessChecker) pingRedis(ctx context.Context) error {
	if c == nil || c.redisClient == nil {
		return errors.New("redis client unavailable")
	}
	_, err := c.redisClient.WithContext(ctx).Ping().Result()
	return err
}
