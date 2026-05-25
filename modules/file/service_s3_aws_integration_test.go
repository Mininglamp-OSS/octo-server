package file_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/Mininglamp-OSS/octo-server/modules/file"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// requireAWSS3Env collects the env vars driving the live-AWS integration
// test. If any required variable is missing the test skips with a
// pointer to the README — keeps the suite green on CI runners without
// credentials and on contributor laptops.
//
// Required env vars (mirror cfg.S3 fields):
//
//	TS_S3_TEST_ENDPOINT          e.g. s3.us-west-2.amazonaws.com
//	TS_S3_TEST_REGION            e.g. us-west-2
//	TS_S3_TEST_BUCKET            pre-existing bucket the test can write to
//	TS_S3_TEST_ACCESS_KEY_ID
//	TS_S3_TEST_SECRET_ACCESS_KEY
//
// Optional:
//
//	TS_S3_TEST_DOWNLOAD_URL      DownloadURL override (CDN front, custom domain). Unsigned URL prefix only.
//	TS_S3_TEST_USE_PATH_STYLE    "true" to force path-style addressing
type awsS3TestEnv struct {
	Endpoint        string
	Region          string
	Bucket          string
	AccessKeyID     string
	SecretAccessKey string
	DownloadURL     string
	UsePathStyle    bool
}

func requireAWSS3Env(t *testing.T) awsS3TestEnv {
	t.Helper()
	env := awsS3TestEnv{
		Endpoint:        os.Getenv("TS_S3_TEST_ENDPOINT"),
		Region:          os.Getenv("TS_S3_TEST_REGION"),
		Bucket:          os.Getenv("TS_S3_TEST_BUCKET"),
		AccessKeyID:     os.Getenv("TS_S3_TEST_ACCESS_KEY_ID"),
		SecretAccessKey: os.Getenv("TS_S3_TEST_SECRET_ACCESS_KEY"),
		DownloadURL:     os.Getenv("TS_S3_TEST_DOWNLOAD_URL"),
		UsePathStyle:    strings.EqualFold(os.Getenv("TS_S3_TEST_USE_PATH_STYLE"), "true"),
	}
	missing := make([]string, 0, 5)
	if env.Endpoint == "" {
		missing = append(missing, "TS_S3_TEST_ENDPOINT")
	}
	if env.Region == "" {
		missing = append(missing, "TS_S3_TEST_REGION")
	}
	if env.Bucket == "" {
		missing = append(missing, "TS_S3_TEST_BUCKET")
	}
	if env.AccessKeyID == "" {
		missing = append(missing, "TS_S3_TEST_ACCESS_KEY_ID")
	}
	if env.SecretAccessKey == "" {
		missing = append(missing, "TS_S3_TEST_SECRET_ACCESS_KEY")
	}
	if len(missing) > 0 {
		t.Skipf("AWS S3 integration test skipped; missing env vars: %s", strings.Join(missing, ", "))
	}
	return env
}

func newAWSS3TestService(t *testing.T, env awsS3TestEnv) (*file.ServiceS3, *config.Config) {
	t.Helper()
	cfg := config.New()
	cfg.Test = true
	cfg.S3.Endpoint = env.Endpoint
	cfg.S3.Region = env.Region
	cfg.S3.Bucket = env.Bucket
	cfg.S3.AccessKeyID = env.AccessKeyID
	cfg.S3.SecretAccessKey = env.SecretAccessKey
	cfg.S3.DownloadURL = env.DownloadURL
	cfg.S3.UsePathStyle = env.UsePathStyle
	// Use a unique prefix per test run so concurrent runs (CI parallelism)
	// and re-runs after partial failures don't collide.
	cfg.S3.Prefix = "octo-integration-test/" + runID(t)
	return file.NewServiceS3(testutil.NewTestContext(cfg)), cfg
}

func runID(t *testing.T) string {
	t.Helper()
	b := make([]byte, 6)
	_, err := rand.Read(b)
	require.NoError(t, err)
	return time.Now().UTC().Format("20060102-150405-") + hex.EncodeToString(b)
}

