---
name: octospec-workflow
description: >-
  Use when implementing a feature, fixing a bug, or making any non-trivial code
  change in this repository. Drives the octospec 4-phase engineering flow
  (Plan, Implement, Verify, Finish) so the change follows this repo's rules in
  .octospec/ and ships a PR with a linked spec. Triggers on requests like
  "add ...", "implement ...", "fix ...", "refactor ...", "change the ... API",
  写功能, 修 bug, 加接口, 改逻辑. Skip for trivial edits (typo, docs, lint,
  pure config) — those do not need the flow.
---

# octospec workflow

This repository uses the **octospec** engineering standard. When you are asked to
make a non-trivial code change here, run the 4-phase flow instead of editing code
directly. Rules live in `.octospec/` and are the source of truth for this repo's
conventions.

## When to run this

Run the flow for: a new feature, a bug fix, a refactor, an API change, or any
change that touches load-bearing behavior.

**Do NOT run the flow** for trivial changes: a typo, a docs-only edit, a
lint-only fix, a pure config or dependency bump. Just make those directly.

## The 4 phases

Run them in order. Each phase maps to a slash command the user can also invoke
manually; as a skill you perform the same steps.

### 1. Plan
- Choose a short kebab-case `<slug>`.
- Read `.octospec/tasks/_brief.template.md`.
- Inspect the relevant existing code, then write
  `.octospec/tasks/<slug>/brief.md` with: Goal, Load-bearing list (use the same
  tags as `.octospec/rules/_index.yaml` `inject_when.touches` where they apply),
  Out of scope, Acceptance.
- **Show the brief and get confirmation before writing code.**

### 2. Implement
- Resolve applicable rules: read `.octospec/rules/_index.yaml` and
  `.octospec/_global/` (if synced). A rule applies when its `inject_when.paths`
  glob matches a file you will touch, OR its `inject_when.touches` tag is in the
  brief's load-bearing list. A repo-tier rule overrides a global one with the
  same id.
- **Read the full text of each matching rule and follow it.** Prioritize
  `load_bearing: true` rules.
- Record what you used in `.octospec/tasks/<slug>/context.yaml`.
- Write the code. Do not commit yet.

### 3. Verify
- Review the diff against each injected rule (trace load-bearing paths, not just
  the happy path).
- Confirm the diff meets the brief's Acceptance and did not touch anything in
  Out of scope.
- Run this repo's gates (lint / type-check / tests; see CLAUDE.md / AGENTS.md).
- Self-fix what you can.

### 4. Finish
- Run the final gate once more.
- Write `.octospec/journal/shared/<slug>.md` (what was done + any learning).
  Start it with OKF frontmatter (`type: Journal` + title/description/tags/
  timestamp) and add a dated entry to `.octospec/log.md`.
- If a learning is worth reusing, stage it in `.octospec/learnings/pending/`
  (promotion into `rules/` is a separate reviewed PR — do not auto-edit rules).
- Open a PR. Fill the PR template's **Linked Spec** (→ the brief) and the
  **COMPREHENSION** three questions to substance, for load-bearing /
  architectural / P0 changes.

## Notes

- This skill orchestrates the flow automatically. The user can still drive any
  single phase manually with `/octospec-plan`, `/octospec-go`, `/octospec-check`,
  `/octospec-finish`.
- The flow is guidance, not a hard gate; the repo's PR/CI checks are the
  enforcement layer.
