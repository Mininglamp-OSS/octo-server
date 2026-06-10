#!/usr/bin/env bash
#
# normalize-spec.sh — post-process swag output to fix known quirks.
#
# Why: swag v2.0.0-rc5 unconditionally emits an empty `externalDocs`
# block (description: "", url: ""), which violates the OpenAPI 3.x
# schema rule that `url` must be a uri. spectral lint then fails with
# "url property must match format uri".
#
# This script removes the empty externalDocs from both swagger.yaml and
# swagger.json post-generation. When swag fixes this upstream, remove the
# normalization step from openapi.mk.
#
# Usage:
#   bash normalize-spec.sh <openapi-out-dir>

set -euo pipefail

OUT_DIR="${1:?usage: normalize-spec.sh <openapi-out-dir>}"

if [ ! -d "$OUT_DIR" ]; then
  echo "❌ output dir not found: $OUT_DIR" >&2
  exit 1
fi

command -v python3 >/dev/null 2>&1 || {
  echo "⚠  python3 not found — skipping spec normalization" >&2
  exit 0
}

python3 - "$OUT_DIR" <<'PY'
import json, os, sys

out_dir = sys.argv[1]

# --- swagger.yaml: line-based delete of empty externalDocs block ---
yaml_path = os.path.join(out_dir, 'swagger.yaml')
if os.path.exists(yaml_path):
    with open(yaml_path) as f:
        lines = f.read().split('\n')
    out = []
    i = 0
    while i < len(lines):
        if (lines[i] == 'externalDocs:'
                and i + 2 < len(lines)
                and lines[i + 1] == '  description: ""'
                and lines[i + 2] == '  url: ""'):
            i += 3
            continue
        out.append(lines[i])
        i += 1
    with open(yaml_path, 'w') as f:
        f.write('\n'.join(out))

# --- swagger.json: parse + delete empty externalDocs ---
json_path = os.path.join(out_dir, 'swagger.json')
if os.path.exists(json_path):
    with open(json_path) as f:
        data = json.load(f)
    e = data.get('externalDocs')
    if isinstance(e, dict) and not e.get('url'):
        del data['externalDocs']
        with open(json_path, 'w') as f:
            json.dump(data, f, indent=4, ensure_ascii=False)
            f.write('\n')

print(f"✓ normalized spec in {out_dir}", file=sys.stderr)
PY
