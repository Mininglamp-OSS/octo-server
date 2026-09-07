package space

// MemberRemovalCleanupStepNames returns a snapshot of the registered
// member-removal cleanup step names.
//
// Exported for downstream modules' tests: the registry is the reverse-registration
// extension point (RegisterMemberRemovalCleanupStep), and a downstream module whose
// whole asynchronous guarantee depends on its step BEING registered needs a way to
// assert that. Before this accessor existed, deleting a module's
// registerSpaceMemberRemovalCleanup call left every downstream suite green — the step
// was simply never in the registry and nothing checked.
//
// Latest-wins semantics apply: a name re-registered keeps one entry.
func MemberRemovalCleanupStepNames() []string {
	cleanupStepsMu.RLock()
	defer cleanupStepsMu.RUnlock()
	names := make([]string, 0, len(cleanupSteps))
	for _, s := range cleanupSteps {
		names = append(names, s.name)
	}
	return names
}
