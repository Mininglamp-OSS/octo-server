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

// baseS3Cfg returns a config with the three required S3 fields populated
// against fake AWS-shaped values. Tests override individual fields as
// needed.
func baseS3Cfg() *config.Config {
	cfg := config.New()
	cfg.Test = true
	cfg.S3.Endpoint = "s3.us-west-2.amazonaws.com"
	cfg.S3.Region = "us-west-2"
	cfg.S3.Bucket = "octo-test-bucket"
	cfg.S3.AccessKeyID = "AKIAEXAMPLE"
	cfg.S3.SecretAccessKey = "secret-key-1234567890"
	return cfg
}

func TestServiceS3_FailFastOnMissingRequiredConfig(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(c *config.Config)
		wantMsg string
	}{
		{
			name:    "missing endpoint",
			mutate:  func(c *config.Config) { c.S3.Endpoint = "" },
			wantMsg: "s3.endpoint",
		},
		{
			name:    "missing region",
			mutate:  func(c *config.Config) { c.S3.Region = "" },
			wantMsg: "s3.region",
		},
		{
			name:    "missing bucket",
			mutate:  func(c *config.Config) { c.S3.Bucket = "" },
			wantMsg: "s3.bucket",
		},
		{
			name: "all three missing reported together",
			mutate: func(c *config.Config) {
				c.S3.Endpoint = ""
				c.S3.Region = ""
				c.S3.Bucket = ""
			},
			wantMsg: "s3.endpoint, s3.region, s3.bucket",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := baseS3Cfg()
			tc.mutate(cfg)
			svc := file.NewServiceS3(testutil.NewTestContext(cfg))

			_, _, err := svc.PresignedPutURL("chat/foo.jpg", "image/jpeg", "", 1024, time.Minute)
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.wantMsg)
		})
	}
}

func TestServiceS3_PresignedPutURL_SignsAgainstEndpointWhenBucketURLEmpty(t *testing.T) {
	cfg := baseS3Cfg()
	cfg.S3.BucketURL = ""

	svc := file.NewServiceS3(testutil.NewTestContext(cfg))

	uploadURL, downloadURL, err := svc.PresignedPutURL(
		"chat/2026/05/abc.jpg", "image/jpeg", "", 12345, 5*time.Minute,
	)
	require.NoError(t, err)
	require.NotEmpty(t, uploadURL)
	require.NotEmpty(t, downloadURL)

	u, err := url.Parse(uploadURL)
	require.NoError(t, err)

	// Virtual-hosted style against AWS S3: <bucket>.<endpoint>.
	// Note: minio-go internally rewrites Amazon S3 endpoints to the
	// dual-stack variant (s3.dualstack.<region>.amazonaws.com) — this
	// is publicly DNS-resolvable and equivalent for browser access,
	// just cosmetically different from the configured endpoint string.
	// CORS / bucket policy match on bucket identity, not host.
	assert.True(t,
		strings.HasPrefix(u.Host, "octo-test-bucket.s3.") &&
			strings.HasSuffix(u.Host, "us-west-2.amazonaws.com"),
		"presigned PUT URL host must virtual-host the bucket against the region endpoint, got %s", u.Host)
	assert.Equal(t, "https", u.Scheme,
		"S3 backend is HTTPS-only by design")

	q := u.Query()
	assert.NotEmpty(t, q.Get("X-Amz-Signature"),
		"presigned PUT URL must carry a SigV4 signature")
	signedHeaders := q.Get("X-Amz-SignedHeaders")
	assert.Contains(t, signedHeaders, "host",
		"presigned PUT URL must include `host` in signed headers (got %q)", signedHeaders)
	assert.Contains(t, signedHeaders, "content-length",
		"presigned PUT URL must include `content-length` in signed headers to enforce upload size cap (got %q)", signedHeaders)
}

func TestServiceS3_PresignedPutURL_BucketSubdomainBucketURL(t *testing.T) {
	// Bucket-subdomain shape: BucketURL host begins with "<bucket>.".
	// SDK must use BucketLookupDNS against the parent host so the
	// signed URL equals BucketURL+key exactly. Pre-fix this was
	// producing "octo-test-bucket.octo-test-bucket.files.example.com"
	// (double bucket) because the SDK re-attached <bucket>. on top of
	// the already-prefixed host.
	cfg := baseS3Cfg()
	cfg.S3.BucketURL = "https://octo-test-bucket.files.example.com"

	svc := file.NewServiceS3(testutil.NewTestContext(cfg))

	uploadURL, _, err := svc.PresignedPutURL(
		"chat/2026/05/abc.jpg", "image/jpeg", "", 12345, 5*time.Minute,
	)
	require.NoError(t, err)

	u, err := url.Parse(uploadURL)
	require.NoError(t, err)
	// Pinned: presigned URL host must equal BucketURL host exactly —
	// that is the host the browser resolves and the host SigV4
	// canonicalized into the signature.
	assert.Equal(t, "octo-test-bucket.files.example.com", u.Host,
		"bucket-subdomain BucketURL: signed URL host must equal BucketURL host (no double bucket prefix), got %s", u.Host)
}

