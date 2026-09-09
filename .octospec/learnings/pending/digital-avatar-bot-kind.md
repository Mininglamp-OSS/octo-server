---
title: "AI Team projection must be an explicit resource relationship"
description: "A Space seat or stale group membership must not authorize an Avatar outside its server-managed AI Team projection."
tags: ["authorization", "ai-team", "avatar", "space-isolation"]
timestamp: 2026-09-09T19:25:00+08:00
---

# Candidate learning

When one principal has a broad discovery/seat relationship but must collaborate
only through a managed projection, authorization must require the projection
row itself. Checking only the Space seat, group membership, or parent channel
creates a stale-row bypass after removal. Keep the projected relation as a
server-owned resource and test that legacy membership rows alone grant no
runtime access.
