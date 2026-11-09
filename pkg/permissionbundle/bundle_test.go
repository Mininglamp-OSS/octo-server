package permissionbundle

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/gocraft/dbr/v2"
	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/require"
)

var fixtureKey = Key{"https://synthetic.invalid", "bot", "space"}
var fixturePin = CatalogPin{"synthetic-v1", strings.Repeat("a", 64)}

type fixtureReleases struct{ equivalent bool }

func (f fixtureReleases) Load(_ context.Context, p CatalogPin) (Release, error) {
	if p.Version != "synthetic-v1" && p.Version != "synthetic-v2" {
		return Release{}, ErrUnavailable
	}
	return Release{p, []byte(`{"fixture":"synthetic-only-not-a-release"}`), map[string]bool{"op:group.get": true, "op:group.set": true}}, nil
}
func (f fixtureReleases) SameIntent(context.Context, CatalogPin, CatalogPin, Rule) (bool, error) {
	return f.equivalent, nil
}
func (f fixtureReleases) Compatible(context.Context, CatalogPin, CatalogPin) error { return nil }
func fixtureBundle(t *testing.T) Bundle {
	t.Helper()
	p, _, e := Seal(Bundle{1, fixtureKey, strings.Repeat("1", 32), "0", "0", fixturePin.Version, fixturePin.Digest, "", Stage{"U", []Rule{}}, Stage{"U", []Rule{}}})
	require.NoError(t, e)
	return p
}
func fixtureRule(effect string) Rule {
	return Rule{"rule_id": "r1", "effect": effect, "caller": map[string]any{"kind": "uids", "uids": []any{"peer"}}, "source": map[string]any{"kind": "refs", "refs": []any{map[string]any{"kind": "group", "id": "group", "include_descendants": false}}}}
}
func request(t *testing.T, p Bundle, changes string) Mutation {
	t.Helper()
	b, _ := json.Marshal(p.Expected())
	m, e := ParseMutation(strings.NewReader(`{"schema_version":1,"action":"mutate","expected":` + string(b) + `,"changes":` + changes + `}`))
	require.NoError(t, e)
	return m
}
func mutationBytes(t *testing.T, p Bundle, changes string) []byte {
	t.Helper()
	b, _ := json.Marshal(p.Expected())
	return []byte(`{"schema_version":1,"action":"mutate","expected":` + string(b) + `,"changes":` + changes + `}`)
}

