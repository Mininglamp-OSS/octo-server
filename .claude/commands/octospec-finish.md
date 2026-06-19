---
description: octospec Finish — final check, record journal + learnings, open a PR
argument-hint: <slug>
---

You are running the octospec **Finish** phase for task `$ARGUMENTS`.

1. Run the final verification gate one more time (lint / type-check / tests).
2. Write `.octospec/journal/shared/$ARGUMENTS.md` — a short, team-visible record:
   what was done, any structural learning, any gotcha worth remembering. Start
   the file with OKF frontmatter (`type: Journal`, plus `title`/`description`/
   `tags`/`timestamp`) so it stays a valid OKF unit and passes `octospec-lint`.
   Also add a dated entry to `.octospec/log.md` (create it if missing).
3. If a learning is worth promoting into a reusable rule, drop a candidate in
   `.octospec/learnings/pending/$ARGUMENTS.md`. Promotion into `rules/` is a
   separate, reviewed PR — do not auto-edit `rules/`.
4. Open a PR. Pre-fill the body using `.github/PULL_REQUEST_TEMPLATE.md`:
   - **Linked Spec** → `.octospec/tasks/$ARGUMENTS/brief.md`
   - **COMPREHENSION** three questions answered to substance (for load-bearing /
     architectural / P0 changes; omit for trivial ones).
5. Commit with a Conventional Commit message referencing the issue.
