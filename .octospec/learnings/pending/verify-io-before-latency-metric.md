---
type: Learning
title: "Verify a method does the I/O before wrapping it in a latency metric"
description: A facade method's name/category can imply I/O it doesn't perform; instrument the operation that actually round-trips.
tags: ["observability", "metrics", "prometheus", "review"]
timestamp: 2026-06-23T09:23:17Z
# --- octospec extension fields ---
source: self
origin_task: upstream-dep-metrics
origin_pr: Mininglamp-OSS/octo-server#442
status: pending
candidate_rule: observability
---

# Verify a method does the I/O before wrapping it in a latency metric

## Context
In #442 (P0-a dependency observability) the object-storage latency histogram was
first wrapped around `file.Service.DownloadURL`, on the assumption that "get a
download URL from object storage" is a storage round-trip. It is not: every
backend's `DownloadURL` is pure local URL string-building (`url.JoinPath` /
`publicURL`). The metric recorded sub-µs work (all samples in `le=0.001`) and was
*actively misleading* — an operator would read "objectstore latency <1ms" and
rule storage out, while the real I/O methods (`UploadFile`→PutObject,
`GetFile`→GetObject+Stat) were uninstrumented.

## Rule of thumb
Before adding a duration metric around a call:
1. **Trace the implementation to a syscall / network round-trip.** A method name
   or dependency category (`objectstore`, `DownloadURL`) can imply I/O the body
   doesn't perform.
2. **Beware lazy clients.** e.g. minio-go `GetObject` defers the request to first
   `Read`/`Stat`; timing the constructor measures nothing. Instrument up to the
   point that forces the round-trip (`obj.Stat()`).
3. **Sanity-check buckets vs. expected magnitude.** If every observation would
   land in the smallest bucket, the metric is probably measuring the wrong thing.

## Why worth a rule
A misleading latency metric is worse than a missing one during incident triage.
This is cheap to check at authoring time and was caught only by source-tracing in
review, not by either automated pass.
