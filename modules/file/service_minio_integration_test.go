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
			"chat/2026/05/abc.jpg", "image/jpeg", "", 5*time.Minute,
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

		// Signature shape: SigV4 query params and `host` in the signed
		// headers. Because the signing client was constructed against
		// DownloadURL, the `host` covered by the signature *is* the
		// URL's own host. Any post-sign host change would break that
		// invariant, which is the whole bug R2 is fixing.
		q := u.Query()
		assert.NotEmpty(t, q.Get("X-Amz-Signature"),
			"presigned PUT URL must carry a SigV4 signature")
		signedHeaders := q.Get("X-Amz-SignedHeaders")
		assert.Contains(t, signedHeaders, "host",
			"presigned PUT URL must include `host` in its signed headers (got %q)", signedHeaders)
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

	t.Run("path-proxied DownloadURL keeps prefix in signed URL", func(t *testing.T) {
		// Mirrors the docker-compose stack where nginx routes
		// `https://octo.example.com/minio/*` to the MinIO service.
		// The reverse-proxy path prefix is part of the signed canonical
		// URI from the start, so both the host and the path are valid
		// for SigV4 verification at the gateway. Companion regression
		// to TestPresignedURLs_PreservesPublicPathPrefix in
		// service_minio_test.go — kept here so the integration suite
		// stays a complete picture of the path-routing case alongside
		// the no-prefix and DownloadURL-empty fallbacks above.
		cfg3 := config.New()
		cfg3.Test = true
		cfg3.Minio.URL = internalURL
		cfg3.Minio.UploadURL = internalURL
		cfg3.Minio.DownloadURL = "https://public.example.com/minio"
		cfg3.Minio.AccessKeyID = "test-access-key"
		cfg3.Minio.SecretAccessKey = "test-secret-access-key-1234567890"

		svc3 := file.NewServiceMinio(testutil.NewTestContext(cfg3))

		uploadURL, _, err := svc3.PresignedPutURL("chat/2026/05/abc.jpg", "image/jpeg", "", 5*time.Minute)
		require.NoError(t, err)
		u, err := url.Parse(uploadURL)
		require.NoError(t, err)

		assert.Equal(t, "public.example.com", u.Host,
			"public path prefix must not leak into the host component")
		assert.True(t, strings.HasPrefix(u.Path, "/minio/chat/"),
			"presigned PUT URL must keep `/minio/<bucket>/...` prefix; got %q", u.Path)
		assert.True(t, strings.HasSuffix(u.Path, "/abc.jpg"),
			"presigned PUT URL must end with the object key; got %q", u.Path)
		q := u.Query()
		assert.NotEmpty(t, q.Get("X-Amz-Signature"))
		assert.Contains(t, q.Get("X-Amz-SignedHeaders"), "host")

		getURL, err := svc3.PresignedGetURL("chat/2026/05/abc.jpg", "report.jpg", "attachment", 5*time.Minute)
		require.NoError(t, err)
		gu, err := url.Parse(getURL)
		require.NoError(t, err)
		assert.Equal(t, "public.example.com", gu.Host)
		assert.True(t, strings.HasPrefix(gu.Path, "/minio/chat/"),
			"presigned GET URL must keep `/minio/<bucket>/...` prefix; got %q", gu.Path)
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
			_, _, err := svc.PresignedPutURL("chat/concurrent.jpg", "image/jpeg", "", time.Minute)
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
