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

// baseS3Cfg returns a config with all required S3 fields populated
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
			name:    "missing accessKeyID",
			mutate:  func(c *config.Config) { c.S3.AccessKeyID = "" },
			wantMsg: "s3.accessKeyID",
		},
		{
			name:    "missing secretAccessKey",
			mutate:  func(c *config.Config) { c.S3.SecretAccessKey = "" },
			wantMsg: "s3.secretAccessKey",
		},
		{
			name: "all required fields missing reported together",
			mutate: func(c *config.Config) {
				c.S3.Endpoint = ""
				c.S3.Region = ""
				c.S3.Bucket = ""
				c.S3.AccessKeyID = ""
				c.S3.SecretAccessKey = ""
			},
			wantMsg: "s3.endpoint, s3.region, s3.bucket, s3.accessKeyID, s3.secretAccessKey",
		},
		{
			name: "SessionToken empty is fine (optional field)",
			mutate: func(c *config.Config) {
				c.S3.SessionToken = ""
			},
			wantMsg: "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := baseS3Cfg()
			tc.mutate(cfg)
			svc := file.NewServiceS3(testutil.NewTestContext(cfg))

			_, _, err := svc.PresignedPutURL("chat/foo.jpg", "image/jpeg", "", 1024, time.Minute)
			if tc.wantMsg == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.wantMsg)
		})
	}
}

func TestServiceS3_PresignedPutURL_AlwaysSignsAgainstEndpoint(t *testing.T) {
	// Core regression: presigned PUT URL host must equal cfg.S3.Endpoint
	// (with the bucket virtual-hosted in front), regardless of whether
	// DownloadURL is set. DownloadURL is for unsigned browser URLs only.
	cfg := baseS3Cfg()
	cfg.S3.DownloadURL = ""

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

// TestServiceS3_PresignedURL_DownloadURLDoesNotAffectSigning pins the
// soul of the post-#43 contract: presigned PUT/GET URLs must ALWAYS
// sign against cfg.S3.Endpoint, no matter what DownloadURL is. If a
// future change accidentally re-introduces DownloadURL into the signing
// path (e.g. by reading it in newClient), this test will catch the
// signature-host drift before it ships.
func TestServiceS3_PresignedURL_DownloadURLDoesNotAffectSigning(t *testing.T) {
	endpointHostSuffix := "us-west-2.amazonaws.com"

	for _, downloadURL := range []string{
		"https://d123.cloudfront.net",                 // CloudFront alias
		"https://files.example.com",                   // generic CDN alias
		"https://octo-test-bucket.cdn.example.com",    // bucket-subdomain custom domain
		"http://localhost:9000",                       // local emulator
	} {
		t.Run(downloadURL, func(t *testing.T) {
			cfg := baseS3Cfg()
			cfg.S3.DownloadURL = downloadURL

			svc := file.NewServiceS3(testutil.NewTestContext(cfg))

			// PUT
			uploadURL, _, err := svc.PresignedPutURL(
				"chat/2026/05/abc.jpg", "image/jpeg", "", 1024, 5*time.Minute,
			)
			require.NoError(t, err)
			pu, err := url.Parse(uploadURL)
			require.NoError(t, err)
			assert.True(t, strings.HasSuffix(pu.Host, endpointHostSuffix),
				"presigned PUT host must derive from Endpoint, not DownloadURL=%q (got %s)", downloadURL, pu.Host)
			assert.Equal(t, "https", pu.Scheme,
				"presigned PUT must always be HTTPS regardless of DownloadURL scheme")

			// GET
			getURL, err := svc.PresignedGetURL("chat/abc.jpg", "x.jpg", "attachment", time.Minute)
			require.NoError(t, err)
			gu, err := url.Parse(getURL)
			require.NoError(t, err)
			assert.True(t, strings.HasSuffix(gu.Host, endpointHostSuffix),
				"presigned GET host must derive from Endpoint, not DownloadURL=%q (got %s)", downloadURL, gu.Host)
			assert.Equal(t, "https", gu.Scheme)
		})
	}
}

// TestServiceS3_SessionToken_AppearsInSignedURL covers STS / IRSA / IMDSv2
// rotating-credentials workflows: when cfg.S3.SessionToken is set,
// minio-go must add X-Amz-Security-Token to the presigned URL query, or
// AWS will reject the request as "InvalidToken / The security token
// included in the request is invalid."
func TestServiceS3_SessionToken_AppearsInSignedURL(t *testing.T) {
	cfg := baseS3Cfg()
	cfg.S3.SessionToken = "IQoJb3JpZ2luX2VjEXAMPLESESSIONTOKENvalueGRwEAAaCXVzLWVhc3QtMQ=="

	svc := file.NewServiceS3(testutil.NewTestContext(cfg))

	uploadURL, _, err := svc.PresignedPutURL(
		"chat/abc.jpg", "image/jpeg", "", 1024, time.Minute,
	)
	require.NoError(t, err)
	u, err := url.Parse(uploadURL)
	require.NoError(t, err)

	token := u.Query().Get("X-Amz-Security-Token")
	require.NotEmpty(t, token,
		"presigned URL must carry X-Amz-Security-Token when SessionToken is configured (STS path)")
	assert.Equal(t, cfg.S3.SessionToken, token,
		"X-Amz-Security-Token must equal the configured SessionToken verbatim")

	// GET path mirrors PUT.
	getURL, err := svc.PresignedGetURL("chat/abc.jpg", "x.jpg", "attachment", time.Minute)
	require.NoError(t, err)
	gu, _ := url.Parse(getURL)
	assert.Equal(t, cfg.S3.SessionToken, gu.Query().Get("X-Amz-Security-Token"))
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
	cfg.S3.DownloadURL = ""
	cfg.S3.UsePathStyle = false

	svc := file.NewServiceS3(testutil.NewTestContext(cfg))
	got, err := svc.DownloadURL("chat/2026/abc.jpg", "")
	require.NoError(t, err)
	assert.Equal(t, "https://octo-test-bucket.s3.us-west-2.amazonaws.com/chat/2026/abc.jpg", got)
}

func TestServiceS3_DownloadURL_PathStyle(t *testing.T) {
	cfg := baseS3Cfg()
	cfg.S3.DownloadURL = ""
	cfg.S3.UsePathStyle = true

	svc := file.NewServiceS3(testutil.NewTestContext(cfg))
	got, err := svc.DownloadURL("chat/2026/abc.jpg", "")
	require.NoError(t, err)
	assert.Equal(t, "https://s3.us-west-2.amazonaws.com/octo-test-bucket/chat/2026/abc.jpg", got)
}

func TestServiceS3_DownloadURL_DownloadURLOverride(t *testing.T) {
	cfg := baseS3Cfg()
	cfg.S3.DownloadURL = "https://cdn.example.com"

	svc := file.NewServiceS3(testutil.NewTestContext(cfg))
	got, err := svc.DownloadURL("chat/2026/abc.jpg", "")
	require.NoError(t, err)
	assert.Equal(t, "https://cdn.example.com/chat/2026/abc.jpg", got)
}

func TestServiceS3_DownloadURL_WithPrefix(t *testing.T) {
	cfg := baseS3Cfg()
	cfg.S3.DownloadURL = "https://cdn.example.com"
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
			cfg.S3.DownloadURL = ""

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
