---
type: Task
title: "Task: <slug>"
description: <one-line summary of the task>
tags: []
timestamp: <ISO8601>
# --- octospec extension fields ---
slug: <slug>
upstream: <issue ref, e.g. repo#123>
source: self
---

# Task: <slug>

> One task = one `.octospec/tasks/<slug>/` directory. This brief is the spec for
> the work. AI may draft it from existing code; a human confirms it.

## Goal
<!-- What behavior changes and why. -->

## Background
<!-- Context a reviewer needs. Links to issue, prior art. -->

## Load-bearing list
<!-- Existing behaviors/contracts this change touches. Drives review depth and
     rule injection (touches: tags). Be honest and complete. -->
- 

## Out of scope
<!-- What this change deliberately does NOT touch. -->
- 

## Acceptance
<!-- Machine-checkable where possible: tests, assertions, repro that must pass. -->
- 
