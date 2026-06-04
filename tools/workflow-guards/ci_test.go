package workflowguards

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"gopkg.in/yaml.v3"
)

type workflow struct {
	Jobs map[string]job `yaml:"jobs"`
}

type job struct {
	Services map[string]service `yaml:"services"`
	Steps    []step             `yaml:"steps"`
}

type service struct {
	Image   string                 `yaml:"image"`
	Env     map[string]interface{} `yaml:"env"`
	Ports   []string               `yaml:"ports"`
	Options string                 `yaml:"options"`
}

type step struct {
	Name string `yaml:"name"`
}

func TestCITestJobProvidesWuKongIM(t *testing.T) {
	wf := readWorkflow(t, ".github/workflows/ci.yml")
	testJob, ok := wf.Jobs["test"]
	if !ok {
		t.Fatal("ci.yml must define the test job")
	}

	wk, ok := testJob.Services["wukongim"]
	if !ok {
		t.Fatal("CI test job must provision a wukongim service for IM-channel integration tests")
	}
	if wk.Image != "wukongim/wukongim:v2.2.4-20260313" {
		t.Fatalf("wukongim service image = %q, want pinned wukongim/wukongim:v2.2.4-20260313", wk.Image)
	}
	if !contains(wk.Ports, "5001:5001") {
		t.Fatalf("wukongim service ports = %v, want 5001:5001 for octo-lib's default APIURL", wk.Ports)
	}
	if got := wk.Env["WK_TOKENAUTHON"]; got != "false" {
		t.Fatalf("wukongim WK_TOKENAUTHON = %#v, want \"false\" so tests can use an empty manager token", got)
	}
	if wk.Options == "" {
		t.Fatal("wukongim service must define a health check")
	}

	waitIdx := stepIndex(testJob.Steps, "Wait for WuKongIM")
	runIdx := stepIndex(testJob.Steps, "Run tests")
	if waitIdx < 0 {
		t.Fatal("CI test job must wait for WuKongIM before running Go tests")
	}
	if runIdx < 0 {
		t.Fatal("CI test job must define the Run tests step")
	}
	if waitIdx > runIdx {
		t.Fatal("Wait for WuKongIM must run before Run tests")
	}
}

func readWorkflow(t *testing.T, rel string) workflow {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "../.."))
	b, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		t.Fatal(err)
	}
	var wf workflow
	if err := yaml.Unmarshal(b, &wf); err != nil {
		t.Fatal(err)
	}
	return wf
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func stepIndex(steps []step, name string) int {
	for i, step := range steps {
		if step.Name == name {
			return i
		}
	}
	return -1
}
