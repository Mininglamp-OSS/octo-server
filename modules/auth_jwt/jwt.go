// Package auth_jwt issues and serves the RSA keys for the JWT trust chain
// used between octo-server (issuer), octo-fleet, octo-matter and daemon-cli.
//
// Design:
//   - octo-server is the only JWT issuer. It generates an RS256 keypair on
//     first start, persists the private key under ~/.octo-server/jwt-priv.pem
//     (overridable via env), and exposes the public key as JWKS at
//     /.well-known/jwks.json.
//   - All other services (fleet / matter) fetch JWKS once at startup, cache
//     it, and verify tokens locally. No HTTP-back-to-server on each request.
//   - The token-exchange endpoint POST /v1/auth/token accepts either a
//     web session token (existing AuthMiddleware lookup) or a daemon
//     api-key. Both paths resolve to (uid, space_id), then mint a JWT.
package auth_jwt

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/go-jose/go-jose/v3"
	"github.com/go-jose/go-jose/v3/jwt"
	"go.uber.org/zap"
)

const (
	defaultPrivKeyPath = ".octo-server/jwt-priv.pem"
	// kid stays stable across restarts because we derive it from the
	// modulus — clients can hold long-lived caches keyed by kid.
	jwtKidPrefix = "octo-server-"
	jwtIssuer    = "octo-server"

	// Token lifetimes — see spec §"技术决策"
	webTokenTTL    = 30 * time.Minute
	daemonTokenTTL = 30 * 24 * time.Hour
)

// AuthJWT is the module entrypoint registered with octo-lib.
type AuthJWT struct {
	ctx *config.Context
	log.Log

	mu      sync.RWMutex
	signer  jose.Signer
	pubKey  *rsa.PublicKey
	kid     string
	jwksDoc []byte // pre-rendered JWKS bytes
}

// New loads or generates the RSA keypair and returns a configured module.
// Panics on failure — the service cannot function without working keys.
func New(ctx *config.Context) *AuthJWT {
	a := &AuthJWT{
		ctx: ctx,
		Log: log.NewTLog("AuthJWT"),
	}
	if err := a.loadOrGenerateKey(); err != nil {
		panic(fmt.Errorf("auth_jwt: %w", err))
	}
	return a
}

func privKeyPath() string {
	// Override via env JWT_PRIVATE_KEY_PATH (full path), otherwise
	// home-dir relative.
	if p := os.Getenv("JWT_PRIVATE_KEY_PATH"); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		// Fallback to cwd-relative so tests don't crash.
		return defaultPrivKeyPath
	}
	return filepath.Join(home, defaultPrivKeyPath)
}