func TestStrictLexicalAndClosedMutations(t *testing.T) {
	p := fixtureBundle(t)
	valid := string(mutationBytes(t, p, `[{"stage":"invocation","action":"activate","rules":[]}]`))
	for name, b := range map[string]string{
		"float version":       strings.Replace(valid, `"schema_version":1`, `"schema_version":1.0`, 1),
		"exponent version":    strings.Replace(valid, `"schema_version":1`, `"schema_version":1e0`, 1),
		"escaped duplicate":   strings.Replace(valid, `"schema_version":1`, `"schema_version":1,"schema_versio\u006e":1`, 1),
		"unknown":             strings.Replace(valid, `"action":"mutate"`, `"action":"mutate","extra":false`, 1),
		"null rules":          strings.Replace(valid, `"rules":[]`, `"rules":null`, 1),
		"trailing":            valid + ` {}`,
		"surrogate":           strings.Replace(valid, `"stage":"invocation"`, `"stage":"\ud800"`, 1),
		"invalid utf8":        valid + string([]byte{0xff}),
		"overflow":            strings.Replace(valid, `"revision":"0"`, `"revision":"18446744073709551616"`, 1),
		"leading zero":        strings.Replace(valid, `"revision":"0"`, `"revision":"00"`, 1),
		"incompatible action": strings.Replace(valid, `"action":"activate"`, `"action":"reset"`, 1),
	} {
		t.Run(name, func(t *testing.T) { _, e := ParseMutation(strings.NewReader(b)); require.ErrorIs(t, e, ErrInvalid) })
	}
	_, e := ParseMutation(strings.NewReader(valid))
	require.NoError(t, e)
	_, e = decodeStrict(strings.NewReader(strings.Repeat("[", 13)+"0"+strings.Repeat("]", 13)), 100)
	require.Error(t, e)
	_, e = ParseMutation(bytes.NewReader(bytes.Repeat([]byte{' '}, MaxBundleBytes+1)))
	require.Error(t, e)
}
func TestCanonicalBytesDefaultsAndDigest(t *testing.T) {
	ref := map[string]any{"id": "G1", "include_descendants": false, "kind": "group"}
	require.Equal(t, "fdf32590c58e4dd5c85f8c602fff10422d85afb5c0c9563cb4dd488715a72f14", hash(canonical(ref)))
	require.Equal(t, `{"s":"<>&/`+"\u2028\u2029"+`"}`, string(canonical(map[string]any{"s": "<>&/\u2028\u2029"})))
	a := []any{"\U00010000", "\ue000"}
	require.NoError(t, sortStrings(a))
	require.Equal(t, []any{"\ue000", "\U00010000"}, a)
	p := fixtureBundle(t)
	m := request(t, p, `[{"stage":"invocation","action":"activate","rules":[{"rule_id":"r","effect":"allow","caller":{"kind":"all_humans"},"source":{"kind":"refs","refs":[{"kind":"group","id":"G1"}]}}]}]`)
	next, e := Apply(context.Background(), p, m, fixtureReleases{})
	require.NoError(t, e)
	require.Equal(t, false, next.Invocation.Rules[0]["source"].(map[string]any)["refs"].([]any)[0].(map[string]any)["include_descendants"])
	_, body, e := Seal(next)
	require.NoError(t, e)
	read, e := ParseBundle(bytes.NewReader(body))
	require.NoError(t, e)
	require.Equal(t, next, read)
	_, e = ParseBundle(bytes.NewReader(bytes.Replace(body, []byte(`"revision":"1"`), []byte(`"revision":"2"`), 1)))
	require.ErrorIs(t, e, ErrUnavailable)
	require.True(t, ValidNativeID(strings.Repeat("😀", 40)))
	require.False(t, ValidNativeID(strings.Repeat("😀", 41)))
	for _, s := range []string{" x", "x\n", "", "https://example.invalid/path"} {
		if strings.HasPrefix(s, "https:") {
			require.False(t, ValidOrigin(s))
		} else {
			require.False(t, ValidNativeID(s))
		}
	}
}
func TestLifecycleSharedCASAndEpoch(t *testing.T) {
	ctx := context.Background()
	p := fixtureBundle(t)
	for i, changes := range []string{
		`[{"stage":"invocation","action":"activate","rules":[]},{"stage":"capability","action":"reset"}]`,
		`[{"stage":"invocation","action":"deactivate"}]`,
		`[{"stage":"invocation","action":"reset"}]`,
		`[{"stage":"capability","action":"reset"}]`,
	} {
		m := request(t, p, changes)
		next, e := Apply(ctx, p, m, fixtureReleases{})
		require.NoError(t, e)
		require.Equal(t, fmt.Sprint(i+1), next.Revision)
		if i >= 2 {
			require.Equal(t, "2", next.ContextEpoch)
		}
		_, e = Apply(ctx, next, m, fixtureReleases{})
		require.ErrorIs(t, e, ErrConflict)
		p = next
	}
	p.Revision = "18446744073709551615"
	m := request(t, p, `[{"stage":"capability","action":"reset"}]`)
	_, e := Apply(ctx, p, m, fixtureReleases{})
	require.ErrorIs(t, e, ErrUnavailable)
}
func TestChoiceBRetainsOnlySameStoredDeny(t *testing.T) {
	ctx := context.Background()
	p := fixtureBundle(t)
	p.Invocation = Stage{"E", []Rule{fixtureRule("deny")}}
	p, _, _ = Seal(p)
	for _, field := range []string{"unchanged", "rule_id", "effect", "caller", "source", "stage"} {
		t.Run(field, func(t *testing.T) {
			next, _, e := Seal(p)
			require.NoError(t, e)
			r := next.Invocation.Rules[0]
			switch field {
			case "rule_id":
				r["rule_id"] = "copied"
			case "effect":
				r["effect"] = "allow"
			case "caller":
				r["caller"] = map[string]any{"kind": "all_humans"}
			case "source":
				r["source"].(map[string]any)["refs"].([]any)[0].(map[string]any)["include_descendants"] = true
			case "stage":
				next.Capability = Stage{"E", []Rule{r}}
				r["capabilities"] = []any{"op:group.get"}
				next.Invocation.Rules = []Rule{}
			}
			m := Mutation{SchemaVersion: 1, Action: "mutate", Expected: p.Expected(), Changes: []Change{{Stage: "invocation", Action: "replace_rules", Rules: next.Invocation.Rules}, {Stage: "capability", Action: "replace_rules", Rules: next.Capability.Rules}}}
			rules, e := ReferencesToValidate(ctx, p, next, m, fixtureReleases{})
			require.NoError(t, e)
			if field == "unchanged" {
				require.Empty(t, rules)
			} else {
				require.Len(t, rules, 1)
			}
		})
	}
	p.Capability = Stage{"E", []Rule{fixtureRule("deny")}}
	p.Capability.Rules[0]["capabilities"] = []any{"op:group.get"}
	p, _, _ = Seal(p)
	m := Mutation{SchemaVersion: 1, Action: "migrate_catalog", Expected: p.Expected(), TargetCatalog: &CatalogPin{"synthetic-v2", strings.Repeat("b", 64)}, CapabilityRules: p.Capability.Rules}
	next, e := Apply(ctx, p, m, fixtureReleases{})
	require.NoError(t, e)
	rules, e := ReferencesToValidate(ctx, p, next, m, fixtureReleases{})
	require.NoError(t, e)
	require.Len(t, rules, 1)
	rules, e = ReferencesToValidate(ctx, p, next, m, fixtureReleases{equivalent: true})
	require.NoError(t, e)
	require.Empty(t, rules)
}

