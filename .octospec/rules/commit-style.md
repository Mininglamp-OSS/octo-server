---
type: Rule
title: Commit & PR style (repo)
description: Repo-specific commit and PR conventions inheriting the global commit rule.
tags: ["commit", "git"]
timestamp: 2026-06-19T00:00:00Z
# --- octospec extension fields (OKF-permitted; consumers must preserve) ---
id: commit-style
tier: repo
priority: 60
load_bearing: false
inject_when:
  paths: ["**"]
  touches: ["commit", "git"]
source: octo-spec@1.1.0
supersedes: []
---

# Commit & PR style (repo)

Inherits the global `commit` and `pr` rules. Repo specifics:

- Commit messages: **English**, Conventional Commits (`feat:`, `fix:`, `test:`,
  `refactor:`, `chore:`, `docs:`).
- PR description: **English**.
- Link the issue in the PR (`Fixes #123`) and in the commit footer.
- Read `CONTRIBUTING.md` before opening a PR.
