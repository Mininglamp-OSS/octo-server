#!/usr/bin/env python3
"""Independent synthetic v42 conformance input, never a production catalog.

Uses Python's ASCII metadata serializer and hashlib, not the server verifier or
MCP emitter. Root payloads and their renderer oracle are literal exact bytes.
All 827 identities and 57,996 selector cells are present; business semantics,
source equivalence, native authority and support approval are not claimed.
"""
import gzip
import hashlib
import io
import json
import pathlib
import tarfile

ROOT = pathlib.Path(__file__).resolve().parents[1]
OUT = ROOT / "pkg/permissionbundle/testdata"
IDENTITIES = ROOT / "pkg/permissionbundle/contracts/permission-catalog-identities-v1.json"
MODULES = ["_global", "bot", "docs", "drive", "event", "file", "group", "html",
           "loop", "mail", "marketplace", "message", "thread"]


def encode(value):
    return json.dumps(value, sort_keys=True, separators=(",", ":"), ensure_ascii=False).encode()


def digest(data):
    return hashlib.sha256(data).hexdigest()


def nodes(value):
    return 1 + (sum(map(nodes, value.values())) if isinstance(value, dict) else
                sum(map(nodes, value)) if isinstance(value, list) else 0)


def depth(value):
    children = value.values() if isinstance(value, dict) else value if isinstance(value, list) else []
    return 1 + max((depth(child) for child in children), default=0)