// The authority below is synthetic test scope, not an election/recovery proof.
type fixtureAuthority struct{ ackFails bool }

func (f fixtureAuthority) Begin(context.Context, *dbr.Tx) (AuthorityRead, error) { return f, nil }
func (f fixtureAuthority) Incarnation() string                                   { return strings.Repeat("a", 32) }
func (f fixtureAuthority) Validate(context.Context) error                        { return nil }
func (f fixtureAuthority) Acknowledge(context.Context) error {
	if f.ackFails {
		return ErrUnavailable
	}
	return nil
}

func fixtureStore(t *testing.T, path string) Store {
	t.Helper()
	conn, e := dbr.Open("sqlite3", path+"?_busy_timeout=1000", nil)
	require.NoError(t, e)
	conn.SetMaxOpenConns(1)
	t.Cleanup(func() { conn.Close() })
	return Store{Session: conn.NewSession(nil), Origin: fixtureKey.Origin, Authority: fixtureAuthority{}, Native: SQLNative{testNoLocks: true}, Releases: fixtureReleases{}, testNoLocks: true}
}
func seed(t *testing.T, s Store, pins ...CatalogPin) {
	t.Helper()
	for _, q := range []string{
		`CREATE TABLE user(id INTEGER PRIMARY KEY,uid TEXT UNIQUE,status INTEGER,is_destroy INTEGER,robot INTEGER)`,
		`CREATE TABLE robot(id INTEGER PRIMARY KEY,robot_id TEXT UNIQUE,creator_uid TEXT,status INTEGER)`,
		`CREATE TABLE app_bot(id INTEGER PRIMARY KEY,uid TEXT UNIQUE,status INTEGER)`,
		`CREATE TABLE space(id INTEGER PRIMARY KEY,space_id TEXT UNIQUE,status INTEGER)`,
		`CREATE TABLE space_member(space_id TEXT,uid TEXT,status INTEGER,UNIQUE(space_id,uid))`,
		"CREATE TABLE `group`(group_no TEXT UNIQUE,space_id TEXT,status INTEGER)",
		`CREATE TABLE thread(group_no TEXT,short_id TEXT,status INTEGER,UNIQUE(group_no,short_id))`,
		`CREATE TABLE bot_permission_scope(bot_uid BLOB,space_id BLOB,scope_incarnation BLOB UNIQUE,revision TEXT,context_epoch TEXT,schema_version INTEGER,catalog_version TEXT,catalog_digest BLOB,bundle_json TEXT,bundle_digest BLOB,native_anchor_json TEXT,native_anchor_digest BLOB,retired INTEGER,updated_by TEXT,updated_at DATETIME,PRIMARY KEY(bot_uid,space_id))`,
		`INSERT INTO user VALUES(1,'bot',1,0,1),(2,'owner',1,0,0),(3,'peer',1,1,0),(4,'departed',1,0,0)`,
		`INSERT INTO robot VALUES(1,'bot','owner',1),(2,'peer','owner',0)`,
		`INSERT INTO space VALUES(1,'space',1)`,
		`INSERT INTO space_member VALUES('space','bot',1),('space','owner',1),('space','peer',1),('space','departed',1)`,
		"INSERT INTO `group` VALUES('group','space',1)",
	} {
		_, e := s.Session.Exec(q)
		require.NoError(t, e)
	}
	ctx := context.Background()
	tx, e := s.Session.Begin()
	require.NoError(t, e)
	defer tx.RollbackUnlessCommitted()
	a, e := s.Native.Scope(ctx, tx, fixtureKey, "owner", strings.Repeat("a", 32))
	require.NoError(t, e)
	pin := fixturePin
	if len(pins) > 0 {
		pin = pins[0]
	}
	require.NoError(t, s.ProvisionSuffix(ctx, tx, fixtureKey, a, pin))
	require.NoError(t, tx.Commit())
}

