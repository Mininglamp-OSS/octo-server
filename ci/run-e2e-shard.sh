#!/usr/bin/env bash
set -euo pipefail

if [ "$#" -ne 2 ]; then
  echo "usage: $0 <shard> <total-shards>" >&2
  exit 2
fi

shard="$1"
total="$2"

if ! listing="$(ci/list-e2e-shard.sh "$shard" "$total")"; then
  echo "::error title=E2E shard listing failed::ci/list-e2e-shard.sh $shard $total exited non-zero" >&2
  exit 1
fi

packages=()
while IFS= read -r pkg; do
  if [ -n "$pkg" ]; then
    packages+=("$pkg")
  fi
done <<EOF
$listing
EOF

if [ "${#packages[@]}" -eq 0 ]; then
  echo "::error title=E2E shard has no packages::No E2E packages assigned to shard $shard/$total" >&2
  exit 1
fi

printf 'E2E packages for shard %s/%s (%d):\n' "$shard" "$total" "${#packages[@]}"
printf '  %s\n' "${packages[@]}"

# The mysql and redis clients live inside the service containers, so CI reaches
# them with `docker exec` rather than installing mysql-client/redis-tools onto
# the runner. That install was an unbounded apt call with no lock timeout: on
# 2026-08-19 it hung two shards for 29.5 minutes each and both were cancelled by
# the job timeout without running a single test. Nothing here depends on a
# package mirror any more.
#
# MYSQL_CID / REDIS_CID are set by the workflow from `job.services.<name>.id`.
# When they are absent the script falls back to local binaries, so running it
# outside CI against a hand-started MySQL/Redis still works.
#
# In Actions that fallback would be a trap rather than a convenience: an absent
# context property renders as the empty string with no error, so a service
# rename or a step that forgets its `env:` block would silently select the
# branch whose binaries CI no longer installs. Fail there instead, naming the
# cause, rather than surfacing later as `redis-cli: command not found`.
if [ "${GITHUB_ACTIONS:-}" = "true" ]; then
  for var in MYSQL_CID REDIS_CID; do
    if [ -z "${!var:-}" ]; then
      echo "::error title=Missing service container id::$var is empty; the workflow must pass job.services.<name>.id" >&2
      exit 1
    fi
  done
fi

mysql_exec() {
  if [ -n "${MYSQL_CID:-}" ]; then
    docker exec "$MYSQL_CID" mysql -h 127.0.0.1 -uroot -pdemo -e "$1"
  else
    mysql -h 127.0.0.1 -uroot -pdemo -e "$1"
  fi
}

# `-e` is load-bearing, not decoration: redis-cli writes server error replies to
# *stdout* and still exits 0, so the previous `redis-cli FLUSHALL >/dev/null`
# swallowed the message and returned success. A write-blocking reply such as
# MISCONF under runner disk pressure would have left the next package running
# against un-flushed Redis, surfacing as an unrelated flaky failure. Verified on
# redis 7.0.15: an error reply gives exit 0 bare and exit 1 with `-e`. The output
# is kept (one `OK` per package) for the same reason -- a fully silenced step can
# only be diagnosed by noticing a missing timestamp.
redis_flush() {
  if [ -n "${REDIS_CID:-}" ]; then
    docker exec "$REDIS_CID" redis-cli -e FLUSHALL
  else
    redis-cli -h 127.0.0.1 -p 6379 -e FLUSHALL
  fi
}

# Per-package Go timeout.
#
# It was 5m, and modules/user grew into it: measured at 272s with
# `-race -shuffle=on` on a machine comparable to a runner, i.e. 9% of headroom
# against the cap. A runner a little slower than that tips the package over and
# the shard fails for a reason that has nothing to do with the code — which is
# what happened on PR #846, and what made #844's shard-1 failure so hard to
# read, since a timeout and a broken test look identical from the outside.
#
# Kept as a Go timeout rather than deleted, deliberately. The job's own
# `timeout-minutes: 30` already bounds the shard, but it kills the runner and
# prints nothing; Go's timeout panics and dumps every goroutine, which is the
# only artifact that tells you WHICH test hung. The value is the slowest
# package plus real headroom, not a guess at how long a test "should" take.
#
# Overridable so a bisect or a slow runner does not need a commit.
PKG_TIMEOUT="${E2E_PKG_TIMEOUT:-12m}"

logdir="$(mktemp -d)"
trap 'rm -rf "$logdir"' EXIT

fail=0
failed=()
failed_logs=()

for pkg in "${packages[@]}"; do
  mysql_exec "DROP DATABASE IF EXISTS test; CREATE DATABASE test CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci;"
  redis_flush
  pkglog="$logdir/$(printf '%s' "$pkg" | tr '/.' '__').log"
  echo "::group::go test $pkg"
  # tee, so the same output can be replayed in the summary below. `pipefail` is
  # already set, so the pipeline still reports go test's status.
  if ! go test -race -shuffle=on -count=1 -timeout "$PKG_TIMEOUT" "$pkg" 2>&1 | tee "$pkglog"; then
    fail=1
    failed+=("$pkg")
    failed_logs+=("$pkglog")
    echo "::error title=Package failed::$pkg"
  fi
  echo "::endgroup::"
done

if [ "$fail" -ne 0 ]; then
  # Repeat the verdict lines at the END of the step.
  #
  # Not decoration. These packages boot a real server per test, so one shard
  # emits hundreds of thousands of lines of gin route dumps and broker debug
  # logs, and the service-container teardown adds hundreds of thousands more
  # AFTER the step. The GitHub log API only serves a bounded tail, so the
  # `--- FAIL` lines end up outside anything a reader — or a tool — can fetch.
  # That happened three times on #844/#846 and cost more than the bugs did.
  # Reprinting them last puts the one thing you need where it is always
  # reachable.
  echo "::group::E2E shard $shard/$total — failure summary"
  for i in "${!failed[@]}"; do
    echo "----- ${failed[$i]}"
    grep -E '^(panic:|fatal error:|--- FAIL|=== FAIL|FAIL|ok )|test timed out' \
      "${failed_logs[$i]}" | head -n 80 || true
    echo
  done
  echo "::endgroup::"
  echo "::error title=E2E shard $shard/$total summary::${#failed[@]} package(s) failed: ${failed[*]}"
fi
exit "$fail"