func TestServiceS3_PresignedPutURL_CDNAliasBucketURL(t *testing.T) {
	// CDN alias shape: BucketURL host has NO "<bucket>." prefix. SDK
	// must use BucketLookupPath so the bucket lives in the URL path
	// (`/<bucket>/<key>`) rather than the SDK silently constructing a
	// "<bucket>.files.example.com" subdomain that does not exist in
	// DNS (the original reviewer-flagged bug).
	cfg := baseS3Cfg()
	cfg.S3.BucketURL = "https://files.example.com"

	svc := file.NewServiceS3(testutil.NewTestContext(cfg))

	uploadURL, _, err := svc.PresignedPutURL(
		"chat/2026/05/abc.jpg", "image/jpeg", "", 12345, 5*time.Minute,
	)
	require.NoError(t, err)

	u, err := url.Parse(uploadURL)
	require.NoError(t, err)
	// Pinned: signed URL host must equal BucketURL host exactly,
	// without an added "<bucket>." subdomain.
	assert.Equal(t, "files.example.com", u.Host,
		"CDN-alias BucketURL: signed URL host must equal BucketURL host (no added bucket subdomain), got %s", u.Host)
	// Bucket lives in the path with path-style addressing.
	assert.Contains(t, u.Path, "/octo-test-bucket/",
		"CDN-alias BucketURL: bucket segment must appear in the URL path, got %s", u.Path)
}

func TestServiceS3_PresignedPutURL_RejectsBucketURLWithPathPrefix(t *testing.T) {
	cfg := baseS3Cfg()
	// Path components on BucketURL would be stripped before SigV4
	// canonical URI computation, producing 403 SignatureDoesNotMatch
	// at the browser. Reject loudly rather than silently dropping the
	// path — same contract as MinioConfig.DownloadURL.
	cfg.S3.BucketURL = "https://cdn.example.com/s3"

	svc := file.NewServiceS3(testutil.NewTestContext(cfg))
	_, _, err := svc.PresignedPutURL("chat/foo.jpg", "image/jpeg", "", 1024, time.Minute)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "路径前缀")
}

func TestServiceS3_PresignedPutURL_RejectsZeroOrNegativeFileSize(t *testing.T) {
	cfg := baseS3Cfg()
	svc := file.NewServiceS3(testutil.NewTestContext(cfg))

	for _, size := range []int64{0, -1, -1024} {
		_, _, err := svc.PresignedPutURL("chat/foo.jpg", "image/jpeg", "", size, time.Minute)
		require.Errorf(t, err, "fileSize=%d must be rejected (Content-Length signing required)", size)
		assert.Contains(t, err.Error(), "fileSize")
	}
}

func TestServiceS3_PresignedURLs_RejectMalformedObjectKeys(t *testing.T) {
	cfg := baseS3Cfg()
	svc := file.NewServiceS3(testutil.NewTestContext(cfg))

	cases := []struct {
		name  string
		input string
	}{
		{"trailing slash on directory key", "chat/dir/"},
		{"embedded double slash", "chat/a//b.png"},
		// Leading slash through the input itself: validatePresignObjectKey
		// already rejects this for MinIO/COS; ServiceS3 must too so the
		// canonical URI never gets normalized mid-flight.
		{"leading slash", "/chat/foo.png"},
	}
	for _, tc := range cases {
		t.Run("PUT/"+tc.name, func(t *testing.T) {
			_, _, err := svc.PresignedPutURL(tc.input, "image/jpeg", "", 1024, time.Minute)
			require.Error(t, err, "PresignedPutURL must reject %q", tc.input)
		})
		t.Run("GET/"+tc.name, func(t *testing.T) {
			_, err := svc.PresignedGetURL(tc.input, "x.jpg", "attachment", time.Minute)
			require.Error(t, err, "PresignedGetURL must reject %q", tc.input)
		})
	}
}

