---
type: Task
title: "Task: agent-mail-http2-response"
description: "Allow bounded unknown-length Agent Mail responses to reach HTTP/2 browser clients."
tags: ["agent-mail", "gateway", "wire-contract", "error-response", "testing"]
timestamp: 2026-08-19T06:55:00Z
slug: agent-mail-http2-response
source: user
upstream: "https://github.com/Mininglamp-OSS/octo-server/issues/789"
---

# Task: agent-mail-http2-response

## Goal

Fix the Agent Mail gateway returning 502 for valid octo-mail responses whose
length is unknown when the downstream hop reaching the gateway uses HTTP/2.

## Background

octo-mail can return dynamically generated message details without a known
`Content-Length`. The gateway currently streams such responses only to HTTP/1.1
clients and rejects them for HTTP/2, which makes larger external HTML messages
unreadable even though octo-mail received and parsed them successfully.

## Load-bearing list

- **Response integrity**: an incomplete or oversized upstream body must never be
  exposed as a successful complete HTTP/2 response.
- **Response limit**: retain the existing 64 MiB Agent Mail response limit.
- **Aggregate memory bound**: cap concurrent HTTP/2 response buffering so one
  account cannot multiply the per-response limit without bound.
- **Wire contract**: preserve upstream status, headers, and body for successful
  responses; preserve the existing localized gateway error for failures.
- **HTTP/1.1 streaming**: keep the existing streaming behavior for self-delimited
  HTTP/1.1 responses.

## Out of scope

- No octo-mail, Web, API, authentication, Space, or mailbox behavior changes.
- No deployment configuration or timeout changes.
- No change to known-length response streaming.

## Acceptance

- A real HTTP/2 client connected to the gateway handler receives a valid
  unknown-length response with status 200, its complete body, and a computed
  `Content-Length`.
- A failed unknown-length upstream read returns the existing 502 error envelope
  before any partial upstream body is sent to an HTTP/2 client.
- Concurrent response buffering has an aggregate budget, and waiting for it
  honors request cancellation.
- Existing HTTP/1.1 streaming and response-size protections remain green.
- `go test ./modules/agentmailgateway/... -count=1` and `golangci-lint` pass.
