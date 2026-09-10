#!/usr/bin/env bash
set -euo pipefail

if ! listing="$(ci/list-unit-packages.sh)"; then
  echo "::error title=Unit package listing failed::ci/list-unit-packages.sh exited non-zero" >&2
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
  echo "no unit packages selected" >&2
  exit 1
fi

printf 'Unit packages (%d):\n' "${#packages[@]}"
printf '  %s\n' "${packages[@]}"

go test -race -shuffle=on -count=1 -timeout 2m "${packages[@]}"