// loadOrGenerateKey loads a stored private key or generates a new one and
// persists it. Sets a.signer, a.pubKey, a.kid, a.jwksDoc.
func (a *AuthJWT) loadOrGenerateKey() error {
	path := privKeyPath()
	var key *rsa.PrivateKey

	if data, err := os.ReadFile(path); err == nil {
		blk, _ := pem.Decode(data)
		if blk == nil {
			return errors.New("invalid PEM in " + path)
		}
		parsed, perr := x509.ParsePKCS8PrivateKey(blk.Bytes)
		if perr != nil {
			// Try PKCS1 as fallback
			parsed1, p1err := x509.ParsePKCS1PrivateKey(blk.Bytes)
			if p1err != nil {
				return fmt.Errorf("parse key: %w (also tried PKCS1: %v)", perr, p1err)
			}
			key = parsed1
		} else {
			rsaKey, ok := parsed.(*rsa.PrivateKey)
			if !ok {
				return errors.New("private key is not RSA")
			}
			key = rsaKey
		}
		a.Info("loaded JWT private key from disk", zap.String("path", path))
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("read key: %w", err)
	} else {
		// Generate fresh
		gen, gerr := rsa.GenerateKey(rand.Reader, 2048)
		if gerr != nil {
			return fmt.Errorf("generate key: %w", gerr)
		}
		key = gen
		der, _ := x509.MarshalPKCS8PrivateKey(key)
		blk := &pem.Block{Type: "PRIVATE KEY", Bytes: der}
		if mkdirErr := os.MkdirAll(filepath.Dir(path), 0o700); mkdirErr != nil {
			return fmt.Errorf("mkdir for key: %w", mkdirErr)
		}
		if writeErr := os.WriteFile(path, pem.EncodeToMemory(blk), 0o600); writeErr != nil {
			return fmt.Errorf("write key: %w", writeErr)
		}
		a.Info("generated new JWT private key", zap.String("path", path))
	}

	a.pubKey = &key.PublicKey
	// kid = prefix + last-8-of-modulus (stable, no time component)
	mod := key.N.Bytes()
	if len(mod) >= 8 {
		a.kid = fmt.Sprintf("%s%x", jwtKidPrefix, mod[len(mod)-8:])
	} else {
		a.kid = jwtKidPrefix + "short"
	}

	signKey := jose.SigningKey{
		Algorithm: jose.RS256,
		Key: jose.JSONWebKey{
			Key:   key,
			KeyID: a.kid,
			Use:   "sig",
		},
	}
	signer, err := jose.NewSigner(signKey, (&jose.SignerOptions{}).WithType("JWT"))
	if err != nil {
		return fmt.Errorf("build signer: %w", err)
	}
	a.signer = signer

	// Pre-render JWKS document
	jwkPub := jose.JSONWebKey{
		Key:       a.pubKey,
		KeyID:     a.kid,
		Algorithm: "RS256",
		Use:       "sig",
	}
	set := jose.JSONWebKeySet{Keys: []jose.JSONWebKey{jwkPub}}
	doc, err := json.Marshal(set)
	if err != nil {
		return fmt.Errorf("marshal JWKS: %w", err)
	}
	a.jwksDoc = doc
	return nil
}

// Claims is what we put inside the JWT. All fleet/matter middleware should
// decode into this exact shape.
type Claims struct {
	jwt.Claims
	Scope    string `json:"scope"`              // "web" | "daemon"
	SpaceID  string `json:"space_id,omitempty"` // current space at token time
	DaemonID string `json:"daemon_id,omitempty"`
}

// IssueWebToken mints a JWT for a logged-in browser session.
func (a *AuthJWT) IssueWebToken(uid, spaceID string) (string, error) {
	now := time.Now()
	cl := Claims{
		Claims: jwt.Claims{
			Issuer:   jwtIssuer,
			Subject:  uid,
			IssuedAt: jwt.NewNumericDate(now),
			Expiry:   jwt.NewNumericDate(now.Add(webTokenTTL)),
		},
		Scope:   "web",
		SpaceID: spaceID,
	}
	return jwt.Signed(a.signer).Claims(cl).CompactSerialize()
}

// IssueDaemonToken mints a long-lived JWT for a daemon. daemon_id is the
// daemon's stable identifier; uid is the daemon owner.
func (a *AuthJWT) IssueDaemonToken(uid, spaceID, daemonID string) (string, error) {
	now := time.Now()
	cl := Claims{
		Claims: jwt.Claims{
			Issuer:   jwtIssuer,
			Subject:  uid,
			IssuedAt: jwt.NewNumericDate(now),
			Expiry:   jwt.NewNumericDate(now.Add(daemonTokenTTL)),
		},
		Scope:    "daemon",
		SpaceID:  spaceID,
		DaemonID: daemonID,
	}
	return jwt.Signed(a.signer).Claims(cl).CompactSerialize()
}

