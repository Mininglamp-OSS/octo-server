---
type: Learning
title: Scope explicit-empty environment validation to one key
description: Detecting an explicitly empty security configuration must not mutate the shared configuration object's semantics for unrelated keys.
tags: ["configuration", "security", "viper", "environment"]
timestamp: 2026-08-09T18:42:28+08:00
# --- octospec extension fields ---
source: self
origin_task: token-lifecycle-hardening
status: pending
candidate_rule: configuration
---

# Scope explicit-empty environment validation to one key

## Context

The token TTL must distinguish an absent environment variable (use the bounded
default or YAML) from an explicitly empty variable (configuration error). A first
implementation enabled Viper's `AllowEmptyEnv` on the shared instance before the
rest of the application loaded configuration. That instance-wide switch made
unrelated empty `TS_*` variables override YAML as well, including database and
Redis TLS settings.

## Rule of thumb

When one security-sensitive setting needs absent-versus-empty detection:

1. Use `os.LookupEnv` for the documented env key and strictly parse it when
   present, including the empty string.
2. Read the YAML/default path only when that env key is absent.
3. Do not toggle an instance-wide or process-wide empty-env policy as a local
   validation shortcut.
4. Add a regression that loads unrelated security/connection values from YAML,
   sets their env counterparts empty, and proves validation leaves them intact.
5. Also prove a non-empty unrelated env override still wins, so the fix does not
   disable legitimate configuration precedence.

## Why worth a rule

Configuration validation runs before most dependencies are initialized, so a
silent precedence change can redirect a service to the wrong database or disable
transport security while startup still appears successful. The safe pattern is
small, deterministic, and applies to every fail-loud security knob, not only
token TTL.
