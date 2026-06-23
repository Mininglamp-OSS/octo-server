package file

import (
	"errors"
	"io"
	"testing"
)

// fakeUploadService 是 IUploadService 的桩,记录调用并返回预设结果,用于验证
// Service.UploadFile / Service.GetFile 的计时包裹对返回值/错误完全透明。
type fakeUploadService struct {
	// upload
	uploadRes   map[string]interface{}
	uploadErr   error
	uploadCalls int
	// getfile
	getRC      io.ReadCloser
	getCT      string
	getErr     error
	getCalls   int
	gotGetPath string
}

func (f *fakeUploadService) UploadFile(string, string, string, func(io.Writer) error) (map[string]interface{}, error) {
	f.uploadCalls++
	return f.uploadRes, f.uploadErr
}

func (f *fakeUploadService) DownloadURL(path string, filename string) (string, error) {
	return "https://cdn.example/" + path, nil
}

func (f *fakeUploadService) GetFile(path string) (io.ReadCloser, string, error) {
	f.getCalls++
	f.gotGetPath = path
	return f.getRC, f.getCT, f.getErr
}

func TestServiceUploadFile_Transparent(t *testing.T) {
	res := map[string]interface{}{"path": "chat/x.png"}
	fake := &fakeUploadService{uploadRes: res}
	s := &Service{uploadService: fake, backend: "minio"}

	got, err := s.UploadFile("p", "image/png", "inline", nil)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got["path"] != "chat/x.png" {
		t.Fatalf("result not propagated: %v", got)
	}
	if fake.uploadCalls != 1 {
		t.Fatalf("backend called %d times, want 1", fake.uploadCalls)
	}
}

func TestServiceUploadFile_TransparentError(t *testing.T) {
	wantErr := errors.New("put failed")
	fake := &fakeUploadService{uploadErr: wantErr}
	s := &Service{uploadService: fake, backend: "oss"}

	got, err := s.UploadFile("p", "image/png", "inline", nil)
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want %v", err, wantErr)
	}
	if got != nil {
		t.Fatalf("result = %v, want nil on error", got)
	}
}

func TestServiceGetFile_Transparent(t *testing.T) {
	rc := io.NopCloser(nil)
	fake := &fakeUploadService{getRC: rc, getCT: "image/png"}
	s := &Service{uploadService: fake, backend: "minio"}

	gotRC, gotCT, err := s.GetFile("avatar/u1.png")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if gotCT != "image/png" {
		t.Fatalf("contentType = %q, want image/png", gotCT)
	}
	if gotRC != rc {
		t.Fatal("ReadCloser not propagated unchanged")
	}
	// 参数原样透传,仅调用一次(包裹不重复调用后端)。
	if fake.getCalls != 1 || fake.gotGetPath != "avatar/u1.png" {
		t.Fatalf("backend called %d times with %q", fake.getCalls, fake.gotGetPath)
	}
}

func TestServiceGetFile_TransparentError(t *testing.T) {
	wantErr := errors.New("get failed")
	fake := &fakeUploadService{getErr: wantErr}
	s := &Service{uploadService: fake, backend: "s3"}

	gotRC, _, err := s.GetFile("p")
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want %v", err, wantErr)
	}
	if gotRC != nil {
		t.Fatal("ReadCloser should be nil on error")
	}
	// 指标默认实例未初始化时 ObserveObjectStore 是 no-op,不影响结果。
}

// DownloadURL 不再打点(本地拼串无 I/O,#442 P1-1);仍验证其行为透明。
func TestServiceDownloadURL_StillTransparentNoMetric(t *testing.T) {
	fake := &fakeUploadService{}
	s := &Service{uploadService: fake, backend: "seaweedfs"}

	got, err := s.DownloadURL("chat/x.png", "")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got != "https://cdn.example/chat/x.png" {
		t.Fatalf("url = %q", got)
	}
}
