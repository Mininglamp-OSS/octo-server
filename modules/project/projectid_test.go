package project

import (
	"regexp"
	"strings"
	"testing"
)

// canonicalUUID is the shape the Loop integration contract requires: 36
// characters, lowercase hex, hyphenated 8-4-4-4-12, version 4, RFC 4122 variant.
//
// Spelled out rather than delegated to uuid.Parse on purpose. uuid.Parse ACCEPTS
// the unhyphenated 32-character form, so a test written with it would pass
// against exactly the value this change exists to stop producing.
var canonicalUUID = regexp.MustCompile(
	`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestNewProjectIDIsCanonicalHyphenatedUUID(t *testing.T) {
	for i := 0; i < 64; i++ {
		id, err := newProjectID()
		if err != nil {
			t.Fatalf("newProjectID: %v", err)
		}
		if len(id) != 36 {
			t.Fatalf("want 36 characters, got %d (%q)", len(id), id)
		}
		if !canonicalUUID.MatchString(id) {
			t.Fatalf("not a canonical v4 UUID: %q", id)
		}
		if strings.ToLower(id) != id {
			t.Fatalf("must be lowercase: %q", id)
		}
	}
}

// TestNewProjectIDRejectsTheUnhyphenatedShape is the regression this file exists
// for: the previous generator returned a 32-character hyphen-free string, which
// the peer system accepts on input but renders back canonically, so the creation
// handshake's string comparison of project_id against the returned workspace id
// could never succeed.
func TestNewProjectIDRejectsTheUnhyphenatedShape(t *testing.T) {
	id, err := newProjectID()
	if err != nil {
		t.Fatalf("newProjectID: %v", err)
	}
	if !strings.Contains(id, "-") {
		t.Fatal("project_id must carry hyphens; the peer system compares this value as a string")
	}
	if strings.Count(id, "-") != 4 {
		t.Fatalf("want 4 hyphens, got %d in %q", strings.Count(id, "-"), id)
	}
}

func TestNewProjectIDIsUnique(t *testing.T) {
	seen := make(map[string]bool, 512)
	for i := 0; i < 512; i++ {
		id, err := newProjectID()
		if err != nil {
			t.Fatalf("newProjectID: %v", err)
		}
		if seen[id] {
			t.Fatalf("duplicate project_id generated: %q", id)
		}
		seen[id] = true
	}
}

// TestProjectIDFitsTheColumn pins the storage assumption. octo_project.project_id
// is VARCHAR(40); the canonical form is 36, so it fits without a migration and
// the pre-existing 32-character rows keep working alongside it.
func TestProjectIDFitsTheColumn(t *testing.T) {
	const columnWidth = 40
	id, err := newProjectID()
	if err != nil {
		t.Fatalf("newProjectID: %v", err)
	}
	if len(id) > columnWidth {
		t.Fatalf("project_id %q (%d chars) does not fit VARCHAR(%d)", id, len(id), columnWidth)
	}
}

// TestCreateUsesTheCanonicalGenerator is a source guard: the create path must
// mint ids through newProjectID, not through util.GenerUUID. A future edit that
// reintroduced the old generator would produce ids the peer handshake silently
// rejects, and no unit test of newProjectID alone would notice.
func TestCreateUsesTheCanonicalGenerator(t *testing.T) {
	src := readStripped(t, "service.go")
	if strings.Contains(src, "util.GenerUUID()") {
		t.Error("modules/project/service.go must mint project_id via newProjectID(); " +
			"util.GenerUUID() returns the unhyphenated form the Loop handshake cannot match")
	}
	if !strings.Contains(src, "newProjectID()") {
		t.Error("service.go no longer calls newProjectID(); if creation moved, point this guard at the new site")
	}
}
