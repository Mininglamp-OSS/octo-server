#!/usr/bin/env bash
set -euo pipefail

packages=()
while IFS= read -r pkg; do
  packages+=("$pkg")
done < <(ci/list-unit-packages.sh)

if [ "${#packages[@]}" -eq 0 ]; then
  echo "no unit packages selected" >&2
  exit 1
fi

printf 'Unit packages (%d):\n' "${#packages[@]}"
printf '  %s\n' "${packages[@]}"

go test -race -shuffle=on -count=1 -timeout 2m "${packages[@]}"
