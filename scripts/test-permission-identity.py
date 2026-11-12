#!/usr/bin/env python3
"""Run synthetic identity/legacy tests without the DB-backed package TestMain."""
from pathlib import Path
import subprocess

repo = Path(__file__).resolve().parents[1]
package = repo / "modules/user"
files = sorted(str(p.relative_to(repo)) for p in package.glob("*.go")
               if not p.name.endswith("_test.go"))
files += [str((package / name).relative_to(repo)) for name in (
    "api_i18n_test.go", "api_mcp_policy_test.go", "permission_identity_test.go")]
raise SystemExit(subprocess.run([
    "go", "test", *files, "-run",
    "Test(PermissionManagedIdentity|MCPVerifyBotCompatibility)", "-count=1"
], cwd=repo).returncode)
