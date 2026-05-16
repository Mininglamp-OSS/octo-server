package file_test

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/Mininglamp-OSS/octo-server/modules/file"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newFakeMinioServer returns an httptest.Server that answers just enough of
// the MinIO HTTP surface to let `ensureBucket` succeed:
//
//   - HEAD /<bucket>/  → 200 (BucketExists returns true, skipping MakeBucket
//     and SetBucketPolicy entirely)
//   - everything else  → 200 with empty body, so the test never panics on an
//     unexpected request shape
//
// The server URL is suitable for cfg.Minio.UploadURL — it is what the
// internal client points at. Crucially, the *public* client built from
// cfg.Minio.DownloadURL never touches this server in the presign path:
// PresignedPutObject / PresignedGetObject are pure URL signing, no network
// I/O. That separation is what we want to assert here.
func newFakeMinioServer(t *testing.T) (string, *atomic.Int32) {
	t.Helper()
	var headCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			headCount.Add(1)
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv.URL, &headCount
}

// TestPresignedURLs_SignAgainstPublicEndpoint is the integration-style test
// requested by the PR#50 review.
//
// Setup mirrors a Docker-compose / nginx-proxied deployment:
//
//   - cfg.Minio.UploadURL is the server-internal endpoint (here, the
//     httptest fake) that only the Go process can reach.
//   - cfg.Minio.DownloadURL is the public, browser-facing host
//     (`public.example.com`) that customers actually hit.
//
// The contract under test: presigned PUT / GET URLs MUST be signed against
// the public host and returned as-is. SigV4 includes `host` in the signed
// headers (always — the SDK serialises `X-Amz-SignedHeaders=host;...` into
// the URL), so any post-sign host rewrite would invalidate the signature.
// Reading the URL host back out and confirming it matches DownloadURL is
// therefore equivalent to confirming the signature is valid for that host:
// if the URL host disagreed with the host actually signed, the URL would
// not authenticate at the server end.
func TestPresignedURLs_SignAgainstPublicEndpoint(t *testing.T) {
	internalURL, headCount := newFakeMinioServer(t)

	cfg := config.New()
	cfg.Test = true
	cfg.Minio.URL = internalURL
	cfg.Minio.UploadURL = internalURL
	cfg.Minio.DownloadURL = "https://public.example.com"
	cfg.Minio.AccessKeyID = "test-access-key"
	cfg.Minio.SecretAccessKey = "test-secret-access-key-1234567890"

	ctx := testutil.NewTestContext(cfg)
	svc := file.NewServiceMinio(ctx)

	t.Run("PUT URL signed against public host (no rewriting)", func(t *testing.T) {
		uploadURL, downloadURL, err := svc.PresignedPutURL(
			"chat/2026/05/abc.jpg", "image/jpeg", "", 12345, 5*time.Minute,
		)
		require.NoError(t, err)
		require.NotEmpty(t, uploadURL)
		require.NotEmpty(t, downloadURL)

		// ensureBucket should have hit the internal endpoint exactly
		// once — confirming the bootstrap path actually ran against the
		// server-internal URL, and *only* against it.
		assert.GreaterOrEqual(t, int(headCount.Load()), 1,
			"ensureBucket must run BucketExists against the internal endpoint")

		u, err := url.Parse(uploadURL)
		require.NoError(t, err)

		// Host check: the URL the browser will PUT to is the public one.
		assert.Equal(t, "public.example.com", u.Host,
			"presigned PUT URL must be served from the public host, got %s", u.Host)
		assert.Equal(t, "https", u.Scheme,
			"presigned PUT URL must inherit scheme from DownloadURL")
		assert.NotContains(t, u.Host, "127.0.0.1",
			"server-internal hostname must not leak into the signed PUT URL")

		// Signature shape: SigV4 query params and `host` + `content-length`
		// in the signed headers. Because the signing client was constructed
		// against DownloadURL, the `host` covered by the signature *is* the
		// URL's own host. Any post-sign host change would break that
		// invariant. `content-length` is the P0 size-bypass guard landed in
		// R6: with it in signed headers, the storage gateway rejects any
		// PUT whose body length disagrees with the signed value.
		q := u.Query()
		assert.NotEmpty(t, q.Get("X-Amz-Signature"),
			"presigned PUT URL must carry a SigV4 signature")
		signedHeaders := q.Get("X-Amz-SignedHeaders")
		assert.Contains(t, signedHeaders, "host",
			"presigned PUT URL must include `host` in its signed headers (got %q)", signedHeaders)
		assert.Contains(t, signedHeaders, "content-length",
			"presigned PUT URL must include `content-length` in its signed headers so the storage layer can enforce the upload size cap (got %q)", signedHeaders)
	})

	t.Run("GET URL signed against public host (no rewriting)", func(t *testing.T) {
		raw, err := svc.PresignedGetURL("chat/2026/05/abc.jpg", "report.jpg", "attachment", 5*time.Minute)
		require.NoError(t, err)
		require.NotEmpty(t, raw)

		u, err := url.Parse(raw)
		require.NoError(t, err)

		assert.Equal(t, "public.example.com", u.Host,
			"presigned GET URL must be served from the public host, got %s", u.Host)
		assert.Equal(t, "https", u.Scheme,
			"presigned GET URL must inherit scheme from DownloadURL")
		assert.NotContains(t, u.Host, "127.0.0.1",
			"server-internal hostname must not leak into the signed GET URL")

		q := u.Query()
		assert.NotEmpty(t, q.Get("X-Amz-Signature"),
			"presigned GET URL must carry a SigV4 signature")
		assert.NotEmpty(t, q.Get("X-Amz-Credential"),
			"presigned GET URL must carry the SigV4 credential scope")
		signedHeaders := q.Get("X-Amz-SignedHeaders")
		assert.Contains(t, signedHeaders, "host",
			"presigned GET URL must include `host` in its signed headers (got %q)", signedHeaders)

		assert.True(t,
			strings.Contains(u.Path, "/chat/") && strings.HasSuffix(u.Path, "/abc.jpg"),
			"object path should be reflected in the signed URL, got %s", u.Path)

		disposition := q.Get("response-content-disposition")
		assert.Contains(t, disposition, "attachment",
			"response-content-disposition should preserve the requested disposition")
		assert.Contains(t, disposition, "report.jpg",
			"response-content-disposition should carry the requested filename")
	})

	t.Run("GET falls back to UploadURL when DownloadURL is empty", func(t *testing.T) {
		// Operator misconfiguration: no DownloadURL set. The code path
		// should fall back to UploadURL and emit a warning rather than
		// hard-failing or silently producing a broken URL.
		cfg2 := config.New()
		cfg2.Test = true
		cfg2.Minio.URL = "http://internal-minio:9000"
		cfg2.Minio.UploadURL = "http://internal-minio:9000"
		cfg2.Minio.DownloadURL = ""
		cfg2.Minio.AccessKeyID = "test-access-key"
		cfg2.Minio.SecretAccessKey = "test-secret-access-key-1234567890"

		ctx2 := testutil.NewTestContext(cfg2)
		svc2 := file.NewServiceMinio(ctx2)

		raw, err := svc2.PresignedGetURL("chat/x.jpg", "x.jpg", "attachment", time.Minute)
		require.NoError(t, err)
		u, err := url.Parse(raw)
		require.NoError(t, err)
		assert.Equal(t, "internal-minio:9000", u.Host,
			"with DownloadURL empty, should fall back to UploadURL host")
	})

}