// newAWSS3RawClient returns a minio-go client used for out-of-band
// cleanup (RemoveObject). It deliberately mirrors ServiceS3.newClient
// — drift here would mask test pollution, not hide a real bug.
func newAWSS3RawClient(t *testing.T, env awsS3TestEnv) *minio.Client {
	t.Helper()
	endpoint := env.Endpoint
	endpoint = strings.TrimPrefix(endpoint, "https://")
	endpoint = strings.TrimPrefix(endpoint, "http://")
	endpoint = strings.TrimRight(endpoint, "/")

	lookup := minio.BucketLookupDNS
	if env.UsePathStyle {
		lookup = minio.BucketLookupPath
	}
	c, err := minio.New(endpoint, &minio.Options{
		Creds:        credentials.NewStaticV4(env.AccessKeyID, env.SecretAccessKey, ""),
		Secure:       true,
		Region:       env.Region,
		BucketLookup: lookup,
	})
	require.NoError(t, err)
	return c
}

// TestAWSS3Integration_UploadDownloadRoundtrip covers the multipart-style
// UploadFile path the server uses for /v1/file/upload, then verifies the
// uploaded object is readable via GetFile and via the (unsigned)
// DownloadURL when applicable.
func TestAWSS3Integration_UploadDownloadRoundtrip(t *testing.T) {
	env := requireAWSS3Env(t)
	svc, cfg := newAWSS3TestService(t, env)
	rawClient := newAWSS3RawClient(t, env)

	payload := []byte("octo-integration-test payload\n")
	relPath := "chat/2026/05/roundtrip.txt"
	keyForCleanup := strings.TrimPrefix(cfg.S3.Prefix+"/"+relPath, "/")
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = rawClient.RemoveObject(ctx, env.Bucket, keyForCleanup, minio.RemoveObjectOptions{})
	})

	res, err := svc.UploadFile(relPath, "text/plain; charset=utf-8", "",
		func(w io.Writer) error {
			_, err := w.Write(payload)
			return err
		})
	require.NoError(t, err)
	require.NotEmpty(t, res["path"])

	// GetFile (server-side proxy path) — body and content type must match.
	body, ct, err := svc.GetFile(relPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = body.Close() })
	gotBytes, err := io.ReadAll(body)
	require.NoError(t, err)
	assert.Equal(t, payload, gotBytes, "GetFile body must equal upload payload")
	assert.Contains(t, ct, "text/plain",
		"GetFile content type must echo upload content type, got %q", ct)
}

// TestAWSS3Integration_PresignedPUTRoundtrip drives the full browser
// direct-upload flow end-to-end against real AWS:
//
//  1. Call PresignedPutURL — server emits the signed URL and the
//     contract headers the client must echo.
//  2. Perform the PUT exactly as a browser would, echoing
//     Content-Type / Content-Length / Content-Disposition. AWS S3
//     returns 200 only if the signature, the byte budget, and the
//     header echo all match.
//  3. Verify the object body via PresignedGetURL — the typical
//     paired flow exposed at /v1/file/download/url.
//
// This is the regression test that catches signature-shape drift:
// e.g. someone "fixes" Content-Length to be unsigned and the upload
// silently accepts arbitrary-size payloads, bypassing MaxFileSize.
func TestAWSS3Integration_PresignedPUTRoundtrip(t *testing.T) {
	env := requireAWSS3Env(t)
	svc, cfg := newAWSS3TestService(t, env)
	rawClient := newAWSS3RawClient(t, env)

	payload := []byte("octo-integration-test presigned PUT payload xyz\n")
	relPath := "chat/2026/05/presigned.txt"
	keyForCleanup := strings.TrimPrefix(cfg.S3.Prefix+"/"+relPath, "/")
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = rawClient.RemoveObject(ctx, env.Bucket, keyForCleanup, minio.RemoveObjectOptions{})
	})

	contentType := "text/plain; charset=utf-8"
	contentDisposition := `inline; filename="presigned.txt"`
	uploadURL, _, err := svc.PresignedPutURL(
		relPath, contentType, contentDisposition, int64(len(payload)), 5*time.Minute,
	)
	require.NoError(t, err)
	require.NotEmpty(t, uploadURL)

	doPresignedPUT(t, uploadURL, contentType, contentDisposition, payload)

	// Verify via PresignedGetURL — full HTTP roundtrip mirrors the
	// /v1/file/download/url contract on the public API.
	getURL, err := svc.PresignedGetURL(relPath, "presigned.txt", "attachment", 5*time.Minute)
	require.NoError(t, err)

	body := doPresignedGET(t, getURL)
	defer body.Close()

	got, err := io.ReadAll(body)
	require.NoError(t, err)
	assert.Equal(t, payload, got)
}

