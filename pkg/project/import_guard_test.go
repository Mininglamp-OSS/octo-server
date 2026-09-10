package project_test

import (
	"os/exec"
	"strings"
	"testing"
)

// Import-direction guards for the Project layer.
//
// The whole reason this package exists is that modules/group needs a PREDICATE
// about project membership and cannot import modules/project without creating a
// cycle. That only holds while the directions below hold, and both are easy to
// break by adding one convenient import — so they are asserted rather than
// documented.
//
// In an EXTERNAL test package (project_test) on purpose: an in-package test that
// imported something heavy would itself become part of what it is measuring.

func deps(t *testing.T, pkg string) string {
	t.Helper()
	out, err := exec.Command("go", "list", "-deps", pkg).Output()
	if err != nil {
		t.Fatalf("go list -deps %s: %v", pkg, err)
	}
	return string(out)
}

// TestPkgProjectDoesNotImportModulesProject.
//
// If this package imported modules/project, modules/group — which imports this
// one for the admission gate — would transitively import modules/project too.
// modules/project reverse-registers its cascade INTO modules/group, so that
// closes the cycle the reverse registration exists to avoid, and the failure
// surfaces as an unrelated-looking build error in whichever package happens to
// be compiled first.
func TestPkgProjectDoesNotImportModulesProject(t *testing.T) {
	list := deps(t, "github.com/Mininglamp-OSS/octo-server/pkg/project")
	if strings.Contains(list, "octo-server/modules/project") {
		t.Fatal("pkg/project must not import modules/project: modules/group imports " +
			"pkg/project, and modules/project reverse-registers into modules/group, so " +
			"this import closes the cycle the reverse registration exists to avoid")
	}
	// It must also stay free of modules/group, for the mirror-image reason.
	if strings.Contains(list, "octo-server/modules/group") {
		t.Fatal("pkg/project must not import modules/group")
	}
}

// TestModulesProjectDoesNotImportModulesGroup.
//
// The cascade runs group-ward, and it is reverse-registered precisely so this
// direction stays at zero: modules/group already imports modules/space, and
// modules/project imports modules/space, so a modules/project -> modules/group
// edge would make the dependency graph depend on which module happened to
// register first.
func TestModulesProjectDoesNotImportModulesGroup(t *testing.T) {
	list := deps(t, "github.com/Mininglamp-OSS/octo-server/modules/project")
	if strings.Contains(list, "octo-server/modules/group") {
		t.Fatal("modules/project must not import modules/group: the cascade is " +
			"reverse-registered (modules/group registers its detach step into " +
			"modules/project) for exactly this reason")
	}
}
