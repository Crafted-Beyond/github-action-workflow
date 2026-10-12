#!/usr/bin/env bash
# Prints a plan summary to the job log with collapsible groups.
#
# Usage: log.sh <plan.json> <plan.txt> [heading]
set -euo pipefail

plan_json=$1
plan_txt=$2
heading=${3:-Plan Summary}

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
counts=$(jq -c --arg mode counts -f "$here/changes.jq" "$plan_json")
n() { jq -r --arg k "$1" '.[$k] // 0' <<<"$counts"; }

echo "=> $heading"
echo
echo "+---------+-------+"
echo "| Action  | Count |"
echo "+---------+-------+"
total=0
for kind in create update replace delete other; do
  c=$(n "$kind")
  total=$((total + c))
  if [ "$c" -gt 0 ]; then
    label=$(printf '%s' "${kind:0:1}" | tr '[:lower:]' '[:upper:]')${kind:1}
    printf '| %-7s | %5s |\n' "$label" "$c"
    echo "+---------+-------+"
  fi
done
if [ "$total" -eq 0 ]; then
  echo "| No changes        |"
  echo "+-------------------+"
fi

for kind in create update replace delete other; do
  c=$(n "$kind")
  if [ "$c" -gt 0 ]; then
    label=$(printf '%s' "${kind:0:1}" | tr '[:lower:]' '[:upper:]')${kind:1}
    echo "::group::$label ($c)"
    jq -r --arg mode "$kind" -f "$here/changes.jq" "$plan_json"
    echo "::endgroup::"
  fi
done

if [ -s "$plan_txt" ]; then
  echo "::group::Details"
  cat "$plan_txt"
  echo "::endgroup::"
fi
