---
description: octospec Implement — inject matching rules, then write code (no commit)
argument-hint: <slug>
---

You are running the octospec **Implement** phase for task `$ARGUMENTS`.

1. Read `.octospec/tasks/$ARGUMENTS/brief.md`.
2. Resolve which rules apply (this is the rule injection step):
   - Read `.octospec/rules/_index.yaml` and `.octospec/_global/` (if synced).
   - A rule applies when its `inject_when.paths` glob matches a file you will
     touch, OR its `inject_when.touches` tag is in the brief's load-bearing list.
   - A repo-tier rule with the same `id` overrides the global one.
   - **Actually read and follow the full text** of each matching rule file before
     writing code. Prioritize `load_bearing: true` rules.
3. Record what you injected in `.octospec/tasks/$ARGUMENTS/context.yaml`:
   ```yaml
   injected:
     - id: <rule-id>
       source: repo|global
   brief: .octospec/tasks/$ARGUMENTS/brief.md
   ```
4. Implement the change following those rules. **Do not commit.**