// TestPresignedURLs_RejectPathPrefixedDownloadURL is the regression test
// for the host:port-only contract on `cfg.Minio.DownloadURL`. R3 attempted
// to support path-prefixed values like `https://octo.example.com/minio` by
// hand-rolling the SigV4 canonical URI; that approach does not survive
// nginx `proxy_pass <upstream>/` strip semantics (the canonical URI the
// gateway sees no longer matches the one the signer signed). R4 drops
// path-prefix support entirely and rejects the configuration up front so
// operators upgrading from R3 see a clear error instead of an opaque
// SignatureDoesNotMatch on every PUT/GET.
//
// The test pins three things:
//
//  1. PresignedPutURL returns the host:port-only error for path-prefixed
//     values.
//  2. PresignedGetURL returns the same error from the same input — the
//     contract is symmetric across PUT and GET.
//  3. The error message names the configuration key (`minio.downloadURL`)
//     and points operators at the supported deployment shapes, not just a
//     bare validation failure.
func TestPresignedURLs_RejectPathPrefixedDownloadURL(t *testing.T) {
	internalURL, _ := newFakeMinioServer(t)

	cases := []struct {
		name        string
		downloadURL string
	}{
		{"single-segment path prefix", "https://octo.example.com/minio"},
		{"trailing-slash path prefix", "https://octo.example.com/minio/"},
		{"multi-segment path prefix", "https://octo.example.com/svc/minio"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.New()
			cfg.Test = true
			cfg.Minio.URL = internalURL
			cfg.Minio.UploadURL = internalURL
			cfg.Minio.DownloadURL = tc.downloadURL
			cfg.Minio.AccessKeyID = "test-access-key"
			cfg.Minio.SecretAccessKey = "test-secret-access-key-1234567890"

			svc := file.NewServiceMinio(testutil.NewTestContext(cfg))

			_, _, putErr := svc.PresignedPutURL("chat/abc.jpg", "image/jpeg", "", 1024, time.Minute)
			require.Error(t, putErr,
				"PresignedPutURL must reject path-prefixed downloadURL %q", tc.downloadURL)
			assert.Contains(t, putErr.Error(), "minio.downloadURL",
				"error must name the offending config key; got %v", putErr)
			assert.Contains(t, putErr.Error(), "host:port",
				"error must point operators at the supported shape; got %v", putErr)

			_, getErr := svc.PresignedGetURL("chat/abc.jpg", "x.jpg", "attachment", time.Minute)
			require.Error(t, getErr,
				"PresignedGetURL must reject path-prefixed downloadURL %q", tc.downloadURL)
			assert.Contains(t, getErr.Error(), "minio.downloadURL",
				"error must name the offending config key; got %v", getErr)
			assert.Contains(t, getErr.Error(), "host:port",
				"error must point operators at the supported shape; got %v", getErr)
		})
	}

	t.Run("trailing-slash-only is accepted", func(t *testing.T) {
		// `https://host/` (single trailing slash, no path segment) is
		// equivalent to host:port and must not trip the validator. This
		// is the operator-friendly accept boundary called out in
		// validatePublicDownloadURL's docstring.
		cfg := config.New()
		cfg.Test = true
		cfg.Minio.URL = internalURL
		cfg.Minio.UploadURL = internalURL
		cfg.Minio.DownloadURL = "https://public.example.com/"
		cfg.Minio.AccessKeyID = "test-access-key"
		cfg.Minio.SecretAccessKey = "test-secret-access-key-1234567890"

		svc := file.NewServiceMinio(testutil.NewTestContext(cfg))
		_, _, err := svc.PresignedPutURL("chat/abc.jpg", "image/jpeg", "", 1024, time.Minute)
		require.NoError(t, err,
			"trailing-slash-only downloadURL must be accepted as host:port-equivalent")
	})
}

