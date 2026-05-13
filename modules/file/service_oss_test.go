package file_test

import (
	"bytes"
	"io"
	"testing"

	"github.com/Mininglamp-OSS/octo-server/modules/file"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
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