def build(use_slots=False):
    identities = json.loads(IDENTITIES.read_bytes())
    profiles = json.loads((OUT / "profile-examples-v6.json").read_bytes())
    # Positive inherited null, distinct from omitted literal/new packaging null.
    leaf = next(p for p in profiles if p["name"] == "leaf")
    leaf["prepare"][0]["condition"] = {"op": "eq", "argument": "x", "literal": None}
    content, roots, rows, raw_roots = {}, {}, {}, {}
    blob = bytearray()

    def root(data, split=False):
        h = digest(data)
        if h not in roots:
            start = len(blob)
            blob.extend(data)
            if not data:
                parts = []
            elif split:
                # A slice boundary inside the three-byte U+2028 encoding.
                cut = data.index(b"\xe2\x80\xa8") + 1
                parts = [{"path": "blobs/0.bin", "offset": start, "length": cut},
                         {"path": "blobs/0.bin", "offset": start + cut, "length": len(data) - cut}]
            else:
                parts = [{"path": "blobs/0.bin", "offset": start, "length": len(data)}]
            roots[h] = {"sha256": h, "length": len(data), "parts": parts}
            raw_roots[h] = data
        return h

    def record(value):
        h = value["sha256"] if value["kind"] == "bytes" else digest(encode({k: v for k, v in value.items() if k != "sha256"}))
        if value["kind"] != "schema_variant":
            value["sha256"] = h
        content[h] = value
        return h

    def byte_record(data, media="application/json"):
        h = root(data)
        return record({"kind": "bytes", "sha256": h, "length": len(data), "media_type": media, "data_ref": h})

    template_raw = b'{"z":"<>&\xe2\x80\xa8","a":null,"number":1e0,"literal":"$static_ref"}'
    slots = []
    if use_slots:
        template_raw = template_raw[:-1] + b',"hint":{"$octo_slot":"facade"},"names":{"$octo_slot":"names"}}'
        slots = [{"id": "facade", "domain": "facade_hint", "pointer": "/hint", "max_length": 64},
                 {"id": "names", "domain": "server_managed_argument_names", "pointer": "/names", "max_length": 256}]
    template_root = root(template_raw, split=True)
    template = record({"kind": "template", "media_type": "application/json", "renderer": "go-json-marshalindent-2sp-html-escape-v1", "value_ref": template_root, "slots": slots})
    # This manual oracle preserves member order and numeric spelling. It does
    # not call either implementation's renderer.
    rendered = b'{\n  "z": "\\u003c\\u003e\\u0026\\u2028",\n  "a": null,\n  "number": 1e0,\n  "literal": "$static_ref"\n}'
    if use_slots:
        rendered = rendered[:-2] + b',\n  "hint": "three",\n  "names": [\n    "channel_id",\n    "channel_type",\n    "space_id"\n  ]\n}'
    static_raw = b'{"static":null,"n":1.00}'
    static_root = root(static_raw)
    static = record({"kind": "template", "media_type": "application/json", "renderer": "go-json-marshalindent-2sp-html-escape-v1", "value_ref": static_root, "slots": []})
    static_rendered = b'{\n  "static": null,\n  "n": 1.00\n}'
    resource = byte_record(b"\xff\x00opaque $static_ref\n", "application/octet-stream")
    only_source = byte_record(b"source-only bytes\n", "application/octet-stream")
    empty = byte_record(b"", "application/octet-stream")
    by_op, op_service = {}, {}
    for e in identities:
        if e["kind"] != "operation":
            continue
        service, op, _, _ = e["target"]
        op_service[op] = service
        truth = byte_record(b'{"operation_id":' + encode(op) + b',"value":null,"number":1e0}')
        by_op[op] = [record({"kind": "schema_variant", "operation_id": op, "facade": facade,
                            "template_hash": template, "parameter_truth_hash": truth,
                            "selector": {"resources_bits": 3, "managed_bits": 255, "facade_bits": 7}})
                     for facade in ["describe_op", "get_skill_full", "get_skill_summary"]]
        data = b'{"operation_id":' + encode(op) + b',"v":null}'
        row = {"operation_id": op, "match_text": op.lower(), "row_ref": root(data)}
        h = digest(encode(row))
        rows[h] = dict(row, sha256=h)
    indices = {}
    for scope in ["global"] + MODULES[1:]:
        indices[scope] = []
        row_ids = [h for h, r in sorted(rows.items(), key=lambda pair: pair[1]["operation_id"])
                   if scope == "global" or op_service[r["operation_id"]] == scope]
        for facade in ["search_ops", "get_skill_search"]:
            indices[scope].append(record({"kind": "discovery_index", "facade": facade,
                "media_type": "application/json", "renderer": "discovery-search-render-v1",
                "scope": {"kind": "global"} if scope == "global" else {"kind": "named", "service": scope},
                "page_limit": 1000, "query_limits": {"bytes": 256, "runes": 128, "terms": 8},
                "notes": {"more_matches_format": "synthetic more", "query_bounded_format": "synthetic bounded"},
                "rows": row_ids, "selector": {"resources_bits": 3, "facade_bits": 7}}))

    entries = {}
    static_bindings = []
    profiles_for_kind = {"operation": "leaf", "schema": "static-schema", "resource": "static-resource",
                         "inventory": "resource-inventory", "secret": "secret", "derived": "portable-bytes", "discovery": "static-discovery"}
    for original in identities:
        e = dict(original, profile=profiles_for_kind[original["kind"]], content_refs=[])
        kind, eid = e["kind"], e["id"]
        if kind in ("operation", "schema"):
            e["content_refs"] = sorted(by_op[e["target"][1]])
        elif kind == "discovery" and eid != "octo-cli/discovery/module-map":
            e["content_refs"] = sorted(indices["global" if eid == "octo-cli/discovery/search-all" else e["target"]])
        elif kind in ("resource", "inventory") or eid == "octo-cli/discovery/module-map":
            ref = resource if kind == "resource" else static
            e["content_refs"] = [ref]
            static_bindings.append({"entry_id": eid, "selector": {"resources_bits": 3, "managed_bits": 255, "facade_bits": 7}, "content_ref": ref})
        entries[eid] = e

    # Independently derive complete Entry/profile closure for physical placement.
    profile_map = {p["name"]: p for p in profiles}
    entry_modules = {}
    for eid, e in entries.items():
        kind = e["kind"]
        entry_modules[eid] = (e["target"][0] if kind in ("operation", "schema") else
            e["target"] if kind == "discovery" and "/discovery/search" in eid and eid != "octo-cli/discovery/search-all" else
            "bot" if kind == "secret" else "docs" if kind == "derived" else "_global")
    labels = {h: set() for h in content}
    def visit_content(h, label):
        if label in labels[h]: return
        labels[h].add(label)
        c = content[h]
        if c["kind"] == "schema_variant":
            visit_content(c["template_hash"], label)
            visit_content(c["parameter_truth_hash"], label)
    def visit_entry(eid, label, seen):
        if eid in seen: return
        seen.add(eid)
        e = entries[eid]
        for h in e["content_refs"]: visit_content(h, label)
        p = profile_map[e["profile"]]
        deps = set(p["children"])
        for phase in ("prepare", "dispatch", "publish"):
            for a in p[phase]:
                if a["capability"] != "self": deps.add(a["capability"])
                b = a["target_template"]["binding"]
                if b["kind"] == "capability": deps.add(b["id"])
        for dep in deps - {eid}: visit_entry(dep, label, seen)
    for eid in entries: visit_entry(eid, entry_modules[eid], set())
    visit_content(only_source, "_global")
    visit_content(empty, "_global")
    files = {}
    index = {"kind": "index", "profiles": profiles,
             "source_files": [["synthetic/empty", empty], ["synthetic/source-only", only_source]],
             "exceptions": ["fixed tools/list definitions", "initialize", "nonexecuting initialized/cancelled notifications", "nonexecuting transport acknowledgements", "ping"],
             "static_bindings": sorted(static_bindings, key=lambda b: b["entry_id"])}
    files["index.json"] = encode(index)
    for module in MODULES:
        es = [e for eid, e in sorted(entries.items()) if entry_modules[eid] == module]
        cs = [c for h, c in sorted(content.items()) if min(labels[h]) == module]
        current = {"kind": "module", "module": module, "shard": 0, "entries": [], "content": []}
        for field, values in [("entries", es), ("content", cs)]:
            for value in values:
                current[field].append(value)
                if len(encode(current)) > 262144:
                    current[field].pop()
                    files[f"modules/{module}/{current['shard']}.json"] = encode(current)
                    current = {"kind": "module", "module": module, "shard": current["shard"] + 1, "entries": [], "content": []}
                    current[field].append(value)
        files[f"modules/{module}/{current['shard']}.json"] = encode(current)
    shared = {"kind": "shared", "shard": 0, "roots": [], "rows": []}
    for field, values in [("roots", [r for _, r in sorted(roots.items())]), ("rows", [r for _, r in sorted(rows.items())])]:
        for value in values:
            shared[field].append(value)
            if len(encode(shared)) > 262144:
                shared[field].pop()
                files[f"shared/{shared['shard']:02}.json"] = encode(shared)
                shared = {"kind": "shared", "shard": shared["shard"] + 1, "roots": [], "rows": []}
                shared[field].append(value)
    files[f"shared/{shared['shard']:02}.json"] = encode(shared)
    files["blobs/0.bin"] = bytes(blob)
    limits = {"artifact_bytes": 8388608, "entries": 827, "profile_nodes": 64, "conditional_children": 8,
              "local_input_bytes": 16777216, "page_limit": 1000, "redirect_hops": 9,
              "packaging": {"files": 64, "manifest_bytes": 65536, "json_file_bytes": 262144,
                "blob_file_bytes": 1048576, "roots": 8192, "shared_rows": 4096, "content_records": 16384,
                "parts_per_root": 256, "parts_total": 65536, "root_bytes": 8388608,
                "expanded_bytes": 16777216, "json_depth": 32, "nodes": 1048576}}
    manifest = {"format": "permission-catalog-package-v2", "contract_revision": 3,
        "catalog_version": "synthetic-complete-package-v2" + ("-slots" if use_slots else ""), "semantics_version": "1",
        "identity_index_sha256": "70cb8771d93bb38788418156029f59f35eb0031f1a8ee3b8d900eb18b673f798",
        "source_heads": {"cli": "f8d434b6318215948ebbf67feb11f6265dee11e5", "endpoint": "5dfde6cfd56621b0fbcf9ec7f310c7caa5135f34", "server": "79701d4912eba79a8bcb02c58ed86609c38335e2"},
        "normalization": {"operation": "exact-id-v1", "resource": "first-fragment-only-v1", "origin": "exact-configured-v1", "discovery": "domain-and-validated-query-v1"},
        "limits": limits, "files": [{"path": path, "kind": "index" if path == "index.json" else path.split('/')[0].rstrip('s'), "bytes": len(raw), "sha256": digest(raw)} for path, raw in sorted(files.items())]}
    files["manifest.json"] = encode(manifest)
    uses = {h: 0 for h in roots}
    json_roots = set()
    for c in content.values():
        if c["kind"] in ("bytes", "template"):
            h = c.get("data_ref", c.get("value_ref")); uses[h] += 1
            if c["media_type"] == "application/json": json_roots.add(h)
        elif c["kind"] == "discovery_index":
            for row in c["rows"]: h = rows[row]["row_ref"]; uses[h] += 1; json_roots.add(h)
    metadata_nodes = sum(nodes(json.loads(b)) for path, b in files.items() if not path.endswith('.bin'))
    j = {h: nodes(json.loads(raw_roots[h])) if h in json_roots else 0 for h in roots}
    pre_b = sum(max(1, uses[h]) * len(raw_roots[h]) for h in roots)
    pre_n = metadata_nodes + sum(max(1, uses[h]) * j[h] for h in roots)
    post_b, post_n = pre_b, pre_n
    for h, output in [(template_root, rendered), (static_root, static_rendered)]:
        post_b += len(output) - len(raw_roots[h])
        post_n += nodes(json.loads(output)) - j[h]
    max_depth = max([depth(json.loads(raw)) for path, raw in files.items() if not path.endswith('.bin')]
                    + [depth(json.loads(raw_roots[h])) for h in json_roots]
                    + [depth(json.loads(rendered)), depth(json.loads(static_rendered))])
    ledger = {"status": "SYNTHETIC_CONFORMANCE_NOT_A_SUPPORTED_RELEASE", "manifest_sha256": digest(files['manifest.json']),
              "files": len(files), "entries": 827, "profiles": 10, "truths": 393, "base_schema_variants": 1179,
              "schema_cells": 56592, "discovery_cells": 156, "static_cells": 1248,
              "stored_bytes": sum(map(len, files.values())), "metadata_nodes": metadata_nodes,
              "pre_bytes": pre_b, "post_bytes": post_b, "pre_nodes": pre_n, "post_nodes": post_n,
              "max_depth": max_depth,
              "parts": sum(len(r['parts']) for r in roots.values()), "roots": len(roots), "shared_rows": len(rows), "content_records": len(content),
              "raw_template_sha256": template_root, "rendered_template_sha256": digest(rendered)}
    suffix = "-slots" if use_slots else ""
    with (OUT / ("package-v2-complete" + suffix + ".tar.gz")).open('wb') as f:
        with gzip.GzipFile(fileobj=f, mode='wb', mtime=0, filename='') as zipped:
            with tarfile.open(fileobj=zipped, mode='w', format=tarfile.USTAR_FORMAT) as tar:
                for path, raw in sorted(files.items()):
                    info = tarfile.TarInfo(path); info.mode = 0o600; info.size = len(raw)
                    tar.addfile(info, io.BytesIO(raw))
    (OUT / ("package-v2-ledger" + suffix + ".json")).write_bytes(encode(ledger) + b'\n')
    print(json.dumps(ledger, indent=2))


if __name__ == "__main__":
    build()
    build(True)
