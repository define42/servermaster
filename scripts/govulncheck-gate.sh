#!/usr/bin/env bash
#
# Run govulncheck and fail only on "called" advisories that are NOT listed in
# .github/govulncheck-allowlist.txt. Everything else that govulncheck reports as
# reachable from our code still fails. Shared by the `govulncheck` makefile
# target and the govulncheck CI workflow so local and CI behavior stay in sync.
#
# Override the govulncheck invocation with GOVULNCHECK (defaults to a pinned
# `go run`, which uses the module's Go toolchain so stdlib advisories are judged
# against the version declared in go.mod).
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
allowlist="$repo_root/.github/govulncheck-allowlist.txt"
govulncheck="${GOVULNCHECK:-go run golang.org/x/vuln/cmd/govulncheck@latest}"

group()    { if [ "${GITHUB_ACTIONS:-}" = "true" ]; then echo "::group::$1"; else echo "== $1 =="; fi; }
endgroup() { if [ "${GITHUB_ACTIONS:-}" = "true" ]; then echo "::endgroup::"; fi; }

# Human-readable report for the log. Informational only; the gate below decides
# whether we pass. govulncheck exits 3 when vulns are called, so guard with `|| true`.
group "govulncheck report"
$govulncheck ./... || true
endgroup

# Machine-readable run for gating.
json="$($govulncheck -format json ./... || true)"

# Allowlisted advisory IDs (one ID per line; inline "# ..." comments ignored).
allow_json="$(grep -vE '^[[:space:]]*(#|$)' "$allowlist" | awk '{print $1}' | jq -R . | jq -s .)"
echo "Allowlisted advisories: $(echo "$allow_json" | jq -c .)"

# Advisory IDs reported as *called* by our code (symbol-level findings have a
# function in the topmost trace frame), minus the allowlist.
unexpected="$(printf '%s' "$json" | jq -s --argjson allow "$allow_json" '
  [ .[] | select(has("finding")) | .finding
    | select(.trace[0].function != null) | .osv ]
  | unique
  | map(select(. as $id | $allow | index($id) | not))
')"

if [ "$(echo "$unexpected" | jq 'length')" -ne 0 ]; then
  {
    echo "govulncheck found called vulnerabilities that are not allowlisted:"
    echo "$unexpected" | jq -r '.[] | "  - https://pkg.go.dev/vuln/\(.)"'
    echo "Either upgrade to a fixed version, or add the ID to"
    echo "  .github/govulncheck-allowlist.txt  with a justification."
  } >&2
  exit 1
fi

echo "No un-allowlisted called vulnerabilities found."
