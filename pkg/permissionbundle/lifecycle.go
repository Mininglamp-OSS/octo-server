package permissionbundle

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"strconv"
)

// Releases is supplied by the shared, immutable, supported catalog integration.
// A candidate inventory is not a supported release. No default/latest pin exists.
type Releases interface {
	Load(context.Context, CatalogPin) (Release, error)
	// Compatible establishes the same reviewed policy/capability semantics for
	// both artifacts, independently of any retained rule comparison.
	Compatible(context.Context, CatalogPin, CatalogPin) error
	// SameIntent must establish explicit old/target semantic equivalence for an
	// unchanged deny during migration. Matching textual IDs alone is insufficient.
	SameIntent(context.Context, CatalogPin, CatalogPin, Rule) (bool, error)
}
type Release struct {
	Pin          CatalogPin
	Bytes        []byte
	Capabilities map[string]bool
}

func loadRelease(ctx context.Context, releases Releases, pin CatalogPin) (Release, error) {
	if releases == nil {
		return Release{}, ErrUnavailable
	}
	r, e := releases.Load(ctx, pin)
	// The provider validates the final manifest and support status, including
	// its defined digest material (which is not necessarily the raw file hash).
	if e != nil || r.Pin != pin || len(r.Bytes) == 0 || len(r.Bytes) > 8388608 || r.Capabilities == nil {
		return Release{}, ErrUnavailable
	}
	return r, nil
}

// Apply implements only storage lifecycle and catalog membership validation.
// It does not decide whether an invocation or action is allowed.
func Apply(ctx context.Context, p Bundle, m Mutation, releases Releases) (Bundle, error) {
	if m.SchemaVersion != 1 {
		return Bundle{}, ErrInvalid
	}
	// Every stage shares the same exact expected tuple. The candidate is
	// structurally revalidated below before reference checks or publication.
	if m.Expected != p.Expected() {
		return Bundle{}, ErrConflict
	}
	r, e := loadRelease(ctx, releases, p.Pin())
	if e != nil {
		return Bundle{}, e
	}
	if !catalogMembers(p, r) {
		return Bundle{}, ErrUnavailable
	}
	stored, marshalErr := json.Marshal(p)
	if marshalErr != nil {
		return Bundle{}, ErrUnavailable
	}
	next, e := ParseBundle(bytes.NewReader(stored))
	if e != nil {
		return Bundle{}, ErrUnavailable
	}
	rev, e := U64(p.Revision)
	if e != nil || rev == math.MaxUint64 {
		return Bundle{}, ErrUnavailable
	}
	epoch, e := U64(p.ContextEpoch)
	if e != nil {
		return Bundle{}, ErrUnavailable
	}
	switch m.Action {
	case "mutate":
		if len(m.Changes) < 1 || len(m.Changes) > 2 || m.TargetCatalog != nil || m.CapabilityRules != nil {
			return Bundle{}, ErrInvalid
		}
		seen := map[string]bool{}
		for _, c := range m.Changes {
			if seen[c.Stage] {
				return Bundle{}, ErrInvalid
			}
			seen[c.Stage] = true
			var stage *Stage
			switch c.Stage {
			case "invocation":
				stage = &next.Invocation
			case "capability":
				stage = &next.Capability
			default:
				return Bundle{}, ErrInvalid
			}
			before := stage.State
			switch c.Action {
			case "activate", "reactivate", "replace_rules":
				if c.Rules == nil || c.RuleIDs != nil || c.Action == "replace_rules" && before != "E" || c.Action != "replace_rules" && before != "U" {
					return Bundle{}, ErrInvalid
				}
				stage.State = "E"
				stage.Rules = c.Rules
			case "delete_rules":
				if before != "E" || c.Rules != nil || len(c.RuleIDs) == 0 {
					return Bundle{}, ErrInvalid
				}
				ids := map[string]bool{}
				for _, id := range c.RuleIDs {
					if ids[id] {
						return Bundle{}, ErrInvalid
					}
					ids[id] = true
				}
				kept := []Rule{}
				for _, rule := range stage.Rules {
					id := rule["rule_id"].(string)
					if ids[id] {
						delete(ids, id)
					} else {
						kept = append(kept, rule)
					}
				}
				if len(ids) != 0 {
					return Bundle{}, ErrInvalid
				}
				stage.Rules = kept
			case "reset", "deactivate":
				if c.Rules != nil || c.RuleIDs != nil {
					return Bundle{}, ErrInvalid
				}
				stage.State = "E"
				if c.Action == "deactivate" {
					stage.State = "U"
				}
				stage.Rules = []Rule{}
			default:
				return Bundle{}, ErrInvalid
			}
			if c.Stage == "invocation" && before == "U" && stage.State == "E" {
				if epoch == math.MaxUint64 {
					return Bundle{}, ErrUnavailable
				}
				epoch++
			}
		}
	case "migrate_catalog":
		if len(m.Changes) != 0 || m.TargetCatalog == nil || m.CapabilityRules == nil || next.Capability.State == "U" && len(m.CapabilityRules) != 0 {
			return Bundle{}, ErrInvalid
		}
		r, e = loadRelease(ctx, releases, *m.TargetCatalog)
		if e != nil {
			return Bundle{}, e
		}
		if releases.Compatible(ctx, p.Pin(), *m.TargetCatalog) != nil {
			return Bundle{}, ErrUnavailable
		}
		next.CatalogVersion = r.Pin.Version
		next.CatalogDigest = r.Pin.Digest
		next.Capability.Rules = m.CapabilityRules
	default:
		return Bundle{}, ErrInvalid
	}
	next.Revision = strconv.FormatUint(rev+1, 10)
	next.ContextEpoch = strconv.FormatUint(epoch, 10)
	next, _, e = Seal(next)
	if e == nil && !catalogMembers(next, r) {
		return Bundle{}, ErrInvalid
	}
	return next, e
}