// TestAWSS3Integration_PresignedPUTRejectsSizeMismatch is the size-bypass
// guard test. The server signed Content-Length: N; the browser sends
// fewer bytes than that. AWS S3 rejects on Content-Length mismatch
// (XAmzContentSHA256Mismatch / signature mismatch family), validating
// that the presigned-URL path cannot be coerced into uploading less
// data than the server budgeted for.
//
// (A test for MORE bytes than signed is harder to construct without
// fighting the http library — http.Client computes Content-Length
// from the body length automatically. The presence of Content-Length
// in SignedHeaders, asserted in service_s3_test.go, is the offline
// guarantee that the same rejection happens for over-size.)
func TestAWSS3Integration_PresignedPUTRejectsSizeMismatch(t *testing.T) {
	env := requireAWSS3Env(t)
	svc, _ := newAWSS3TestService(t, env)

	signedSize := int64(1024) // server says: signing for exactly 1024 bytes
	uploadURL, _, err := svc.PresignedPutURL(
		"chat/2026/05/should-not-land.txt", "text/plain", "", signedSize, 5*time.Minute,
	)
	require.NoError(t, err)

	body := bytes.Repeat([]byte("a"), 100) // sending only 100 bytes
	req, err := http.NewRequest(http.MethodPut, uploadURL, bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "text/plain")
	req.ContentLength = int64(len(body))

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	require.NotEqual(t, http.StatusOK, resp.StatusCode,
		"PUT with body size != signed Content-Length must be rejected; status=%d", resp.StatusCode)
	// AWS responds with 403 SignatureDoesNotMatch on Content-Length
	// mismatch for SigV4 presigned URLs. Use 4xx as the contract
	// (some providers return 400 for the same case).
	assert.GreaterOrEqual(t, resp.StatusCode, 400)
	assert.Less(t, resp.StatusCode, 500,
		"size-mismatch rejection must be a 4xx, got %d", resp.StatusCode)
}

// TestAWSS3Integration_DownloadURLShape sanity-checks that the public
// (unsigned) download URL points at the configured DownloadURL host
// when set, and at the canonical S3 hostname when not. We don't
// follow the URL — for private buckets it will 403 — but the host /
// path shape is what /v1/file/preview hands the browser.
func TestAWSS3Integration_DownloadURLShape(t *testing.T) {
	env := requireAWSS3Env(t)
	svc, cfg := newAWSS3TestService(t, env)

	relPath := "chat/2026/abc.jpg"
	got, err := svc.DownloadURL(relPath, "")
	require.NoError(t, err)
	u, err := url.Parse(got)
	require.NoError(t, err)

	expectedSuffix := "/" + cfg.S3.Prefix + "/" + relPath
	assert.True(t, strings.HasSuffix(u.EscapedPath(), expectedSuffix) ||
		strings.HasSuffix(u.EscapedPath(), expectedSuffix[1:]),
		"DownloadURL path must include the configured prefix + object path, got %s", u.EscapedPath())
	if env.DownloadURL != "" {
		bu, _ := url.Parse(env.DownloadURL)
		assert.Equal(t, bu.Host, u.Host,
			"DownloadURL host must dominate the unsigned URL host when set")
	}
}

func doPresignedPUT(t *testing.T, signedURL, contentType, contentDisposition string, payload []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPut, signedURL, bytes.NewReader(payload))
	require.NoError(t, err)
	req.Header.Set("Content-Type", contentType)
	if contentDisposition != "" {
		req.Header.Set("Content-Disposition", contentDisposition)
	}
	req.ContentLength = int64(len(payload))

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	require.Equalf(t, http.StatusOK, resp.StatusCode,
		"presigned PUT must succeed; status=%d body=%s", resp.StatusCode, string(respBody))
}

func doPresignedGET(t *testing.T, signedURL string) io.ReadCloser {
	t.Helper()
	resp, err := http.Get(signedURL) //nolint:gosec // test code, URL comes from our own signer
	require.NoError(t, err)
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("presigned GET failed: status=%d body=%s", resp.StatusCode, string(body))
	}
	return resp.Body
}

