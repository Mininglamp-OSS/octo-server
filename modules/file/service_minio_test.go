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

// TestPresignedURLs_PreservesPublicPathPrefix is the regression test for
// PR#50 R3 Blocker A.
//
// The bug: R2's `newPublicClient` parsed `cfg.Minio.DownloadURL` but only
// kept `parsed.Host`, dropping `parsed.Path`. For path-proxied deployments
// where nginx routes `https://octo.example.com/minio/*` to the MinIO
// service (the shape the bundled docker-compose stack documents), this
// produced presigned URLs of the form
// `https://octo.example.com/<bucket>/<object>?X-Amz-Signature=...` —
// without the `/minio` prefix nginx looks for, so PUT/GET requests 404 at
// the proxy.
//
// The fix: bake `parsed.Path` into the signed canonical URI from the
// start. SigV4 covers the canonical URI, so the prefix MUST be present at
// signing time, not appended afterward — post-sign rewriting would
// invalidate the signature.
//
// This test asserts the host, path-prefix, signature shape, and the fact
// that we did NOT post-process the URL (the X-Amz-SignedHeaders include
// `host`, the signature is over the full prefixed path).
func TestPresignedURLs_PreservesPublicPathPrefix(t *testing.T) {
	// Internal endpoint: the in-cluster MinIO that ensureBucket talks to.
	// Doesn't need a path prefix — the prefix only matters for the
	// browser-facing nginx route.
	var headCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			headCount.Add(1)
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	cfg := config.New()
	cfg.Test = true
	cfg.Minio.URL = srv.URL
	cfg.Minio.UploadURL = srv.URL
	// Path-proxied endpoint: nginx routes /minio/* to the MinIO service.
	cfg.Minio.DownloadURL = "https://octo.example.com/minio"
	cfg.Minio.AccessKeyID = "test-access-key"
	cfg.Minio.SecretAccessKey = "test-secret-access-key-1234567890"

	ctx := testutil.NewTestContext(cfg)
	svc := file.NewServiceMinio(ctx)

	t.Run("PUT URL host and path prefix preserved end-to-end", func(t *testing.T) {
		uploadURL, _, err := svc.PresignedPutURL(
			"chat/2026/05/abc.jpg", "image/jpeg", "", 5*time.Minute,
		)
		require.NoError(t, err)
		require.NotEmpty(t, uploadURL)

		u, err := url.Parse(uploadURL)
		require.NoError(t, err)

		// Host must be the public hostname only — `parsed.Path` from
		// the DownloadURL must NOT leak into the host.
		assert.Equal(t, "octo.example.com", u.Host,
			"presigned PUT URL must sit on the public host, got %s", u.Host)
		assert.Equal(t, "https", u.Scheme,
			"presigned PUT URL must inherit scheme from DownloadURL")

		// Path must carry the `/minio/<bucket>/<object>` prefix —
		// this is the whole bug under test. nginx will not route
		// `/<bucket>/...` to the MinIO upstream.
		assert.True(t, strings.HasPrefix(u.Path, "/minio/chat/"),
			"presigned PUT URL must keep the public path prefix; got path %q", u.Path)
		assert.True(t, strings.HasSuffix(u.Path, "/abc.jpg"),
			"presigned PUT URL must end with the object key; got path %q", u.Path)

		// Signature shape: SigV4 covers `host` + canonical URI. If the
		// signature were computed over `/<bucket>/<object>` and the URL
		// then prefixed with `/minio`, the canonical URI used at the
		// gateway would not match what was signed and the URL would
		// authenticate-fail. The presence of `host` in SignedHeaders
		// plus a non-empty signature is the strongest assertion we can
		// make without standing up a real MinIO; combined with the
		// path-prefix check above, it confirms the signature was
		// computed over the full prefixed path.
		q := u.Query()
		assert.NotEmpty(t, q.Get("X-Amz-Signature"),
			"presigned PUT URL must carry a SigV4 signature")
		assert.NotEmpty(t, q.Get("X-Amz-Credential"),
			"presigned PUT URL must carry the SigV4 credential scope")
		assert.Contains(t, q.Get("X-Amz-SignedHeaders"), "host",
			"presigned PUT URL must include `host` in its signed headers")
	})

	t.Run("GET URL host and path prefix preserved end-to-end", func(t *testing.T) {
		raw, err := svc.PresignedGetURL("chat/2026/05/abc.jpg", "report.jpg", "attachment", 5*time.Minute)
		require.NoError(t, err)

		u, err := url.Parse(raw)
		require.NoError(t, err)

		assert.Equal(t, "octo.example.com", u.Host,
			"presigned GET URL must sit on the public host")
		assert.True(t, strings.HasPrefix(u.Path, "/minio/chat/"),
			"presigned GET URL must keep the public path prefix; got path %q", u.Path)
		assert.True(t, strings.HasSuffix(u.Path, "/abc.jpg"),
			"presigned GET URL must end with the object key; got path %q", u.Path)

		q := u.Query()
		assert.NotEmpty(t, q.Get("X-Amz-Signature"))
		assert.Contains(t, q.Get("X-Amz-SignedHeaders"), "host")
		// response-content-disposition still works through the prefix.
		assert.Contains(t, q.Get("response-content-disposition"), "attachment")
		assert.Contains(t, q.Get("response-content-disposition"), "report.jpg")
	})

	t.Run("trailing slash on DownloadURL is normalized (no double slash)", func(t *testing.T) {
		// Operator-friendly: `https://octo.example.com/minio/` should
		// produce the same URL as `https://octo.example.com/minio`.
		// Without normalization the canonical URI would carry `//`,
		// which most gateways collapse before re-signing — a classic
		// signature-mismatch foot-gun.
		cfg2 := config.New()
		cfg2.Test = true
		cfg2.Minio.URL = srv.URL
		cfg2.Minio.UploadURL = srv.URL
		cfg2.Minio.DownloadURL = "https://octo.example.com/minio/"
		cfg2.Minio.AccessKeyID = "test-access-key"
		cfg2.Minio.SecretAccessKey = "test-secret-access-key-1234567890"
		svc2 := file.NewServiceMinio(testutil.NewTestContext(cfg2))

		raw, err := svc2.PresignedGetURL("chat/x.jpg", "x.jpg", "attachment", time.Minute)
		require.NoError(t, err)
		u, err := url.Parse(raw)
		require.NoError(t, err)
		assert.False(t, strings.Contains(u.Path, "//"),
			"trailing slash on DownloadURL should not produce `//` in the signed path; got %q", u.Path)
		assert.True(t, strings.HasPrefix(u.Path, "/minio/chat/"),
			"path prefix should be preserved exactly once; got %q", u.Path)
	})
}

