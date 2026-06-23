package file

import (
	"errors"
	"io"
	"testing"
)

// fakeUploadService 是 IUploadService 的桩,只实现本测试关心的 DownloadURL,
// 用于验证 Service.DownloadURL 的计时包裹对返回值/错误完全透明。
type fakeUploadService struct {
	url     string
	err     error
	gotPath string
	gotName string
	calls   int
}

func (f *fakeUploadService) UploadFile(string, string, string, func(io.Writer) error) (map[string]interface{}, error) {
	return nil, nil
}

func (f *fakeUploadService) DownloadURL(path string, filename string) (string, error) {
	f.calls++
	f.gotPath, f.gotName = path, filename
	return f.url, f.err
}

func (f *fakeUploadService) GetFile(string) (io.ReadCloser, string, error) {
	return nil, "", nil
}

func TestServiceDownloadURL_TransparentSuccess(t *testing.T) {
	fake := &fakeUploadService{url: "https://cdn.example/obj?sig=abc"}
	s := &Service{uploadService: fake, backend: "minio"}

	got, err := s.DownloadURL("avatar/u1.png", "u1.png")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got != fake.url {
		t.Fatalf("url = %q, want %q", got, fake.url)
	}
	// 参数原样透传,且仅调用一次(包裹不重复调用后端)。
	if fake.calls != 1 || fake.gotPath != "avatar/u1.png" || fake.gotName != "u1.png" {
		t.Fatalf("backend called %d times with (%q,%q)", fake.calls, fake.gotPath, fake.gotName)
	}
}

func TestServiceDownloadURL_TransparentError(t *testing.T) {
	wantErr := errors.New("backend down")
	fake := &fakeUploadService{url: "", err: wantErr}
	s := &Service{uploadService: fake, backend: "oss"}

	got, err := s.DownloadURL("p", "n")
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want %v", err, wantErr)
	}
	if got != "" {
		t.Fatalf("url = %q, want empty on error", got)
	}
	// 指标默认实例未初始化(本测试未注册),ObserveObjectStore 应是 no-op,不影响结果。
}