func TestServiceS3_DownloadURL_VirtualHostedStyle(t *testing.T) {
	cfg := baseS3Cfg()
	cfg.S3.BucketURL = ""
	cfg.S3.UsePathStyle = false

	svc := file.NewServiceS3(testutil.NewTestContext(cfg))
	got, err := svc.DownloadURL("chat/2026/abc.jpg", "")
	require.NoError(t, err)
	assert.Equal(t, "https://octo-test-bucket.s3.us-west-2.amazonaws.com/chat/2026/abc.jpg", got)
}

func TestServiceS3_DownloadURL_PathStyle(t *testing.T) {
	cfg := baseS3Cfg()
	cfg.S3.BucketURL = ""
	cfg.S3.UsePathStyle = true

	svc := file.NewServiceS3(testutil.NewTestContext(cfg))
	got, err := svc.DownloadURL("chat/2026/abc.jpg", "")
	require.NoError(t, err)
	assert.Equal(t, "https://s3.us-west-2.amazonaws.com/octo-test-bucket/chat/2026/abc.jpg", got)
}

func TestServiceS3_DownloadURL_BucketURLOverride(t *testing.T) {
	cfg := baseS3Cfg()
	cfg.S3.BucketURL = "https://cdn.example.com"

	svc := file.NewServiceS3(testutil.NewTestContext(cfg))
	got, err := svc.DownloadURL("chat/2026/abc.jpg", "")
	require.NoError(t, err)
	assert.Equal(t, "https://cdn.example.com/chat/2026/abc.jpg", got)
}

func TestServiceS3_DownloadURL_WithPrefix(t *testing.T) {
	cfg := baseS3Cfg()
	cfg.S3.BucketURL = "https://cdn.example.com"
	cfg.S3.Prefix = "prod"

	svc := file.NewServiceS3(testutil.NewTestContext(cfg))
	got, err := svc.DownloadURL("chat/abc.jpg", "")
	require.NoError(t, err)
	assert.Equal(t, "https://cdn.example.com/prod/chat/abc.jpg", got)
}

func TestServiceS3_EndpointTolerates_FullURL(t *testing.T) {
	// Operators sometimes paste the full URL into the endpoint field
	// despite the godoc saying "hostname, without scheme". Trim it
	// silently so we don't surface a confusing SDK error.
	for _, ep := range []string{
		"s3.us-west-2.amazonaws.com",
		"https://s3.us-west-2.amazonaws.com",
		"https://s3.us-west-2.amazonaws.com/",
	} {
		t.Run(ep, func(t *testing.T) {
			cfg := baseS3Cfg()
			cfg.S3.Endpoint = ep
			cfg.S3.BucketURL = ""

			svc := file.NewServiceS3(testutil.NewTestContext(cfg))
			uploadURL, _, err := svc.PresignedPutURL(
				"chat/abc.jpg", "image/jpeg", "", 1024, time.Minute,
			)
			require.NoError(t, err)
			u, err := url.Parse(uploadURL)
			require.NoError(t, err)
			// minio-go canonicalizes AWS endpoints to the dual-stack
			// hostname; the region suffix is what we check for.
			assert.True(t, strings.HasSuffix(u.Host, "us-west-2.amazonaws.com"),
				"URL host should derive from endpoint hostname regardless of input shape, got %s", u.Host)
		})
	}
}

func TestServiceS3_PresignedGetURL_EmitsContentDisposition(t *testing.T) {
	cfg := baseS3Cfg()
	svc := file.NewServiceS3(testutil.NewTestContext(cfg))

	raw, err := svc.PresignedGetURL("chat/abc.jpg", "report 报告.jpg", "attachment", 5*time.Minute)
	require.NoError(t, err)

	u, err := url.Parse(raw)
	require.NoError(t, err)

	disposition := u.Query().Get("response-content-disposition")
	require.NotEmpty(t, disposition,
		"PresignedGetURL must set response-content-disposition for filename overrides")
	assert.Contains(t, disposition, "attachment")
	// Non-ASCII filenames must use RFC 5987 percent-encoding.
	assert.Contains(t, disposition, "UTF-8''",
		"non-ASCII filename should be RFC 5987 encoded, got %q", disposition)
}

func TestServiceS3_PresignedGetURL_InlineDisposition(t *testing.T) {
	cfg := baseS3Cfg()
	svc := file.NewServiceS3(testutil.NewTestContext(cfg))

	raw, err := svc.PresignedGetURL("chat/abc.jpg", "doc.pdf", "inline", 5*time.Minute)
	require.NoError(t, err)

	u, err := url.Parse(raw)
	require.NoError(t, err)
	assert.Contains(t, u.Query().Get("response-content-disposition"), "inline")
}
