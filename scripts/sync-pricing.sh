#!/usr/bin/env bash
# The Pi prices counter orders with main's pricing engine: agent/internal/domain/pricing
# is a byte copy of main/backend/internal/domain/pricing (its .go files + testdata/cases),
# with one change: the menuview import points at the agent's own copy of the few view
# types the engine reads (agent/internal/domain/menuview).
#
#   scripts/sync-pricing.sh           copy main's package over the agent's (after main changes it)
#   scripts/sync-pricing.sh --check   exit 1 when the two differ (the release gate's step)
#
# MAIN_PRICING overrides where main's package is (default: ../main next to agent/).
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
src="${MAIN_PRICING:-$here/../main/backend/internal/domain/pricing}"
dst="$here/internal/domain/pricing"
from='"quiccpos/main/internal/domain/menuview"'
to='"quiccpos/agent/internal/domain/menuview"'

[ -d "$src" ] || { echo "sync-pricing: main's pricing package not found at $src" >&2; exit 2; }

# The expected copy, built in a temp dir: every file of main's package, .go files with
# the import rewritten.
want="$(mktemp -d)"
trap 'rm -rf "$want"' EXIT
(cd "$src" && find . -type f \( -name '*.go' -o -path './testdata/*' \) -print0) |
	while IFS= read -r -d '' f; do
		mkdir -p "$want/$(dirname "$f")"
		if [[ "$f" == *.go ]]; then
			sed "s#$from#$to#" "$src/$f" >"$want/$f"
		else
			cp "$src/$f" "$want/$f"
		fi
	done

if [ "${1:-}" = "--check" ]; then
	if [ ! -d "$dst" ] || ! diff -r -q "$want" "$dst" >/dev/null; then
		echo "sync-pricing: agent/internal/domain/pricing differs from main's pricing package:" >&2
		diff -r -q "$want" "${dst}" >&2 || true
		echo "run agent/scripts/sync-pricing.sh (then go test ./internal/domain/pricing)" >&2
		exit 1
	fi
	echo "sync-pricing: agent's pricing engine is in step with main's"
	exit 0
fi

rm -rf "$dst"
mkdir -p "$dst"
cp -r "$want/." "$dst/"
echo "sync-pricing: copied $(find "$dst" -type f | wc -l) files from $src"