func catalogMembers(p Bundle, r Release) bool {
	for _, rule := range p.Capability.Rules {
		caps, ok := rule["capabilities"].([]any)
		if !ok {
			return false
		}
		for _, v := range caps {
			id, ok := v.(string)
			if !ok || !r.Capabilities[id] {
				return false
			}
		}
	}
	return true
}

// ReferencesToValidate returns newly supplied intent, omitting ONLY exact stored
// denies retained under choice B. The caller must still guard current scope,
// anchor and expected tuple in the same transaction through commit.
func ReferencesToValidate(ctx context.Context, old, next Bundle, m Mutation, releases Releases) ([]Rule, error) {
	if old.Key != next.Key || old.Incarnation != next.Incarnation || m.Expected != old.Expected() {
		return nil, ErrConflict
	}
	var out []Rule
	for _, name := range []string{"invocation", "capability"} {
		before, after := old.Invocation, next.Invocation
		if name == "capability" {
			before, after = old.Capability, next.Capability
		}
		supplied := m.Action == "migrate_catalog" && name == "capability"
		for _, c := range m.Changes {
			if c.Stage == name && (c.Action == "activate" || c.Action == "reactivate" || c.Action == "replace_rules") {
				supplied = true
			}
		}
		if !supplied {
			continue
		}
		for _, rule := range after.Rules {
			retained := false
			if rule["effect"] == "deny" && before.State == after.State {
				for _, prior := range before.Rules {
					if !bytes.Equal(canonical(map[string]any(prior)), canonical(map[string]any(rule))) {
						continue
					}
					retained = true
					if old.Pin() != next.Pin() {
						var e error
						retained, e = releases.SameIntent(ctx, old.Pin(), next.Pin(), rule)
						if e != nil {
							return nil, ErrUnavailable
						}
					}
					break
				}
			}
			if !retained {
				out = append(out, rule)
			}
		}
	}
	return out, nil
}