func TestSQLPreparationSaveReadbackRestartAndAuthority(t *testing.T) {
	path := filepath.Join(t.TempDir(), "task-owned.sqlite")
	s := fixtureStore(t, path)
	seed(t, s)
	ctx := context.Background()
	read, e := s.Get(ctx, fixtureKey, "owner")
	require.NoError(t, e)
	require.Equal(t, "0", read.Bundle.Revision)
	prepBytes, _ := json.Marshal(Preparation{fixtureKey, read.Bundle.Expected(), []string{"peer", "departed"}})
	prep, e := s.Prepare(ctx, fixtureKey, "owner", bytes.NewReader(prepBytes))
	require.NoError(t, e)
	require.Len(t, prep.References, 2)
	require.Len(t, prep.References[0], 2)
	rule := fixtureRule("deny")
	rule["source"] = map[string]any{"kind": "refs", "refs": []any{prep.References[0]}}
	rawRule, _ := json.Marshal(rule)
	body := mutationBytes(t, read.Bundle, `[{"stage":"invocation","action":"activate","rules":[`+string(rawRule)+`]}]`)
	saved, e := s.Mutate(ctx, fixtureKey, "owner", bytes.NewReader(body))
	require.NoError(t, e)
	require.Equal(t, "1", saved.Bundle.Revision)
	require.Equal(t, "1", saved.Bundle.ContextEpoch)
	// Preparation confers no lease; whole-batch preparation and a changed allow
	// both fail after removal, with no partial mapping or revision increment.
	_, e = s.Session.Exec(`UPDATE space_member SET status=0 WHERE uid='peer'`)
	require.NoError(t, e)
	_, e = s.Prepare(ctx, fixtureKey, "owner", bytes.NewReader(prepBytes))
	require.Error(t, e)
	rule["effect"] = "allow"
	rawRule, _ = json.Marshal(rule)
	_, e = s.Mutate(ctx, fixtureKey, "owner", bytes.NewReader(mutationBytes(t, saved.Bundle, `[{"stage":"invocation","action":"replace_rules","rules":[`+string(rawRule)+`]}]`)))
	require.ErrorIs(t, e, ErrReference)
	// Exact retained deny survives an unrelated same-stage replacement.
	rule["effect"] = "deny"
	rawRule, _ = json.Marshal(rule)
	saved, e = s.Mutate(ctx, fixtureKey, "owner", bytes.NewReader(mutationBytes(t, saved.Bundle, `[{"stage":"invocation","action":"replace_rules","rules":[`+string(rawRule)+`]}]`)))
	require.NoError(t, e)
	// Reopening the real disposable file retains incarnation, digest and denies.
	reopened := fixtureStore(t, path)
	again, e := reopened.Get(ctx, fixtureKey, "owner")
	require.NoError(t, e)
	require.Equal(t, saved, again)
	_, e = reopened.Get(ctx, fixtureKey, "peer")
	require.ErrorIs(t, e, ErrForbidden)
	other := fixtureKey
	other.BotUID = "other"
	_, e = reopened.Self(ctx, other)
	require.Error(t, e)
	reopened.Authority = nil
	_, e = reopened.Self(ctx, fixtureKey)
	require.ErrorIs(t, e, ErrUnavailable)
	_, e = s.Session.Exec(`UPDATE robot SET creator_uid='departed' WHERE robot_id='bot'`)
	require.NoError(t, e)
	_, e = s.Get(ctx, fixtureKey, "departed")
	require.ErrorIs(t, e, ErrUnavailable)
	require.FileExists(t, path)
	_, e = os.Stat(path)
	require.NoError(t, e)
}
func TestConcurrentCASAndAmbiguousCommit(t *testing.T) {
	s := fixtureStore(t, filepath.Join(t.TempDir(), "race.sqlite"))
	seed(t, s)
	ctx := context.Background()
	read, e := s.Get(ctx, fixtureKey, "owner")
	require.NoError(t, e)
	body := mutationBytes(t, read.Bundle, `[{"stage":"capability","action":"reset"}]`)
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, e := s.Mutate(ctx, fixtureKey, "owner", bytes.NewReader(body))
			results <- e
		}()
	}
	wg.Wait()
	close(results)
	ok, conflicts := 0, 0
	for e := range results {
		if e == nil {
			ok++
		} else if errors.Is(e, ErrConflict) {
			conflicts++
		} else {
			t.Fatal(e)
		}
	}
	require.Equal(t, 1, ok)
	require.Equal(t, 1, conflicts)
	read, e = s.Get(ctx, fixtureKey, "owner")
	require.NoError(t, e)
	s.Authority = fixtureAuthority{ackFails: true}
	_, e = s.Mutate(ctx, fixtureKey, "owner", bytes.NewReader(mutationBytes(t, read.Bundle, `[{"stage":"invocation","action":"reset"}]`)))
	require.ErrorIs(t, e, ErrUnavailable)
	require.ErrorIs(t, e, ErrReconcile)
	s.Authority = fixtureAuthority{}
	actual, e := s.Get(ctx, fixtureKey, "owner")
	require.NoError(t, e)
	require.Equal(t, "2", actual.Bundle.Revision)
	require.Equal(t, "1", actual.Bundle.ContextEpoch)
}

