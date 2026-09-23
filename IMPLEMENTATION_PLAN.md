## Stage 1: Review inventory
**Goal**: Reconcile all review findings raised against `dc3255a` with the current diff.
**Success Criteria**: Findings are grouped by blocking impact and mapped to code, tests, or documentation.
**Tests**: Review/source inspection.
**Status**: Complete

## Stage 2: Correctness and compatibility
**Goal**: Fix Grant lifecycle, timestamp, migration, and legacy read-model issues without changing authorization semantics.
**Success Criteria**: Fresh creation, reactivation, expiry, audit timestamps, and legacy reads expose consistent state.
**Tests**: Focused SQL mocks, expiry tests, and build.
**Status**: Complete

## Stage 3: Test seams and HTTP coverage
**Goal**: Route generic management through the OBO store seam and cover the new Project/management HTTP paths.
**Success Criteria**: Handler tests can inject a fake store and verify owner scoping and OBO request handling.
**Tests**: httptest cases and existing package tests.
**Status**: Complete

## Stage 4: Review handoff
**Goal**: Run focused tests, compile, inspect the diff, and report environment-only failures separately.
**Success Criteria**: No formatting or build errors; remaining non-blocking follow-ups are documented.
**Tests**: `go test`, `go build`, `git diff --check`.
**Status**: Complete
