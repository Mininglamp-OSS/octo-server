#!/usr/bin/env bash
set -euo pipefail

unit_file="${1:-ci/unit-packages.txt}"

patterns=()
while IFS= read -r line || [ -n "$line" ]; do
  line="${line%%#*}"
  line="${line#"${line%%[![:space:]]*}"}"
  line="${line%"${line##*[![:space:]]}"}"
  if [ -n "$line" ]; then
    patterns+=("$line")
  fi
done < "$unit_file"

if [ "${#patterns[@]}" -eq 0 ]; then
  echo "no unit package patterns in $unit_file" >&2
  exit 1
fi

go list "${patterns[@]}" | sort -u
