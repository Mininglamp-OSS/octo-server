---
description: octospec Verify — check the diff against injected rules + lint/test, self-fix
argument-hint: <slug>
---

You are running the octospec **Verify** phase for task `$ARGUMENTS`.

1. Read `.octospec/tasks/$ARGUMENTS/brief.md` and `context.yaml` (the rules that
   were injected).
2. Review the working diff against each injected rule. For every
   `load_bearing: true` rule, confirm the diff actually complies — not just the
   happy path.
3. Check the diff satisfies the brief's **Acceptance** and that nothing in the
   **Out of scope** section was touched.
4. Run this repo's gates (see CLAUDE.md / AGENTS.md): lint, type-check, tests.
5. Self-fix what you can. Report anything you cannot fix as a clear list.
6. This phase is read-and-fix only; do not open a PR here.
