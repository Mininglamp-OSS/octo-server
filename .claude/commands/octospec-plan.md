---
description: Draft an octospec task brief (goal / load-bearing list / out-of-scope / acceptance)
argument-hint: <task description>
---

You are running the octospec **Plan** phase for this repo.

Task: $ARGUMENTS

1. Pick a short kebab-case `<slug>` for this task.
2. Read `.octospec/tasks/_brief.template.md` for the required shape.
3. Inspect the relevant existing code to ground the brief in reality (you may
   draft the brief from the code; the human will confirm it).
4. Write `.octospec/tasks/<slug>/brief.md` filling every section (including the
   OKF frontmatter from the template — set `type: Task`, `title`, `description`,
   `tags`, `timestamp`):
   - **Goal** — what behavior changes and why.
   - **Load-bearing list** — existing behaviors/contracts this touches. Use the
     same tags as `.octospec/rules/_index.yaml` `inject_when.touches` where they
     apply, so the right rules get injected later.
   - **Out of scope** — what this deliberately does NOT touch.
   - **Acceptance** — machine-checkable where possible.
5. Do NOT write implementation code in this phase. Stop after the brief and show
   it for confirmation.
