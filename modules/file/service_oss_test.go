package file_test

import (
	"bytes"
	"io"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/Mininglamp-OSS/octo-server/modules/file"
	"github.com/stretchr/testify/assert"
)

func TestOSSUpload(t *testing.T) {
	// Manual integration check against Aliyun OSS — skipped in CI because
	// the fixture below ships placeholder credentials (`xxxx` / `xxxxxx`)
	// and an empty bucket; ServiceOSS rejects with
	// `bucket name len is between [3-63]`. To run locally, drop real
	// values into the cfg literal and remove this Skip.
	t.Skip("requires real Aliyun OSS credentials; manual-only fixture")

	cfg := config.New()
	ctx := testutil.NewTestContext(cfg)
	cfg.OSS.Endpoint = "oss-cn-shanghai.aliyuncs.com"
	cfg.OSS.AccessKeyID = "xxxx"
	cfg.OSS.AccessKeySecret = "xxxxxx"

	service := file.NewServiceOSS(ctx)
	_, err := service.UploadFile("chat/zdd/fjj.txt", "*", "", func(writer io.Writer) error {
		_, err := writer.Write(bytes.NewBufferString("this is test content").Bytes())
		return err
	})
	assert.NoError(t, err)

}

// TestOSSDownloadURL_RoutesThroughNormalizer is the regression test for
// PR#50 R6 P1 (lml2468): `ServiceOSS.DownloadURL` must apply the same
// `<BucketName>/` strip that `UploadFile` and `PresignedPutURL` apply,
// otherwise a deployer with `OSS.BucketName == "chat"` ends up with the
// asymmetric pair:
//
//	upload path  → object stored as `2025/x.png` (prefix stripped)
//	download URL → `<BucketURL>/chat/2025/x.png` (prefix kept) → 404
//
// The fix is one line — route DownloadURL through `normalizeOSSObjectKey`
// — but the asymmetry is exactly the kind of latent bug that a focused
// unit test prevents from regressing under future refactors. The cases
// below pin both the bucket-name-equals-prefix path and the negative
// case (bucket name not matching the first segment) so we can refactor
// the helper without breaking either one.
func TestOSSDownloadURL_RoutesThroughNormalizer(t *testing.T) {
	cases := []struct {
		name       string
		bucketName string
		bucketURL  string
		input      string
		want       string
	}{
		{
			name:       "bucket name equals fileType prefix (chat) — the lml2468 case",
			bucketName: "chat",
			bucketURL:  "https://chat.oss-cn-hangzhou.aliyuncs.com",
			input:      "chat/2025/x.png",
			want:       "https://chat.oss-cn-hangzhou.aliyuncs.com/2025/x.png",
		},
		{
			name:       "bucket name equals fileType prefix with leading slash",
			bucketName: "chat",
			bucketURL:  "https://chat.oss-cn-hangzhou.aliyuncs.com",
			input:      "/chat/2025/x.png",
			want:       "https://chat.oss-cn-hangzhou.aliyuncs.com/2025/x.png",
		},
		{
			name:       "bucket name does not match first segment — path preserved",
			bucketName: "my-bucket",
			bucketURL:  "https://my-bucket.oss-cn-hangzhou.aliyuncs.com",
			input:      "chat/2025/x.png",
			want:       "https://my-bucket.oss-cn-hangzhou.aliyuncs.com/chat/2025/x.png",
		},
		{
			name:       "bucket name is non-segment prefix only (ch vs chat) — path preserved",
			bucketName: "ch",
			bucketURL:  "https://ch.oss-cn-hangzhou.aliyuncs.com",
			input:      "chat/2025/x.png",
			want:       "https://ch.oss-cn-hangzhou.aliyuncs.com/chat/2025/x.png",
		},
		{
			name:       "explicit bucket-name segment stripped",
			bucketName: "my-bucket",
			bucketURL:  "https://my-bucket.oss-cn-hangzhou.aliyuncs.com",
			input:      "my-bucket/chat/2025/x.png",
			want:       "https://my-bucket.oss-cn-hangzhou.aliyuncs.com/chat/2025/x.png",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.New()
			cfg.Test = true
			cfg.OSS.Endpoint = "oss-cn-hangzhou.aliyuncs.com"
			cfg.OSS.AccessKeyID = "test-access-key"
			cfg.OSS.AccessKeySecret = "test-secret-access-key"
			cfg.OSS.BucketName = tc.bucketName
			cfg.OSS.BucketURL = tc.bucketURL

			svc := file.NewServiceOSS(testutil.NewTestContext(cfg))
			got, err := svc.DownloadURL(tc.input, "")
			assert.NoError(t, err)
			assert.Equal(t, tc.want, got,
				"DownloadURL must route through the same normalizer as UploadFile / PresignedPutURL")
		})
	}
}
