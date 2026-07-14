package resource_share

import (
	"crypto/ecdsa"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-server/internal/resourceshare"
	octoredis "github.com/Mininglamp-OSS/octo-server/pkg/redis"
	jose "github.com/go-jose/go-jose/v3"
	rd "github.com/go-redis/redis"
)

const (
	maxProofKeyFileBytes = 64 << 10
	limiterKeyPrefix     = "resource-share:v1:"
	limiterPoolSize      = 10

	defaultMaxConcurrentDispatches  = 16
	maxConcurrentDispatches         = 256
	defaultGlobalRPS                = 20.0
	defaultGlobalBurst              = 100
	defaultDMRPS                    = 1.0 / 30.0
	defaultDMBurst                  = 1
	defaultChannelRPS               = 5.0
	defaultChannelBurst             = 20
	defaultLimitFailureRetrySeconds = 5
	maxConfiguredRPS                = 1000.0
	maxConfiguredBurst              = 10000
)

type runtimeConfig struct {
	FeatureEnabled          bool
	MaxConcurrentDispatches int
	GlobalBudget            resourceshare.RateBudget
	DMBudget                resourceshare.RateBudget
	ChannelBudget           resourceshare.RateBudget
	LimitFailureRetry       time.Duration
	ProofSigningJWKFile     string
	ProofVerificationFile   string
}

func loadRuntimeConfigFromEnv() (runtimeConfig, error) {
	enabled, err := featureEnabledFromEnv()
	if err != nil {
		return runtimeConfig{}, err
	}
	concurrency, err := boundedIntFromEnv(
		"DM_RESOURCE_SHARE_MAX_CONCURRENT_DISPATCHES", defaultMaxConcurrentDispatches, 1, maxConcurrentDispatches,
	)
	if err != nil {
		return runtimeConfig{}, err
	}
	global, err := rateBudgetFromEnv(
		"DM_RESOURCE_SHARE_GLOBAL_RPS", "DM_RESOURCE_SHARE_GLOBAL_BURST", defaultGlobalRPS, defaultGlobalBurst,
	)
	if err != nil {
		return runtimeConfig{}, err
	}
	dm, err := rateBudgetFromEnv(
		"DM_RESOURCE_SHARE_DM_RPS", "DM_RESOURCE_SHARE_DM_BURST", defaultDMRPS, defaultDMBurst,
	)
	if err != nil {
		return runtimeConfig{}, err
	}
	channel, err := rateBudgetFromEnv(
		"DM_RESOURCE_SHARE_CHANNEL_RPS", "DM_RESOURCE_SHARE_CHANNEL_BURST", defaultChannelRPS, defaultChannelBurst,
	)
	if err != nil {
		return runtimeConfig{}, err
	}
	retrySeconds, err := boundedIntFromEnv(
		"DM_RESOURCE_SHARE_LIMIT_FAILURE_RETRY_SECONDS", defaultLimitFailureRetrySeconds, 1, 60,
	)
	if err != nil {
		return runtimeConfig{}, err
	}
	return runtimeConfig{
		FeatureEnabled: enabled, MaxConcurrentDispatches: concurrency,
		GlobalBudget: global, DMBudget: dm, ChannelBudget: channel,
		LimitFailureRetry:     time.Duration(retrySeconds) * time.Second,
		ProofSigningJWKFile:   strings.TrimSpace(os.Getenv("DM_RESOURCE_SHARE_PROOF_SIGNING_JWK_FILE")),
		ProofVerificationFile: strings.TrimSpace(os.Getenv("DM_RESOURCE_SHARE_PROOF_VERIFICATION_JWKS_FILE")),
	}, nil
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

func boundedIntFromEnv(name string, fallback, minimum, maximum int) (int, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < minimum || value > maximum {
		return 0, fmt.Errorf("resource share: %s must be between %d and %d", name, minimum, maximum)
	}
	return value, nil
}

func rateBudgetFromEnv(rateName, burstName string, defaultRate float64, defaultBurst int) (resourceshare.RateBudget, error) {
	rate := defaultRate
	if raw := strings.TrimSpace(os.Getenv(rateName)); raw != "" {
		parsed, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return resourceshare.RateBudget{}, fmt.Errorf("resource share: invalid %s", rateName)
		}
		rate = parsed
	}
	burst, err := boundedIntFromEnv(burstName, defaultBurst, 1, maxConfiguredBurst)
	if err != nil {
		return resourceshare.RateBudget{}, err
	}
	if rate <= 0 || rate > maxConfiguredRPS || math.IsNaN(rate) || math.IsInf(rate, 0) {
		return resourceshare.RateBudget{}, fmt.Errorf("resource share: %s must be finite and within (0, %.0f]", rateName, maxConfiguredRPS)
	}
	return resourceshare.RateBudget{RatePerSecond: rate, Burst: burst}, nil
}

