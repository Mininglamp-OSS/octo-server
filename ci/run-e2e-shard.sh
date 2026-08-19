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
mysql_exec() {
  if [ -n "${MYSQL_CID:-}" ]; then
    docker exec "$MYSQL_CID" mysql -h 127.0.0.1 -uroot -pdemo -e "$1"
  else
    mysql -h 127.0.0.1 -uroot -pdemo -e "$1"
  fi
}

redis_flush() {
  if [ -n "${REDIS_CID:-}" ]; then
    docker exec "$REDIS_CID" redis-cli FLUSHALL >/dev/null
  else
    redis-cli -h 127.0.0.1 -p 6379 FLUSHALL >/dev/null
  fi
}

fail=0
failed=()

for pkg in "${packages[@]}"; do
  mysql_exec "DROP DATABASE IF EXISTS test; CREATE DATABASE test CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci;"
  redis_flush
  echo "::group::go test $pkg"
  if ! go test -race -shuffle=on -count=1 -timeout 5m "$pkg"; then
    fail=1
    failed+=("$pkg")
    echo "::error title=Package failed::$pkg"
  fi
  echo "::endgroup::"
done

if [ "$fail" -ne 0 ]; then
  echo "::error title=E2E shard $shard/$total summary::${#failed[@]} package(s) failed: ${failed[*]}"
fi
exit "$fail"
