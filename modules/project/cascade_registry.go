package project

import (
	"sync"

	"github.com/Mininglamp-OSS/octo-lib/config"
)

// Reverse-registered disband steps keep group detachment in its owning module.

// ProjectDisband describes a project being disbanded, whether by a human
// request or by a future automated lifecycle worker.
type ProjectDisband struct {
	ProjectID string
	SpaceID   string
	// ByCascade is true when automation disbanded the project, rather than a
	// human choosing to. The current Space-removal cascade preserves the Owner
	// row and does not use this branch.
	ByCascade bool
}

// DisbandStep reverts whatever the registering module attached to the project.
type DisbandStep func(ctx *config.Context, disband ProjectDisband) error

type namedDisbandStep struct {
	name string
	fn   DisbandStep
}

var (
	cascadeMu           sync.RWMutex
	projectDisbandSteps []namedDisbandStep
)

// RegisterProjectDisbandStep registers work to run when a project is disbanded.
func RegisterProjectDisbandStep(name string, fn DisbandStep) {
	if name == "" || fn == nil {
		return
	}
	cascadeMu.Lock()
	defer cascadeMu.Unlock()
	for i := range projectDisbandSteps {
		if projectDisbandSteps[i].name == name {
			projectDisbandSteps[i].fn = fn
			return
		}
	}
	projectDisbandSteps = append(projectDisbandSteps, namedDisbandStep{name: name, fn: fn})
}

func snapshotDisbandSteps() []namedDisbandStep {
	cascadeMu.RLock()
	defer cascadeMu.RUnlock()
	out := make([]namedDisbandStep, len(projectDisbandSteps))
	copy(out, projectDisbandSteps)
	return out
}