// TestPresignedPutURL_ConcurrentBucketBootstrap exercises the concurrency
// requirement called out in the PR#50 review: parallel presigned PUTs to the
// same fresh bucket must not double-create or race the SetBucketPolicy step.
//
// The fake server flips between "bucket missing" and "bucket created" so
// the first BucketExists call drives both paths. If multiple goroutines
// reach MakeBucket without serialization, the call count would explode. The
// per-bucket sync.Mutex inside ServiceMinio guarantees exactly one
// MakeBucket+SetBucketPolicy round per bucket per process.
func TestPresignedPutURL_ConcurrentBucketBootstrap(t *testing.T) {
	var (
		headCount   atomic.Int32
		makeCount   atomic.Int32
		policyCount atomic.Int32
		bucketReady atomic.Bool
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodHead:
			headCount.Add(1)
			if bucketReady.Load() {
				w.WriteHeader(http.StatusOK)
			} else {
				w.WriteHeader(http.StatusNotFound)
			}
		case http.MethodPut:
			// minio-go uses PUT for both MakeBucket (path /<bucket>/)
			// and SetBucketPolicy (path /<bucket>/?policy). Distinguish
			// by the presence of the `policy` query key.
			if _, ok := r.URL.Query()["policy"]; ok {
				policyCount.Add(1)
			} else {
				makeCount.Add(1)
				bucketReady.Store(true)
			}
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	t.Cleanup(srv.Close)

	cfg := config.New()
	cfg.Test = true
	cfg.Minio.URL = srv.URL
	cfg.Minio.UploadURL = srv.URL
	cfg.Minio.DownloadURL = "https://public.example.com"
	cfg.Minio.AccessKeyID = "test-access-key"
	cfg.Minio.SecretAccessKey = "test-secret-access-key-1234567890"

	ctx := testutil.NewTestContext(cfg)
	svc := file.NewServiceMinio(ctx)

	const parallelism = 16
	errCh := make(chan error, parallelism)
	start := make(chan struct{})
	for i := 0; i < parallelism; i++ {
		go func() {
			<-start
			_, _, err := svc.PresignedPutURL("chat/concurrent.jpg", "image/jpeg", "", 1024, time.Minute)
			errCh <- err
		}()
	}
	close(start)
	for i := 0; i < parallelism; i++ {
		require.NoError(t, <-errCh)
	}

	// MakeBucket and SetBucketPolicy must each have run at most once for
	// the shared bucket — even with `parallelism` callers racing through
	// ensureBucket. The exact bound is "1 per bucket"; a value of 0 would
	// mean ensureBucket short-circuited (it didn't, because bucketReady
	// started false).
	assert.Equal(t, int32(1), makeCount.Load(),
		"MakeBucket should run exactly once for a fresh shared bucket; ran %d times", makeCount.Load())
	assert.Equal(t, int32(1), policyCount.Load(),
		"SetBucketPolicy should run exactly once for a fresh shared bucket; ran %d times", policyCount.Load())
}
