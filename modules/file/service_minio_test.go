package file_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/Mininglamp-OSS/octo-server/modules/file"
	"github.com/stretchr/testify/require"
)

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
		// Leading-slash bypass of the `//` rejection: input `chat//foo.png`
		// splits to bucket=`chat`, objectKey=`/foo.png` (no embedded `//`,
		// not trailing-slash, not empty). Without an explicit leading-slash
		// guard the canonical URI becomes `/chat//foo.png` and gateway
		// normalization rewrites it to `/chat/foo.png` mid-flight, breaking
		// signature validation. Pinned in PR#50 R5 codex finding 2.3.
		{"leading slash via bucket-separator boundary", "chat//foo.png"},
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