func TestContractFixturesAndWholeBundleLimits(t *testing.T) {
	b, e := os.ReadFile("contracts/synthetic-examples-v1.json")
	require.NoError(t, e)
	var f struct {
		Bundle          json.RawMessage `json:"bundle"`
		Positive        json.RawMessage `json:"positive_mutation"`
		Negative        []string        `json:"negative_mutation_raw"`
		Identity        string          `json:"positive_identity_raw"`
		InvalidIdentity string          `json:"negative_identity_raw"`
	}
	require.NoError(t, json.Unmarshal(b, &f))
	p, e := ParseBundle(bytes.NewReader(f.Bundle))
	require.NoError(t, e)
	m, e := ParseMutation(bytes.NewReader(f.Positive))
	require.NoError(t, e)
	_, e = Apply(t.Context(), p, m, fixtureReleases{})
	require.NoError(t, e)
	for _, raw := range f.Negative {
		_, e := ParseMutation(strings.NewReader(raw))
		require.Error(t, e)
	}
	_, e = ParseManagedIdentity([]byte(f.Identity))
	require.NoError(t, e)
	_, e = ParseManagedIdentity([]byte(f.InvalidIdentity))
	require.Error(t, e)
	var handoff struct {
		Hashes map[string]string `json:"immutable_source_contracts"`
	}
	b, e = os.ReadFile("contracts/handoff-v1.json")
	require.NoError(t, e)
	require.NoError(t, json.Unmarshal(b, &handoff))
	for name, digest := range handoff.Hashes {
		b, e := os.ReadFile(filepath.Join("contracts", name))
		require.NoError(t, e)
		require.Equal(t, digest, hash(b), name)
	}
	// Post-default duplicates differ structurally in the supplied JSON.
	raw := string(mutationBytes(t, p, `[{"stage":"invocation","action":"activate","rules":[{"rule_id":"r","effect":"allow","caller":{"kind":"all_humans"},"source":{"kind":"refs","refs":[{"kind":"group","id":"g"},{"kind":"group","id":"g","include_descendants":false}]}}]}]`))
	_, e = ParseMutation(strings.NewReader(raw))
	require.ErrorIs(t, e, ErrInvalid)
	p.Invocation = Stage{"E", []Rule{}}
	for i := 0; i < 65; i++ {
		r := fixtureRule("allow")
		r["rule_id"] = fmt.Sprintf("r%d", i)
		ids := []any{}
		for j := 0; j < 64; j++ {
			ids = append(ids, fmt.Sprintf("uid%d", j))
		}
		r["caller"].(map[string]any)["uids"] = ids
		p.Invocation.Rules = append(p.Invocation.Rules, r)
	}
	_, _, e = Seal(p)
	require.ErrorIs(t, e, ErrInvalid)
}

