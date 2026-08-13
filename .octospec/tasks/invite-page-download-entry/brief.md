---
type: Task
title: "Task: invite-page-download-entry"
description: Resolve the group invite landing page's app-download entry from the public updater endpoint instead of the API base URL.
tags: ["group", "invite", "h5", "download"]
timestamp: 2026-08-13T00:00:00Z
# --- octospec extension fields ---
slug: invite-page-download-entry
upstream: Mininglamp-OSS/octo-server (regression from 10cff2da "feat(group): public H5 invite landing page")
source: self
---

# Task: invite-page-download-entry

## Goal

Make the group invite landing page's app-download entry actually lead to an
installer.

The page bound its single "download app" button to the injected `API_BASE`. In
production `External.BaseURL` is the API prefix (`https://<host>/api`), so the
only download entry point on the page was a guaranteed 404 — verified live
against the deployed host. The code comment said "fall back to the product
site", but the implementation reused the API base; semantics and implementation
had diverged.

## Background

Two public, unauthenticated endpoints already serve exactly what the page needs,
so no new configuration is required:

| Endpoint | Returns | Existing consumer |
|---|---|---|
| `GET /v1/common/updater/{os}/{version}` | `{"url": "<installer>"}` | web login page download popover |
| `GET /v1/common/appconfig` | `web_url` (= `External.WebLoginURL`) | clients |

`octo-web`'s `dmworklogin` package already resolves installer addresses through
the updater endpoint (`common/updater/android/1.0`, `common/updater/ios/1.0.0`).
The invite page was simply never wired to it.

`appconfig.web_url` is used as a last resort, but only when **neither** platform
yields a usable installer. That case is reachable in practice — both platforms
lacking a version record, the updater returning 5xx, the network failing, or every
address failing validation — and without a fallback the page renders a group card
above an unexplained blank, with the invite-detail request succeeding so nothing
looks wrong. An earlier revision ruled `web_url` out on the grounds that both
buttons render everywhere; that reasoning assumed at least one row resolves, which
is exactly the assumption this case violates.

Prior art constraining the design: `octo-server#1246` removed the `dmwork://`
deep link because neither mobile client registers the scheme; a grep test pins
that. No "open in app" button may be reintroduced.

## Load-bearing list

- **Public invite landing page** (`assets/web/group_invite.html`) — unauthenticated,
  rate-limited, the first product surface many users see. Rendered by
  `groupInvitePage` (`modules/group/api.go`) via `{{API_BASE_URL}}` substitution.
- **`External.BaseURL` injection contract** — still injected as `API_BASE` and
  still used for API calls. Only its misuse as a *download page* address is removed.
- **Cross-module route dependency** — the page hardcodes `modules/common`'s
  `/v1/common/updater/:os/:version` as a string with no symbolic reference from Go.
- **URL destination handling** (`.octospec/rules/trust-boundary.md`) — the updater
  returns the raw `download_url` column, which ends up in `a.href`.
- **Existing landing-page behavior** — join button, `need_space` / `external_blocked` /
  expired / rate-limited / server-error / network-error states, `findTokenAndSid`
  session discovery. None of it changes.

## Out of scope

- `octo-web`: no shared download component, no download entry in the logged-in
  shell. The invite page is self-contained instead.
- `appconfig.web_url` as a desktop fallback — unnecessary once both buttons render
  on desktop.
- The other landing pages (`space_email_invite.html`, `space_join_approve.html`) —
  they have no download button and are unaffected.
- The pre-existing `t.Skip` on `TestGroupInvitePage_NoDmworkScheme` (OCTO migration
  TODO, issue #17). Note it will collide with the existing `need_space` copy
  "请打开 App 或 Web 首页" whenever it is unskipped — unrelated to this change.

## Acceptance

- [x] The download button's href never derives from `API_BASE`
      (`TestGroupInvitePage_DownloadButtonResolvesInstallerNotAPIBase` asserts the
      *source* of the href, not merely that the placeholder was substituted — the
      broken code satisfied the latter, which is how it reached production).
- [x] Both platform paths are resolved from the public updater endpoint, and those
      routes are actually served (`TestGroupInvitePage_UpdaterRoutesAreServed`
      issues a real request; a rename would otherwise keep every grep assertion green).
- [x] No user-agent sniffing — in-app browsers rewrite the UA and iPadOS reports
      itself as Macintosh, so a wrong guess hands over the wrong platform's installer.
- [x] A platform with no version record (updater 204), a malformed address, or a
      failed request hides only its own row. Nothing falls back to `API_BASE`.
- [x] Installer addresses must be absolute http/https **and carry a file suffix**.
      Relative values are rejected rather than resolved: this page and the web login
      page would resolve them against different bases (`API_BASE` vs
      `location.origin`), so at most one could be right. A suffix-less address is a
      landing page, not a package — a download button that opens a web page is the
      same broken promise this task exists to remove.
- [x] When neither platform yields an installer, fall back to `appconfig.web_url`
      (absolute http/https, no suffix requirement — a portal is not a package).
      If that is unavailable too, still show nothing; never `API_BASE`.
- [x] Copying chains the `execCommand` fallback onto a **rejected** `writeText`,
      not merely onto the Clipboard API being absent. Embedded IM webviews commonly
      expose `navigator.clipboard` under HTTPS and then deny the write, which is
      precisely the environment the fallback exists for.
- [x] Zero external resources: no CDN script, external stylesheet, `@import`,
      `url(http…)` or remote image; icons are inline SVG
      (`TestGroupInvitePage_NoExternalResources`).
- [x] `modules/group` passes under the CI recipe (fresh database, `-race`,
      `-shuffle=on`).

## Known verification gap

The landing page is a static asset, so the Go assertions can only grep its
*shape*, not exercise its *behavior*. Reintroducing the bug through a local
variable would evade them. Closing this properly means either moving the logic
server-side or giving the repository a JS test environment; neither is in scope
here. Behavior was verified out-of-band with a headless-browser harness against a
stub server (platform matrix, updater 204, malformed scheme, network failure, and
a real clipboard read-back), but that harness is not part of the repository.
