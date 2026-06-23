package file

import (
	"errors"
	"io"
	"testing"

	"github.com/Mininglamp-OSS/octo-server/pkg/metrics"
	"github.com/prometheus/client_golang/prometheus"
)

// fakeUploadService 是 IUploadService 的桩,记录调用并返回预设结果,用于验证
// Service.UploadFile / Service.GetFile 的计时包裹对返回值/错误完全透明,
// 并且发出预期的依赖指标。
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

// newDepRegistry 装一个独立 registry 作为依赖指标的进程默认,供断言指标发射。
// 返回的 registry 用于 Gather;返回的 restore 在测试结束时恢复包级默认,避免
// 跨测试污染(metrics 包未导出 reset,故用 NewDependencyMetrics 重设 + cleanup)。
func newDepRegistry(t *testing.T) *prometheus.Registry {
	t.Helper()
	reg := prometheus.NewRegistry()
	metrics.NewDependencyMetrics(reg) // 同时把该实例设为包级默认
	return reg
}

// depOpSampleCount 返回 dmwork_dependency_duration_seconds 上带指定 op label 的
// 观测次数(SampleCount)。找不到该 op 序列返回 0。
func depOpSampleCount(t *testing.T, reg *prometheus.Registry, op string) uint64 {
	t.Helper()
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	var n uint64
	for _, mf := range mfs {
		if mf.GetName() != "dmwork_dependency_duration_seconds" {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "op" && l.GetValue() == op {
					n += m.GetHistogram().GetSampleCount()
				}
			}
		}
	}
	return n
}

func TestServiceUploadFile_TransparentAndObserved(t *testing.T) {
	reg := newDepRegistry(t)
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
	// 指标:恰好 1 次 op=upload_file 观测。
	if c := depOpSampleCount(t, reg, metrics.OpUploadFile); c != 1 {
		t.Fatalf("op=upload_file sample count = %d, want 1", c)
	}
}

func TestServiceUploadFile_TransparentError(t *testing.T) {
	reg := newDepRegistry(t)
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
	// 错误路径也计入(status=error 由观测层区分),仍是 op=upload_file。
	if c := depOpSampleCount(t, reg, metrics.OpUploadFile); c != 1 {
		t.Fatalf("op=upload_file sample count = %d, want 1", c)
	}
}

func TestServiceGetFile_TransparentAndObserved(t *testing.T) {
	reg := newDepRegistry(t)
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
	if fake.getCalls != 1 || fake.gotGetPath != "avatar/u1.png" {
		t.Fatalf("backend called %d times with %q", fake.getCalls, fake.gotGetPath)
	}
	// 指标:恰好 1 次 op=get_file 观测。
	if c := depOpSampleCount(t, reg, metrics.OpGetFile); c != 1 {
		t.Fatalf("op=get_file sample count = %d, want 1", c)
	}
}

func TestServiceGetFile_TransparentError(t *testing.T) {
	reg := newDepRegistry(t)
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
	if c := depOpSampleCount(t, reg, metrics.OpGetFile); c != 1 {
		t.Fatalf("op=get_file sample count = %d, want 1", c)
	}
}

// DownloadURL 是本地拼串、无 I/O,不应打点(#442 P1-1)。这条测试是 load-bearing:
// 它既验证返回值透明,又断言调用后注册表里**不存在** op=download_url 序列 ——
// 若有人误把 DownloadURL 重新包上 ObserveObjectStore,这里会失败。
func TestServiceDownloadURL_TransparentAndNoMetric(t *testing.T) {
	reg := newDepRegistry(t)
	fake := &fakeUploadService{}
	s := &Service{uploadService: fake, backend: "seaweedfs"}

	got, err := s.DownloadURL("chat/x.png", "")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got != "https://cdn.example/chat/x.png" {
		t.Fatalf("url = %q", got)
	}
	// 关键断言:DownloadURL 不发任何 op=download_url 指标。
	if c := depOpSampleCount(t, reg, "download_url"); c != 0 {
		t.Fatalf("op=download_url sample count = %d, want 0 (DownloadURL must not be instrumented)", c)
	}
}
