package redis

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	rd "github.com/go-redis/redis"
)

// newConfig 构造一个带默认 RedisAddr 的 config，方便测试场景里只覆盖 TLS 相关字段。
func newConfig() *config.Config {
	cfg := config.New()
	cfg.DB.RedisAddr = "127.0.0.1:6379"
	cfg.DB.RedisPass = "secret"
	return cfg
}

func TestBuildOptions_PlaintextDefaults(t *testing.T) {
	cfg := newConfig()
	opts, err := BuildOptions(cfg)
	if err != nil {
		t.Fatalf("BuildOptions: %v", err)
	}
	if opts.Addr != "127.0.0.1:6379" {
		t.Errorf("Addr = %q, want 127.0.0.1:6379", opts.Addr)
	}
	if opts.Password != "secret" {
		t.Errorf("Password = %q, want secret", opts.Password)
	}
	if opts.TLSConfig != nil {
		t.Errorf("TLSConfig should be nil when RedisTLS=false, got %+v", opts.TLSConfig)
	}
}

func TestBuildOptions_TLSInsecureSkipVerify(t *testing.T) {
	cfg := newConfig()
	cfg.DB.RedisTLS = true
	cfg.DB.RedisTLSInsecureSkipVerify = true

	opts, err := BuildOptions(cfg)
	if err != nil {
		t.Fatalf("BuildOptions: %v", err)
	}
	if opts.TLSConfig == nil {
		t.Fatal("TLSConfig should be set when RedisTLS=true")
	}
	if !opts.TLSConfig.InsecureSkipVerify {
		t.Error("InsecureSkipVerify should be true")
	}
	if opts.TLSConfig.RootCAs != nil {
		t.Error("RootCAs should be nil when no CA file supplied")
	}
}

func TestBuildOptions_TLSWithValidCAFile(t *testing.T) {
	caPath := writeSelfSignedCA(t)
	cfg := newConfig()
	cfg.DB.RedisTLS = true
	cfg.DB.RedisTLSCAFile = caPath

	opts, err := BuildOptions(cfg)
	if err != nil {
		t.Fatalf("BuildOptions: %v", err)
	}
	if opts.TLSConfig == nil || opts.TLSConfig.RootCAs == nil {
		t.Fatal("RootCAs should be populated from CA file")
	}
}

func TestBuildOptions_TLSWithMissingCAFile(t *testing.T) {
	cfg := newConfig()
	cfg.DB.RedisTLS = true
	cfg.DB.RedisTLSCAFile = "/nonexistent/path/ca.pem"

	if _, err := BuildOptions(cfg); err == nil {
		t.Fatal("expected error for missing CA file, got nil")
	} else if !strings.Contains(err.Error(), "tls") {
		t.Errorf("error should mention tls context: %v", err)
	}
}

func TestBuildOptions_OverridesApplied(t *testing.T) {
	cfg := newConfig()
	opts, err := BuildOptions(cfg, func(o *rd.Options) {
		o.MaxRetries = 7
		o.PoolSize = 42
	})
	if err != nil {
		t.Fatalf("BuildOptions: %v", err)
	}
	if opts.MaxRetries != 7 {
		t.Errorf("MaxRetries = %d, want 7", opts.MaxRetries)
	}
	if opts.PoolSize != 42 {
		t.Errorf("PoolSize = %d, want 42", opts.PoolSize)
	}
}

func TestBuildOptions_NilOverrideTolerated(t *testing.T) {
	cfg := newConfig()
	if _, err := BuildOptions(cfg, nil); err != nil {
		t.Fatalf("nil override should be tolerated, got: %v", err)
	}
}

func TestMustBuildOptions_PanicsOnCAFailure(t *testing.T) {
	cfg := newConfig()
	cfg.DB.RedisTLS = true
	cfg.DB.RedisTLSCAFile = "/nonexistent/path/ca.pem"

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("MustBuildOptions should panic on CA load failure")
		}
	}()
	MustBuildOptions(cfg)
}

// writeSelfSignedCA 在临时目录生成一个自签 CA 证书 PEM，用于 RedisTLSCAFile 测试。
// 走运行时生成是为了避免落 fixture 文件 + 避免证书过期。
func writeSelfSignedCA(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("gen key: %v", err)
	}
	tpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "octo-redis-test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	path := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatalf("write ca: %v", err)
	}
	return path
}