// reviewedProviderSpecs is deliberately compile-time and empty in the
// platform-foundation rollout. Each provider onboarding adds a reviewed spec;
// requests can never register a provider dynamically.
func reviewedProviderSpecs() []resourceshare.ProviderSpec {
	return nil
}

func newProductionAPI(
	ctx *config.Context,
	runtimeConfig runtimeConfig,
	providerSpecs []resourceshare.ProviderSpec,
) (*API, error) {
	registry, err := resourceshare.NewRegistry(providerSpecs)
	if err != nil {
		return nil, err
	}
	signer, verifier, err := loadProofMaterial(
		runtimeConfig.ProofSigningJWKFile,
		runtimeConfig.ProofVerificationFile,
	)
	if err != nil {
		return nil, err
	}
	if !runtimeConfig.FeatureEnabled {
		service, serviceErr := resourceshare.NewShareService(resourceshare.ShareServiceDependencies{
			FeatureEnabled: func() bool { return false },
		})
		if serviceErr != nil {
			return nil, serviceErr
		}
		return newAPI(ctx, service, verifier), nil
	}
	if ctx == nil || registry.EnabledProviderCount() == 0 || signer == nil {
		return nil, fmt.Errorf("%w: enabled deployment requires context, reviewed provider, and active proof key", resourceshare.ErrShareConfig)
	}

	redisClient := octoredis.NewInstrumentedClient(ctx.GetConfig(), func(options *rd.Options) {
		options.MaxRetries = 1
		options.PoolSize = limiterPoolSize
	})
	limiter, err := resourceshare.NewRedisTargetLimiter(redisClient, registry, resourceshare.TargetLimiterConfig{
		KeyPrefix: limiterKeyPrefix, GlobalBudget: runtimeConfig.GlobalBudget,
		DMBudget: runtimeConfig.DMBudget, ChannelBudget: runtimeConfig.ChannelBudget,
		FailureRetryAfter: runtimeConfig.LimitFailureRetry,
	})
	if err != nil {
		_ = redisClient.Close()
		return nil, err
	}
	service, err := resourceshare.NewShareService(resourceshare.ShareServiceDependencies{
		Registry: registry, Store: resourceshare.NewDurableStore(ctx.DB()),
		Authorizer: resourceshare.NewHumanTargetAuthorizer(ctx.DB()), Limiter: limiter,
		ProofSigner: signer, Transport: productionTransport{ctx: ctx},
		FeatureEnabled:          func() bool { return true },
		MaxConcurrentDispatches: runtimeConfig.MaxConcurrentDispatches,
	})
	if err != nil {
		_ = redisClient.Close()
		return nil, err
	}
	return newAPI(ctx, service, verifier), nil
}

type productionTransport struct {
	ctx *config.Context
}

func (t productionTransport) SendMessageWithResult(request *config.MsgSendReq) (*config.MsgSendResp, error) {
	ctx := t.ctx
	return ctx.SendMessageWithResult(request)
}

