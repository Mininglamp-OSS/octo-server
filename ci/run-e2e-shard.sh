#!/usr/bin/env bash
set -euo pipefail

if [ "$#" -ne 2 ]; then
  echo "usage: $0 <shard> <total-shards>" >&2
  exit 2
fi

shard="$1"
total="$2"
packages=()
while IFS= read -r pkg; do
  packages+=("$pkg")
done < <(ci/list-e2e-shard.sh "$shard" "$total")

if [ "${#packages[@]}" -eq 0 ]; then
  echo "No E2E packages assigned to shard $shard/$total"
  exit 0
fi

printf 'E2E packages for shard %s/%s (%d):\n' "$shard" "$total" "${#packages[@]}"
printf '  %s\n' "${packages[@]}"

fail=0
failed=()

for pkg in "${packages[@]}"; do
  mysql -h 127.0.0.1 -uroot -pdemo -e "DROP DATABASE IF EXISTS test; CREATE DATABASE test CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci;"
  redis-cli -h 127.0.0.1 -p 6379 FLUSHALL >/dev/null
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
