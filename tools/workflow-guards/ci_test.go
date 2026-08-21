package workflowguards

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

type workflow struct {
	Jobs map[string]job `yaml:"jobs"`
}

type job struct {
	Services map[string]service `yaml:"services"`
	Steps    []step             `yaml:"steps"`
	Needs    []string           `yaml:"needs"`
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

func TestCIE2EJobProvidesWuKongIM(t *testing.T) {
	wf := readWorkflow(t, ".github/workflows/ci.yml")
	testJob, ok := wf.Jobs["e2e-test"]
	if !ok {
		t.Fatal("ci.yml must define the e2e-test job")
	}

	wk, ok := testJob.Services["wukongim"]
	if !ok {
		t.Fatal("CI e2e-test job must provision a wukongim service for IM-channel integration tests")
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
	runIdx := stepIndex(testJob.Steps, "Run E2E shard")
	if waitIdx < 0 {
		t.Fatal("CI e2e-test job must wait for WuKongIM before running Go tests")
	}
	if runIdx < 0 {
		t.Fatal("CI e2e-test job must define the Run E2E shard step")
	}
	if waitIdx > runIdx {
		t.Fatal("Wait for WuKongIM must run before Run E2E shard")
	}
}

func TestCITestJobAggregatesSplitLanes(t *testing.T) {
	wf := readWorkflow(t, ".github/workflows/ci.yml")
	testJob, ok := wf.Jobs["test"]
	if !ok {
		t.Fatal("ci.yml must define the aggregate test job")
	}

	for _, need := range []string{"changes", "unit-test", "e2e-test"} {
		if !contains(testJob.Needs, need) {
			t.Fatalf("aggregate test job needs = %v, want %q", testJob.Needs, need)
		}
	}
	if len(testJob.Services) != 0 {
		t.Fatal("aggregate test job must not provision services; e2e-test owns MySQL/Redis/WuKongIM")
	}
	if stepIndex(testJob.Steps, "Aggregate split test results") < 0 {
		t.Fatal("aggregate test job must define the Aggregate split test results step")
	}
}

func TestCIValidatesManagerPermissionContract(t *testing.T) {
	wf := readWorkflow(t, ".github/workflows/ci.yml")
	contractJob, ok := wf.Jobs["manager-permission-contract"]
	if !ok {
		t.Fatal("ci.yml must define the manager-permission-contract job")
	}
	if !contains(contractJob.Needs, "changes") {
		t.Fatalf("manager permission contract needs = %v, want changes", contractJob.Needs)
	}
	if len(contractJob.Services) != 0 {
		t.Fatal("manager permission contract must remain service-free")
	}
	if stepIndex(contractJob.Steps, "Validate manager permission contract") < 0 {
		t.Fatal("manager permission contract job must run the strict validation step")
	}
}

func TestCIE2EShardRunnerRejectsInvalidShard(t *testing.T) {
	root := repoRoot(t)
	cmd := exec.Command("bash", "ci/run-e2e-shard.sh", "5", "4")
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("ci/run-e2e-shard.sh 5 4 succeeded, want failure; output:\n%s", out)
	}
	if !strings.Contains(string(out), "invalid shard 5/4") {
		t.Fatalf("ci/run-e2e-shard.sh 5 4 output = %q, want invalid shard diagnostic", out)
	}
}

func TestCITestPackagePartitionCoversEveryTestPackageOnce(t *testing.T) {
	root := repoRoot(t)
	all := commandLines(t, root, "go", "list", "-f", "{{if or .TestGoFiles .XTestGoFiles}}{{.ImportPath}}{{end}}", "./...")
	unit := commandLines(t, root, "bash", "ci/list-unit-packages.sh")

	assignments := make(map[string]string, len(all))
	for _, pkg := range unit {
		assignments[pkg] = "unit"
	}
	for shard := 1; shard <= 4; shard++ {
		shardPackages := commandLines(t, root, "bash", "ci/list-e2e-shard.sh", strconv.Itoa(shard), "4")
		for _, pkg := range shardPackages {
			if lane, ok := assignments[pkg]; ok {
				t.Fatalf("package %s assigned to both %s and e2e shard %d", pkg, lane, shard)
			}
			assignments[pkg] = "e2e"
		}
	}

	for _, pkg := range all {
		if _, ok := assignments[pkg]; !ok {
			t.Fatalf("test package %s was not assigned to unit or e2e lanes", pkg)
		}
	}
	for pkg, lane := range assignments {
		if !contains(all, pkg) {
			t.Fatalf("%s lane assigned non-test package %s", lane, pkg)
		}
	}
}

func readWorkflow(t *testing.T, rel string) workflow {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repoRoot(t), rel))
	if err != nil {
		t.Fatal(err)
	}
	var wf workflow
	if err := yaml.Unmarshal(b, &wf); err != nil {
		t.Fatal(err)
	}
	return wf
}

func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "../.."))
}

func commandLines(t *testing.T, dir string, name string, args ...string) []string {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("%s %s failed: %v\nstdout:\n%s\nstderr:\n%s", name, strings.Join(args, " "), err, out, stderr.String())
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	filtered := lines[:0]
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line != "" {
			filtered = append(filtered, line)
		}
	}
	return filtered
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