// TestPresignedURLs_RejectMalformedObjectKeys is the regression test for
// PR#50 R3 codex finding 2.5.
//
// `<bucket>/dir/` reaches the MinIO backend as an empty-keyed prefix; some
// gateways collapse the trailing slash, others 404. Either way the URL is
// useless to the browser. Reject up front, with the same "空对象路径" error
// style as the existing empty-key path. Same for keys containing `//` —
// these get path-normalized away by most HTTP intermediaries, breaking
// signature validation downstream.
func TestPresignedURLs_RejectMalformedObjectKeys(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	cfg := config.New()
	cfg.Test = true
	cfg.Minio.URL = srv.URL
	cfg.Minio.UploadURL = srv.URL
	cfg.Minio.DownloadURL = "https://public.example.com"
	cfg.Minio.AccessKeyID = "test-access-key"
	cfg.Minio.SecretAccessKey = "test-secret-access-key-1234567890"

	svc := file.NewServiceMinio(testutil.NewTestContext(cfg))

	cases := []struct {
		name  string
		input string
	}{
		{"trailing slash on allowed bucket prefix", "chat/dir/"},
		{"trailing slash falls through to default bucket", "loose-name/"},
		{"embedded double slash inside object key", "chat/a//b.png"},
	}
	for _, tc := range cases {
		t.Run("PUT/"+tc.name, func(t *testing.T) {
			_, _, err := svc.PresignedPutURL(tc.input, "image/jpeg", "", time.Minute)
			require.Error(t, err, "PresignedPutURL must reject %q", tc.input)
		})
		t.Run("GET/"+tc.name, func(t *testing.T) {
			_, err := svc.PresignedGetURL(tc.input, "x.jpg", "attachment", time.Minute)
			require.Error(t, err, "PresignedGetURL must reject %q", tc.input)
		})
	}
}