// Route mounts /v1/auth/token and /.well-known/jwks.json.
//
// Token exchange is the public-facing endpoint; it accepts either a web
// session token (existing AuthMiddleware semantics) or a daemon api-key.
// JWKS is unauthenticated and cacheable.
//
// PR-A.2 added two cross-service bot endpoints (see bot_api.go):
//   POST /v1/bot/mint        — web session auth, mints bot OBO
//   GET  /v1/bot/:uid/token  — daemon JWT auth, returns bot_token
func (a *AuthJWT) Route(r *wkhttp.WKHttp) {
	r.GET("/.well-known/jwks.json", a.serveJWKS)
	r.POST("/v1/auth/token", a.exchangeToken)

	// /v1/bot/mint requires the standard session auth — daemon-scope
	// JWTs aren't allowed to mint (only browsers do, on behalf of the
	// logged-in user). a.ctx.AuthMiddleware(r) is octo-lib's session
	// middleware that's already used elsewhere.
	authGroup := r.Group("/v1", a.ctx.AuthMiddleware(r))
	authGroup.POST("/bot/mint", a.mintBot)

	// /v1/bot/:uid/token verifies daemon JWT inline (no group middleware
	// because the existing AuthMiddleware would reject our Bearer JWT).
	r.GET("/v1/bot/:uid/token", a.botToken)
}

func (a *AuthJWT) serveJWKS(c *wkhttp.Context) {
	c.Header("Cache-Control", "public, max-age=300")
	c.Data(http.StatusOK, "application/json", a.jwksDoc)
}

// exchangeRequest accepts either auth path. Exactly one must be set.
type exchangeRequest struct {
	SessionToken string `json:"session_token,omitempty"`
	APIKey       string `json:"api_key,omitempty"`
	DaemonID     string `json:"daemon_id,omitempty"`
	// SpaceID lets the caller specify which space's scope to embed; if
	// blank, the resolver picks the caller's default space.
	SpaceID string `json:"space_id,omitempty"`
}

type exchangeResponse struct {
	Token     string `json:"token"`
	ExpiresIn int    `json:"expires_in"` // seconds until exp
	Scope     string `json:"scope"`
}

func (a *AuthJWT) exchangeToken(c *wkhttp.Context) {
	var req exchangeRequest
	if err := c.BindJSON(&req); err != nil {
		c.ResponseError(fmt.Errorf("invalid body: %w", err))
		return
	}

	// Path 1: session → web JWT.
	// We piggyback on the standard "token" header used by AuthMiddleware
	// — if the caller forgot to put the session in the body, allow the
	// header form too.
	sessionTok := req.SessionToken
	if sessionTok == "" {
		sessionTok = c.GetHeader("token")
	}
	if sessionTok != "" {
		uid, spaceID, err := a.resolveSession(sessionTok, req.SpaceID)
		if err != nil {
			a.Warn("session resolve failed", zap.Error(err))
			c.ResponseErrorWithStatus(errors.New("unauthorized"), http.StatusUnauthorized)
			return
		}
		tok, err := a.IssueWebToken(uid, spaceID)
		if err != nil {
			c.ResponseError(err)
			return
		}
		c.JSON(http.StatusOK, exchangeResponse{
			Token:     tok,
			ExpiresIn: int(webTokenTTL.Seconds()),
			Scope:     "web",
		})
		return
	}

	// Path 2: api-key → daemon JWT.
	if req.APIKey != "" {
		uid, spaceID, daemonID, err := a.resolveAPIKey(req.APIKey, req.DaemonID, req.SpaceID)
		if err != nil {
			a.Warn("api-key resolve failed", zap.Error(err))
			c.ResponseErrorWithStatus(errors.New("unauthorized"), http.StatusUnauthorized)
			return
		}
		tok, err := a.IssueDaemonToken(uid, spaceID, daemonID)
		if err != nil {
			c.ResponseError(err)
			return
		}
		c.JSON(http.StatusOK, exchangeResponse{
			Token:     tok,
			ExpiresIn: int(daemonTokenTTL.Seconds()),
			Scope:     "daemon",
		})
		return
	}

	c.ResponseError(errors.New("either session_token or api_key required"))
}