func TestErasedGrantsNeverRevive(t *testing.T) {
	p := fixtureBundle(t)
	r, _ := json.Marshal(fixtureRule("allow"))
	m := request(t, p, `[{"stage":"invocation","action":"activate","rules":[`+string(r)+`]}]`)
	p, e := Apply(t.Context(), p, m, fixtureReleases{})
	require.NoError(t, e)
	m = request(t, p, `[{"stage":"invocation","action":"deactivate"}]`)
	p, e = Apply(t.Context(), p, m, fixtureReleases{})
	require.NoError(t, e)
	require.Empty(t, p.Invocation.Rules)
	m = request(t, p, `[{"stage":"invocation","action":"reactivate","rules":[]}]`)
	p, e = Apply(t.Context(), p, m, fixtureReleases{})
	require.NoError(t, e)
	require.Equal(t, "E", p.Invocation.State)
	require.Empty(t, p.Invocation.Rules)
	require.Equal(t, "2", p.ContextEpoch)
}

func TestPreparationExactFieldNamesAndStoredCatalogMembership(t *testing.T) {
	p := fixtureBundle(t)
	raw, e := json.Marshal(Preparation{fixtureKey, p.Expected(), []string{"peer"}})
	require.NoError(t, e)
	_, e = ParsePreparation(bytes.NewReader(raw))
	require.NoError(t, e)
	for _, changed := range []string{strings.Replace(string(raw), `"key"`, `"KEY"`, 1), strings.Replace(string(raw), `"bot_uid"`, `"BOT_UID"`, 1), strings.Replace(string(raw), `"peer_uids":["peer"]`, `"peer_uids":null`, 1)} {
		_, e = ParsePreparation(strings.NewReader(changed))
		require.ErrorIs(t, e, ErrInvalid)
	}
	s := fixtureStore(t, filepath.Join(t.TempDir(), "corrupt.sqlite"))
	seed(t, s)
	stored, e := s.Get(t.Context(), fixtureKey, "owner")
	require.NoError(t, e)
	corrupt := stored.Bundle
	rule := fixtureRule("allow")
	rule["capabilities"] = []any{"not-in-supported-release"}
	corrupt.Capability = Stage{"E", []Rule{rule}}
	corrupt, body, e := Seal(corrupt)
	require.NoError(t, e)
	digest, e := hex.DecodeString(corrupt.Digest)
	require.NoError(t, e)
	_, e = s.Session.Exec(`UPDATE bot_permission_scope SET bundle_json=?,bundle_digest=?`, string(body), digest)
	require.NoError(t, e)
	_, e = s.Get(t.Context(), fixtureKey, "owner")
	require.ErrorIs(t, e, ErrUnavailable)
}
