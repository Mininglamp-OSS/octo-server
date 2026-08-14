#!/usr/bin/env bash
set -euo pipefail

if [ "$#" -ne 2 ]; then
  echo "usage: $0 <shard> <total-shards>" >&2
  exit 2
fi

shard="$1"
total="$2"
if ! [[ "$shard" =~ ^[0-9]+$ && "$total" =~ ^[0-9]+$ ]]; then
  echo "shard and total-shards must be positive integers" >&2
  exit 2
fi
if [ "$shard" -lt 1 ] || [ "$total" -lt 1 ] || [ "$shard" -gt "$total" ]; then
  echo "invalid shard $shard/$total" >&2
  exit 2
fi

tmpdir="$(mktemp -d)"
trap 'rm -rf "$tmpdir"' EXIT

all_tests="$tmpdir/all-tests.txt"
unit="$tmpdir/unit.txt"
e2e="$tmpdir/e2e.txt"
e2e_sorted="$tmpdir/e2e-sorted.txt"
weights_sorted="$tmpdir/weights-sorted.txt"
weighted="$tmpdir/weighted.txt"

go list -f '{{if or .TestGoFiles .XTestGoFiles}}{{.ImportPath}}{{end}}' ./... |
  sed '/^$/d' |
  sort -u > "$all_tests"

ci/list-unit-packages.sh > "$unit"
grep -vxF -f "$unit" "$all_tests" > "$e2e" || true

if [ ! -s "$e2e" ]; then
  exit 0
fi

sort -u "$e2e" > "$e2e_sorted"
awk '!/^#/ && NF >= 2 { print $1 "\t" $2 }' ci/e2e-package-weights.tsv |
  sort -k1,1 > "$weights_sorted"

join -t "$(printf '\t')" -a 1 -e 1000 -o '2.2 1.1' "$e2e_sorted" "$weights_sorted" |
  sort -k1,1nr -k2,2 > "$weighted"

for ((i = 0; i < total; i++)); do
  loads[$i]=0
  files[$i]="$tmpdir/shard-$((i + 1)).txt"
  : > "${files[$i]}"
done

while IFS=$'\t' read -r weight pkg; do
  target=0
  for ((i = 1; i < total; i++)); do
    if [ "${loads[$i]}" -lt "${loads[$target]}" ]; then
      target="$i"
    fi
  done
  printf '%s\n' "$pkg" >> "${files[$target]}"
  loads[$target]=$((loads[$target] + weight))
done < "$weighted"

cat "${files[$((shard - 1))]}"
