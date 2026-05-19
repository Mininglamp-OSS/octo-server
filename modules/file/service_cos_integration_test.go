package file_test

import (
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/Mininglamp-OSS/octo-server/modules/file"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCOSPresignedURLs_SignAgainstPublicEndpoint mirrors the MinIO-side
// integration test (see service_minio_integration_test.go) for COS.
//
// PR#50 R6 shipped a `presigned.Host = bucketURL.Host` mutation AFTER
// signing — same hazard MinIO closed at R3+. SigV4 covers `host` in
// the signed headers, so any post-sign host change produces 403
// SignatureDoesNotMatch from the COS gateway on every browser PUT/GET.
//
// The R7 fix builds a public-facing minio client whose endpoint is
// derived from `cosConfig.BucketURL` (parent domain after stripping
// the documented `<bucket>.` subdomain), and signs against that
// client directly. Reading the resulting URL host back out and
// confirming it matches BucketURL is equivalent to confirming the
// signature is valid for that host: if the URL host disagreed with
// the host actually signed, the URL would not authenticate at the
// COS gateway.
//
// The test uses fake credentials and never makes a network call —
// PresignHeader / PresignedGetObject are pure URL signing.
func TestCOSPresignedURLs_SignAgainstPublicEndpoint(t *testing.T) {
	cfg := config.New()
	cfg.Test = true
	cfg.COS.SecretID = "test-secret-id"
	cfg.COS.SecretKey = "test-secret-key-1234567890"
	cfg.COS.Bucket = "my-bucket-12345678"
	cfg.COS.Region = "ap-beijing"
	cfg.COS.BucketURL = "https://my-bucket-12345678.cos.example.com"

	ctx := testutil.NewTestContext(cfg)
	svc := file.NewServiceCOS(ctx)

	t.Run("PUT URL signed against public host (no rewriting)", func(t *testing.T) {
		uploadURL, downloadURL, err := svc.PresignedPutURL(
			"chat/2026/05/abc.jpg", "image/jpeg", "", 12345, 5*time.Minute,
		)
		require.NoError(t, err)
		require.NotEmpty(t, uploadURL)
		require.NotEmpty(t, downloadURL)

		u, err := url.Parse(uploadURL)
		require.NoError(t, err)

		// Host check: BucketURL host should match exactly. The minio
		// SDK virtual-hosts `<bucket>.<parent>` — with parent
		// `cos.example.com` and bucket `my-bucket-12345678`, the
		// reconstructed host equals BucketURL host.
		assert.Equal(t, "my-bucket-12345678.cos.example.com", u.Host,
			"presigned PUT URL must be served from the BucketURL host, got %s", u.Host)
		assert.Equal(t, "https", u.Scheme,
			"presigned PUT URL must inherit scheme from BucketURL")

		// SigV4 shape: `host` and `content-length` MUST appear in
		// the signed headers. Because the signing client was
		// constructed against BucketURL's parent domain, the host
		// covered by the signature is the URL's own host. Any
		// post-sign host change would break that invariant.
		// `content-length` is the P0 size-bypass guard landed in R6.
		q := u.Query()
		assert.NotEmpty(t, q.Get("X-Amz-Signature"),
			"presigned PUT URL must carry a SigV4 signature")
		signedHeaders := q.Get("X-Amz-SignedHeaders")
		assert.Contains(t, signedHeaders, "host",
			"presigned PUT URL must include `host` in its signed headers (got %q)", signedHeaders)
		assert.Contains(t, signedHeaders, "content-length",
			"presigned PUT URL must include `content-length` in its signed headers so the COS gateway can enforce the upload size cap (got %q)", signedHeaders)
	})

	t.Run("GET URL signed against public host (no rewriting)", func(t *testing.T) {
		raw, err := svc.PresignedGetURL("chat/2026/05/abc.jpg", "report.jpg", "attachment", 5*time.Minute)
		require.NoError(t, err)
		require.NotEmpty(t, raw)

		u, err := url.Parse(raw)
		require.NoError(t, err)

		assert.Equal(t, "my-bucket-12345678.cos.example.com", u.Host,
			"presigned GET URL must be served from the BucketURL host, got %s", u.Host)
		assert.Equal(t, "https", u.Scheme,
			"presigned GET URL must inherit scheme from BucketURL")

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
}

// TestCOSPresignedURLs_DefaultEndpointWhenBucketURLEmpty pins the
// fallback contract: when `cosConfig.BucketURL` is empty, presigned
// URLs are signed against the SDK's canonical endpoint
// `<bucket>.cos.<region>.myqcloud.com`. This is the COS "no custom
// domain" deployment shape — the canonical hostname is reachable
// from the browser without any operator-side DNS work.
func TestCOSPresignedURLs_DefaultEndpointWhenBucketURLEmpty(t *testing.T) {
	cfg := config.New()
	cfg.Test = true
	cfg.COS.SecretID = "test-secret-id"
	cfg.COS.SecretKey = "test-secret-key-1234567890"
	cfg.COS.Bucket = "my-bucket-12345678"
	cfg.COS.Region = "ap-beijing"
	cfg.COS.BucketURL = "" // fallback path

	svc := file.NewServiceCOS(testutil.NewTestContext(cfg))

	uploadURL, _, err := svc.PresignedPutURL(
		"chat/2026/05/abc.jpg", "image/jpeg", "", 12345, time.Minute,
	)
	require.NoError(t, err)

	u, err := url.Parse(uploadURL)
	require.NoError(t, err)
	assert.Equal(t, "my-bucket-12345678.cos.ap-beijing.myqcloud.com", u.Host,
		"with BucketURL empty, presigned URL must use canonical COS host")
	assert.Equal(t, "https", u.Scheme,
		"COS canonical endpoint must be HTTPS")
}

// TestCOSPresignedURLs_WithPrefix pins the env-prefix routing: when
// `cosConfig.Prefix` is set (multi-env shared bucket), the prefix
// is prepended to the object key BEFORE signing, so the signed URL
// resolves to the prefixed object on the COS server. This is the
// behaviour `withPrefix` provided in R6 and the R7 host fix must
// not regress.
func TestCOSPresignedURLs_WithPrefix(t *testing.T) {
	cfg := config.New()
	cfg.Test = true
	cfg.COS.SecretID = "test-secret-id"
	cfg.COS.SecretKey = "test-secret-key-1234567890"
	cfg.COS.Bucket = "my-bucket-12345678"
	cfg.COS.Region = "ap-beijing"
	cfg.COS.BucketURL = "https://my-bucket-12345678.cos.example.com"
	cfg.COS.Prefix = "env-test-prefix"

	svc := file.NewServiceCOS(testutil.NewTestContext(cfg))

	uploadURL, _, err := svc.PresignedPutURL(
		"chat/2026/05/abc.jpg", "image/jpeg", "", 12345, time.Minute,
	)
	require.NoError(t, err)

	u, err := url.Parse(uploadURL)
	require.NoError(t, err)
	assert.Equal(t, "my-bucket-12345678.cos.example.com", u.Host,
		"prefix routing must not perturb the BucketURL host")
	assert.Contains(t, u.Path, "/env-test-prefix/chat/2026/05/abc.jpg",
		"signed URL path must include the env prefix, got %s", u.Path)
}

// TestCOSPresignedURLs_HTTPScheme pins that an `http://` BucketURL is
// honoured (non-TLS local emulators or test setups). Going via the
// SDK's `Secure: false` toggle means the signature is computed for
// the http variant — flipping to https post-sign would invalidate it.
func TestCOSPresignedURLs_HTTPScheme(t *testing.T) {
	cfg := config.New()
	cfg.Test = true
	cfg.COS.SecretID = "test-secret-id"
	cfg.COS.SecretKey = "test-secret-key-1234567890"
	cfg.COS.Bucket = "my-bucket-12345678"
	cfg.COS.Region = "ap-beijing"
	cfg.COS.BucketURL = "http://my-bucket-12345678.cos.local"

	svc := file.NewServiceCOS(testutil.NewTestContext(cfg))

	uploadURL, _, err := svc.PresignedPutURL(
		"chat/2026/05/abc.jpg", "image/jpeg", "", 12345, time.Minute,
	)
	require.NoError(t, err)

	u, err := url.Parse(uploadURL)
	require.NoError(t, err)
	assert.Equal(t, "http", u.Scheme, "http BucketURL must produce http presigned URL")
	assert.Equal(t, "my-bucket-12345678.cos.local", u.Host)
}

// TestServiceCOS_PresignedPutURL_CDNAlias pins the YUJ-877 (GH#57) fix:
// when `cosConfig.BucketURL` is a CDN alias domain that does NOT carry a
// `<bucket>.` subdomain (e.g. `https://cdn.deepminer.com.cn`), presigned
// PUT/GET URLs must be signed against the canonical COS endpoint
// (`<bucket>.cos.<region>.myqcloud.com`) because the CDN has no route
// for path-style `/<bucket>/<key>` in the URL path.
//
// Pre-fix behaviour (broken by PR#56 YUJ-846):
//   - publicEndpoint detected the missing `<bucket>.` prefix and
//     returned `BucketLookupPath`
//   - newPublicClient signed against `cdn.example.com` with path-style
//   - the SDK emitted `https://cdn.example.com/<bucket>/<key>`
//   - CDN has no route for `/<bucket>/…` → all presigned URLs 404
//
// Post-fix behaviour (YUJ-877):
//   - CDN alias detected → sign against canonical COS endpoint via
//     getClient (DNS-style: `<bucket>.cos.<region>.myqcloud.com/<key>`)
//   - browser uploads/downloads directly to COS, bypassing CDN
//   - publicURL returns CDN URL without bucket segment for non-presigned
//     download URLs
func TestServiceCOS_PresignedPutURL_CDNAlias(t *testing.T) {
	cfg := config.New()
	cfg.Test = true
	cfg.COS.SecretID = "test-secret-id"
	cfg.COS.SecretKey = "test-secret-key-1234567890"
	cfg.COS.Bucket = "im-data-1255521909"
	cfg.COS.Region = "ap-beijing"
	// CDN alias: host has NO `<bucket>.` subdomain.
	cfg.COS.BucketURL = "https://cdn.example.com"

	svc := file.NewServiceCOS(testutil.NewTestContext(cfg))

	t.Run("PUT URL signed against canonical COS endpoint", func(t *testing.T) {
		uploadURL, downloadURL, err := svc.PresignedPutURL(
			"chat/2026/05/abc.jpg", "image/jpeg", "", 12345, 5*time.Minute,
		)
		require.NoError(t, err)
		require.NotEmpty(t, uploadURL)
		require.NotEmpty(t, downloadURL)

		u, err := url.Parse(uploadURL)
		require.NoError(t, err)

		// Host MUST be the canonical COS endpoint, NOT the CDN host.
		// CDN domains are bucket aliases — presigned URLs go direct to COS.
		assert.Equal(t, "im-data-1255521909.cos.ap-beijing.myqcloud.com", u.Host,
			"CDN alias: presigned PUT URL must be signed against canonical COS endpoint; got %s", u.Host)
		assert.Equal(t, "https", u.Scheme)

		// Path must NOT contain the bucket segment (DNS-style: bucket in host).
		assert.False(t, strings.HasPrefix(u.Path, "/im-data-1255521909/"),
			"CDN alias: presigned PUT URL path must NOT contain bucket segment (DNS-style); got path=%s", u.Path)
		assert.True(t, strings.HasSuffix(u.Path, "/chat/2026/05/abc.jpg"),
			"object key must be reflected in the signed URL path; got path=%s", u.Path)

		// SigV4 shape
		q := u.Query()
		assert.NotEmpty(t, q.Get("X-Amz-Signature"))
		signedHeaders := q.Get("X-Amz-SignedHeaders")
		assert.Contains(t, signedHeaders, "host")
		assert.Contains(t, signedHeaders, "content-length")
	})

	t.Run("GET URL signed against canonical COS endpoint", func(t *testing.T) {
		raw, err := svc.PresignedGetURL(
			"chat/2026/05/abc.jpg", "report.jpg", "attachment", 5*time.Minute,
		)
		require.NoError(t, err)
		require.NotEmpty(t, raw)

		u, err := url.Parse(raw)
		require.NoError(t, err)

		assert.Equal(t, "im-data-1255521909.cos.ap-beijing.myqcloud.com", u.Host,
			"CDN alias: presigned GET URL must be signed against canonical COS endpoint; got %s", u.Host)
		assert.Equal(t, "https", u.Scheme)

		assert.False(t, strings.HasPrefix(u.Path, "/im-data-1255521909/"),
			"CDN alias: presigned GET URL path must NOT contain bucket segment; got path=%s", u.Path)
		assert.True(t, strings.HasSuffix(u.Path, "/chat/2026/05/abc.jpg"),
			"object key must be reflected in the signed GET URL; got path=%s", u.Path)

		signedHeaders := u.Query().Get("X-Amz-SignedHeaders")
		assert.Contains(t, signedHeaders, "host")
	})

	t.Run("download URL uses CDN without bucket segment", func(t *testing.T) {
		_, downloadURL, err := svc.PresignedPutURL(
			"chat/2026/05/abc.jpg", "image/jpeg", "", 12345, 5*time.Minute,
		)
		require.NoError(t, err)

		du, err := url.Parse(downloadURL)
		require.NoError(t, err)

		// Download URL should be CDN-based, no bucket segment.
		assert.Equal(t, "cdn.example.com", du.Host,
			"download URL must use CDN host; got %s", du.Host)
		assert.Equal(t, "/chat/2026/05/abc.jpg", du.Path,
			"CDN download URL must NOT contain bucket segment; got %s", du.Path)
	})
}

// TestServiceCOS_PresignedPutURL_CDNAlias_WithPrefix pins that the
// env-prefix routing keeps working under CDN alias addressing — the
// prefix is prepended to the object key before signing, and the presigned
// URL goes to the canonical COS endpoint (NOT the CDN).
func TestServiceCOS_PresignedPutURL_CDNAlias_WithPrefix(t *testing.T) {
	cfg := config.New()
	cfg.Test = true
	cfg.COS.SecretID = "test-secret-id"
	cfg.COS.SecretKey = "test-secret-key-1234567890"
	cfg.COS.Bucket = "im-data-1255521909"
	cfg.COS.Region = "ap-beijing"
	cfg.COS.BucketURL = "https://cdn.example.com"
	cfg.COS.Prefix = "im-test"

	svc := file.NewServiceCOS(testutil.NewTestContext(cfg))

	uploadURL, _, err := svc.PresignedPutURL(
		"chat/2026/05/abc.jpg", "image/jpeg", "", 12345, time.Minute,
	)
	require.NoError(t, err)

	u, err := url.Parse(uploadURL)
	require.NoError(t, err)
	assert.Equal(t, "im-data-1255521909.cos.ap-beijing.myqcloud.com", u.Host,
		"CDN alias with prefix: presigned URL must use canonical COS endpoint; got %s", u.Host)
	assert.Contains(t, u.Path, "/im-test/chat/2026/05/abc.jpg",
		"presigned URL path must include the env prefix; got path=%s", u.Path)
	// Path must NOT contain bucket segment (DNS-style).
	assert.False(t, strings.Contains(u.Path, "/im-data-1255521909/"),
		"CDN alias: presigned URL path must NOT contain bucket segment; got path=%s", u.Path)
}

// TestServiceCOS_DownloadURL_CDNAlias pins the YUJ-877 (GH#57) fix for
// download URLs: when BucketURL is a CDN alias (no `<bucket>.`
// subdomain), the download URL must be `<CDN>/<key>` WITHOUT a bucket
// segment. CDN domains are bucket aliases — the CDN origin routes to
// the bucket implicitly. Inserting `/<bucket>/` in the URL path was
// the root cause of all 404s on im-test.deepminer.com.cn after PR#56.
func TestServiceCOS_DownloadURL_CDNAlias(t *testing.T) {
	cfg := config.New()
	cfg.Test = true
	cfg.COS.SecretID = "test-secret-id"
	cfg.COS.SecretKey = "test-secret-key-1234567890"
	cfg.COS.Bucket = "im-data-1255521909"
	cfg.COS.Region = "ap-beijing"
	cfg.COS.BucketURL = "https://cdn.example.com"

	svc := file.NewServiceCOS(testutil.NewTestContext(cfg))

	t.Run("plain DownloadURL has no bucket segment", func(t *testing.T) {
		raw, err := svc.DownloadURL("chat/2026/05/abc.jpg", "")
		require.NoError(t, err)
		require.NotEmpty(t, raw)

		u, err := url.Parse(raw)
		require.NoError(t, err)

		assert.Equal(t, "cdn.example.com", u.Host,
			"CDN alias DownloadURL must use CDN host; got %s", u.Host)

		// Path must NOT contain bucket segment — the CDN routes to the
		// bucket implicitly. This is the P0 YUJ-877 fix.
		assert.Equal(t, "/chat/2026/05/abc.jpg", u.Path,
			"CDN alias DownloadURL must NOT contain bucket segment; got path=%s", u.Path)
		assert.False(t, strings.Contains(u.Path, "im-data-1255521909"),
			"CDN alias DownloadURL must NOT contain bucket name anywhere in path; got path=%s", u.Path)
	})

	t.Run("DownloadURL with prefix routes through CDN without bucket", func(t *testing.T) {
		cfg2 := config.New()
		cfg2.Test = true
		cfg2.COS.SecretID = "test-secret-id"
		cfg2.COS.SecretKey = "test-secret-key-1234567890"
		cfg2.COS.Bucket = "im-data-1255521909"
		cfg2.COS.Region = "ap-beijing"
		cfg2.COS.BucketURL = "https://cdn.example.com"
		cfg2.COS.Prefix = "im-test"

		svc2 := file.NewServiceCOS(testutil.NewTestContext(cfg2))

		raw, err := svc2.DownloadURL("chat/2026/05/abc.jpg", "")
		require.NoError(t, err)

		u, err := url.Parse(raw)
		require.NoError(t, err)
		assert.Equal(t, "cdn.example.com", u.Host)
		// Prefix is present, but bucket segment is NOT.
		assert.Equal(t, "/im-test/chat/2026/05/abc.jpg", u.Path,
			"CDN alias DownloadURL with prefix must be `/<prefix>/<key>` without bucket; got path=%s", u.Path)
		assert.False(t, strings.Contains(u.Path, "im-data-1255521909"),
			"CDN alias DownloadURL must NOT contain bucket name; got path=%s", u.Path)
	})
}

// TestServiceCOS_DownloadURL_DNSStyle pins that the bucket-subdomain
// (DNS-style) shape is unchanged — DownloadURL appends the key to the
// BucketURL host (which already carries the `<bucket>.` subdomain) and
// MUST NOT inject another bucket segment into the path.
func TestServiceCOS_DownloadURL_DNSStyle(t *testing.T) {
	cfg := config.New()
	cfg.Test = true
	cfg.COS.SecretID = "test-secret-id"
	cfg.COS.SecretKey = "test-secret-key-1234567890"
	cfg.COS.Bucket = "my-bucket-12345678"
	cfg.COS.Region = "ap-beijing"
	cfg.COS.BucketURL = "https://my-bucket-12345678.cos.example.com"

	svc := file.NewServiceCOS(testutil.NewTestContext(cfg))

	raw, err := svc.DownloadURL("chat/2026/05/abc.jpg", "")
	require.NoError(t, err)

	u, err := url.Parse(raw)
	require.NoError(t, err)
	assert.Equal(t, "my-bucket-12345678.cos.example.com", u.Host,
		"DNS-style DownloadURL must keep BucketURL host verbatim")
	assert.Equal(t, "/chat/2026/05/abc.jpg", u.Path,
		"DNS-style DownloadURL must NOT prepend bucket to path (bucket is already in host); got %s", u.Path)
}

// TestServiceCOS_DownloadURL_DefaultEndpoint pins that BucketURL empty
// falls back to the canonical SDK endpoint
// `https://<bucket>.cos.<region>.myqcloud.com/<key>`. This is the COS
// "no custom domain" deployment shape — the canonical hostname is
// reachable from the browser without any operator-side DNS work.
func TestServiceCOS_DownloadURL_DefaultEndpoint(t *testing.T) {
	cfg := config.New()
	cfg.Test = true
	cfg.COS.SecretID = "test-secret-id"
	cfg.COS.SecretKey = "test-secret-key-1234567890"
	cfg.COS.Bucket = "my-bucket-12345678"
	cfg.COS.Region = "ap-beijing"
	cfg.COS.BucketURL = "" // fallback path

	svc := file.NewServiceCOS(testutil.NewTestContext(cfg))

	raw, err := svc.DownloadURL("chat/2026/05/abc.jpg", "")
	require.NoError(t, err)

	u, err := url.Parse(raw)
	require.NoError(t, err)
	assert.Equal(t, "my-bucket-12345678.cos.ap-beijing.myqcloud.com", u.Host,
		"default DownloadURL must use canonical COS host")
	assert.Equal(t, "https", u.Scheme)
	assert.Equal(t, "/chat/2026/05/abc.jpg", u.Path,
		"default DownloadURL must NOT prepend bucket to path (bucket is already in host); got %s", u.Path)
}

// TestServiceCOS_PresignedPutURL_DownloadURLConsistency verifies the
// relationship between the `uploadUrl` and `downloadUrl` returned by
// `PresignedPutURL` for each BucketURL shape.
//
// For bucket-subdomain and empty BucketURL: upload and download URLs
// share the same host and path (upload signed, download unsigned).
//
// For CDN alias BucketURL (YUJ-877 fix): upload URL points to the
// canonical COS endpoint (`<bucket>.cos.<region>.myqcloud.com`) while
// download URL points to the CDN (`cdn.example.com`). The hosts
// intentionally differ — browser uploads go direct to COS (presigned),
// while reads are served from the CDN (no bucket in path).
func TestServiceCOS_PresignedPutURL_DownloadURLConsistency(t *testing.T) {
	cases := []struct {
		name         string
		bucketURL    string
		prefix       string
		sameHost     bool   // whether upload and download URLs share the same host
		uploadHost   string // expected upload URL host
		downloadHost string // expected download URL host
	}{
		{
			name:         "CDN alias without prefix",
			bucketURL:    "https://cdn.example.com",
			prefix:       "",
			sameHost:     false,
			uploadHost:   "im-data-1255521909.cos.ap-beijing.myqcloud.com",
			downloadHost: "cdn.example.com",
		},
		{
			name:         "CDN alias with env prefix",
			bucketURL:    "https://cdn.example.com",
			prefix:       "im-test",
			sameHost:     false,
			uploadHost:   "im-data-1255521909.cos.ap-beijing.myqcloud.com",
			downloadHost: "cdn.example.com",
		},
		{
			name:         "DNS-style bucket subdomain without prefix",
			bucketURL:    "https://im-data-1255521909.cos.example.com",
			prefix:       "",
			sameHost:     true,
			uploadHost:   "im-data-1255521909.cos.example.com",
			downloadHost: "im-data-1255521909.cos.example.com",
		},
		{
			name:         "DNS-style bucket subdomain with env prefix",
			bucketURL:    "https://im-data-1255521909.cos.example.com",
			prefix:       "im-prod",
			sameHost:     true,
			uploadHost:   "im-data-1255521909.cos.example.com",
			downloadHost: "im-data-1255521909.cos.example.com",
		},
		{
			name:         "BucketURL empty (canonical default endpoint)",
			bucketURL:    "",
			prefix:       "",
			sameHost:     true,
			uploadHost:   "im-data-1255521909.cos.ap-beijing.myqcloud.com",
			downloadHost: "im-data-1255521909.cos.ap-beijing.myqcloud.com",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.New()
			cfg.Test = true
			cfg.COS.SecretID = "test-secret-id"
			cfg.COS.SecretKey = "test-secret-key-1234567890"
			cfg.COS.Bucket = "im-data-1255521909"
			cfg.COS.Region = "ap-beijing"
			cfg.COS.BucketURL = tc.bucketURL
			cfg.COS.Prefix = tc.prefix

			svc := file.NewServiceCOS(testutil.NewTestContext(cfg))

			objectPath := "chat/2026/05/abc.jpg"
			uploadURL, downloadURL, err := svc.PresignedPutURL(
				objectPath, "image/jpeg", "", 12345, time.Minute,
			)
			require.NoError(t, err)
			require.NotEmpty(t, uploadURL)
			require.NotEmpty(t, downloadURL)

			pu, err := url.Parse(uploadURL)
			require.NoError(t, err)
			pd, err := url.Parse(downloadURL)
			require.NoError(t, err)

			assert.Equal(t, tc.uploadHost, pu.Host,
				"upload URL host mismatch")
			assert.Equal(t, tc.downloadHost, pd.Host,
				"download URL host mismatch")

			if tc.sameHost {
				assert.Equal(t, pu.Host, pd.Host,
					"uploadUrl and downloadUrl must share the same host")
				assert.Equal(t, pu.Scheme, pd.Scheme)
				assert.Equal(t, pu.Path, pd.Path,
					"uploadUrl and downloadUrl must address the same object path")
			} else {
				// CDN alias: hosts intentionally differ.
				assert.NotEqual(t, pu.Host, pd.Host,
					"CDN alias: upload (COS) and download (CDN) hosts must differ")
			}
		})
	}
}

// TestServiceCOS_CDNAlias_ProductionRepro is the exact production repro
// from im-test.deepminer.com.cn (GH#57 / YUJ-877). Config:
//
//	cos:
//	  bucketURL: "https://cdn.deepminer.com.cn"
//	  bucket: "im-data-1255521909"
//	  prefix: "im-test"
//	  region: "ap-beijing"
//
// Before the fix:
//   - publicURL("group/26/x.png") = "cdn.deepminer.com.cn/im-data-1255521909/im-test/group/26/x.png" (404!)
//
// After the fix:
//   - publicURL("group/26/x.png") = "cdn.deepminer.com.cn/im-test/group/26/x.png" (200 ✓)
func TestServiceCOS_CDNAlias_ProductionRepro(t *testing.T) {
	cfg := config.New()
	cfg.Test = true
	cfg.COS.SecretID = "test-secret-id"
	cfg.COS.SecretKey = "test-secret-key-1234567890"
	cfg.COS.Bucket = "im-data-1255521909"
	cfg.COS.Region = "ap-beijing"
	cfg.COS.BucketURL = "https://cdn.deepminer.com.cn"
	cfg.COS.Prefix = "im-test"

	svc := file.NewServiceCOS(testutil.NewTestContext(cfg))

	t.Run("download URL must NOT contain bucket segment", func(t *testing.T) {
		raw, err := svc.DownloadURL("group/26/x.png", "")
		require.NoError(t, err)

		// Expected: https://cdn.deepminer.com.cn/im-test/group/26/x.png
		assert.Equal(t, "https://cdn.deepminer.com.cn/im-test/group/26/x.png", raw,
			"production CDN download URL must be <CDN>/<prefix>/<key> without bucket segment")

		u, err := url.Parse(raw)
		require.NoError(t, err)
		assert.Equal(t, "cdn.deepminer.com.cn", u.Host)
		assert.Equal(t, "/im-test/group/26/x.png", u.Path)
		assert.False(t, strings.Contains(raw, "im-data-1255521909"),
			"CDN download URL must NOT contain bucket name; got %s", raw)
	})

	t.Run("presigned PUT URL goes to canonical COS endpoint", func(t *testing.T) {
		uploadURL, downloadURL, err := svc.PresignedPutURL(
			"group/26/x.png", "image/png", "", 9999, 5*time.Minute,
		)
		require.NoError(t, err)

		pu, err := url.Parse(uploadURL)
		require.NoError(t, err)
		assert.Equal(t, "im-data-1255521909.cos.ap-beijing.myqcloud.com", pu.Host,
			"upload URL must go to canonical COS endpoint")

		du, err := url.Parse(downloadURL)
		require.NoError(t, err)
		assert.Equal(t, "cdn.deepminer.com.cn", du.Host,
			"download URL must use CDN host")
		assert.Equal(t, "/im-test/group/26/x.png", du.Path,
			"download URL path must be /<prefix>/<key> without bucket")
	})

	t.Run("presigned GET URL goes to canonical COS endpoint", func(t *testing.T) {
		raw, err := svc.PresignedGetURL("group/26/x.png", "x.png", "attachment", 5*time.Minute)
		require.NoError(t, err)

		u, err := url.Parse(raw)
		require.NoError(t, err)
		assert.Equal(t, "im-data-1255521909.cos.ap-beijing.myqcloud.com", u.Host,
			"presigned GET URL must go to canonical COS endpoint")
	})
}