func loadProofMaterial(signingFile, verificationFile string) (*resourceshare.ProofSigner, *resourceshare.ProofVerifier, error) {
	verificationKeys := make(map[string]*ecdsa.PublicKey)
	if verificationFile != "" {
		encoded, err := readBoundedKeyFile(verificationFile)
		if err != nil {
			return nil, nil, err
		}
		var set jose.JSONWebKeySet
		if err := json.Unmarshal(encoded, &set); err != nil || len(set.Keys) == 0 {
			return nil, nil, fmt.Errorf("%w: invalid proof verification JWKS", resourceshare.ErrProofConfig)
		}
		for _, key := range set.Keys {
			publicKey, ok := key.Key.(*ecdsa.PublicKey)
			if !ok || key.Algorithm != string(jose.ES256) || key.Use != "sig" {
				return nil, nil, fmt.Errorf("%w: invalid retained proof key", resourceshare.ErrProofConfig)
			}
			if err := addVerificationKey(verificationKeys, key.KeyID, publicKey); err != nil {
				return nil, nil, err
			}
		}
	}

	var signer *resourceshare.ProofSigner
	if signingFile != "" {
		encoded, err := readBoundedKeyFile(signingFile)
		if err != nil {
			return nil, nil, err
		}
		var key jose.JSONWebKey
		if err := json.Unmarshal(encoded, &key); err != nil || key.Algorithm != string(jose.ES256) || key.Use != "sig" {
			return nil, nil, fmt.Errorf("%w: invalid active proof JWK", resourceshare.ErrProofConfig)
		}
		privateKey, ok := key.Key.(*ecdsa.PrivateKey)
		if !ok {
			return nil, nil, fmt.Errorf("%w: active proof key must be private", resourceshare.ErrProofConfig)
		}
		signer, err = resourceshare.NewProofSigner(resourceshare.ProofSigningKey{
			KeyID: key.KeyID, PrivateKey: privateKey,
		})
		if err != nil {
			return nil, nil, err
		}
		if err := addVerificationKey(verificationKeys, key.KeyID, &privateKey.PublicKey); err != nil {
			return nil, nil, err
		}
	}
	if len(verificationKeys) == 0 {
		return signer, nil, nil
	}
	inputs := make([]resourceshare.ProofVerificationKey, 0, len(verificationKeys))
	for keyID, publicKey := range verificationKeys {
		inputs = append(inputs, resourceshare.ProofVerificationKey{KeyID: keyID, PublicKey: publicKey})
	}
	verifier, err := resourceshare.NewProofVerifier(inputs)
	if err != nil {
		return nil, nil, err
	}
	return signer, verifier, nil
}

// LoadProofVerifierFromEnv returns the same active+retained verification ring
// used by the JWKS endpoint, without exposing private key material to callers.
// Display surfaces use this to verify historic human resource-share cards even
// while new sharing is disabled.
func LoadProofVerifierFromEnv() (*resourceshare.ProofVerifier, error) {
	_, verifier, err := loadProofMaterial(
		strings.TrimSpace(os.Getenv("DM_RESOURCE_SHARE_PROOF_SIGNING_JWK_FILE")),
		strings.TrimSpace(os.Getenv("DM_RESOURCE_SHARE_PROOF_VERIFICATION_JWKS_FILE")),
	)
	return verifier, err
}

func addVerificationKey(keys map[string]*ecdsa.PublicKey, keyID string, publicKey *ecdsa.PublicKey) error {
	if publicKey == nil || publicKey.X == nil || publicKey.Y == nil {
		return fmt.Errorf("%w: invalid proof public key", resourceshare.ErrProofConfig)
	}
	if existing, duplicate := keys[keyID]; duplicate {
		if existing == nil || existing.X == nil || existing.Y == nil ||
			existing.X.Cmp(publicKey.X) != 0 || existing.Y.Cmp(publicKey.Y) != 0 {
			return fmt.Errorf("%w: conflicting proof key id", resourceshare.ErrProofConfig)
		}
		return nil
	}
	keys[keyID] = publicKey
	return nil
}

func readBoundedKeyFile(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("%w: open managed proof key file: %v", resourceshare.ErrProofConfig, err)
	}
	defer file.Close()
	encoded, err := io.ReadAll(io.LimitReader(file, maxProofKeyFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("%w: read managed proof key file: %v", resourceshare.ErrProofConfig, err)
	}
	if len(encoded) == 0 || len(encoded) > maxProofKeyFileBytes {
		return nil, fmt.Errorf("%w: managed proof key file size invalid", resourceshare.ErrProofConfig)
	}
	return encoded, nil
}
