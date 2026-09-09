---
type: Learning
title: A batch status label comes from the layer that stops
description: "When execution and reporting live in different layers, only the layer that decides where execution stops can truthfully label what ran; the other layer's labels describe a world that may no longer exist."
tags: ["api-design", "batch", "reporting", "review"]
timestamp: 2026-09-06T03:40:00Z
status: pending
source: project-p0-foundation
---

# A batch status label comes from the layer that stops

## What happened

A batch endpoint ran per-target transactions in the service layer, then reported per-target
outcomes in the handler. When an actor-level authorization failure landed mid-batch, the
handler labelled the remaining targets `not_attempted` — but the service had already executed
them. Some had committed. A committed write was reported as never tried, its audit entry was
never written, and a client trusting the report would retry it.

## The rule

`attempted / not_attempted / committed / rejected` are facts about **execution**. Whoever
controls the loop that executes owns those labels. If the reporting layer wants to stop a
batch, it must do so by asking the executing layer to stop — not by relabelling results the
executing layer already produced. Symmetry check: if two endpoints share a label, run the
"what would this label claim" question against BOTH execution models; same word, different
execution model, guaranteed drift.
