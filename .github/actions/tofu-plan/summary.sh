#!/usr/bin/env bash
# Renders a Markdown summary (table + collapsible details) for an OpenTofu plan.
#
# Usage: summary.sh <plan.json | -> <text-file> <out.md> <title> <ok|failed>
#   ok     : <plan.json> is `tofu show -json`, <text-file> is `tofu show -no-color`
#   failed : <text-file> is the raw tofu output containing the error
#
# Prints add=, change= and destroy= lines on stdout for $GITHUB_OUTPUT.
set -euo pipefail

plan_json=$1
text_file=$2
out=$3
title=$4
status=$5

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
max_list=300
max_text=20000

header() {
  printf '## 🧱 OpenTofu plan: `%s`\n\n' "$title"
}

footer() {
  local sha=${TOFU_SOURCE_SHA:-$GITHUB_SHA}
  printf '<sub>Commit `%s` · [workflow run](%s/%s/actions/runs/%s)</sub>\n' \
    "${sha:0:7}" "${GITHUB_SERVER_URL:-https://github.com}" "${GITHUB_REPOSITORY:-}" "${GITHUB_RUN_ID:-}"
}

if [ "$status" != ok ]; then
  {
    header
    printf '> ❌ **Plan failed.** See the error output below.\n\n'
    printf '<details open>\n<summary>Error output</summary>\n\n````text\n'
    tail -n 200 "$text_file"
    printf '\n````\n\n</details>\n\n'
    footer
  } >"$out"
  exit 0
fi

counts=$(jq -c --arg mode counts -f "$here/changes.jq" "$plan_json")
n() { jq -r --arg k "$1" '.[$k] // 0' <<<"$counts"; }

create=$(n create)
update=$(n update)
delete=$(n delete)
replace=$(n replace)
other=$(n other)

add=$((create + replace))
change=$update
destroy=$((delete + replace))

row() {
  if [ "$2" -gt 0 ]; then printf '| %s | %s |\n' "$1" "$2"; fi
  return 0
}

section() {
  local kind=$1 label=$2 count=$3
  if [ "$count" -eq 0 ]; then return 0; fi
  printf '<details>\n<summary><b>%s (%s)</b></summary>\n\n```text\n' "$label" "$count"
  jq -r --arg mode "$kind" -f "$here/changes.jq" "$plan_json" | sed -n "1,${max_list}p"
  if [ "$count" -gt "$max_list" ]; then printf '… and %s more\n' "$((count - max_list))"; fi
  printf '```\n\n</details>\n\n'
  return 0
}

{
  header
  if [ $((create + update + delete + replace + other)) -eq 0 ]; then
    printf '> ✅ **No changes.** The infrastructure matches the configuration.\n\n'
  else
    printf '> **Plan:** %s to add, %s to change, %s to destroy.\n\n' "$add" "$change" "$destroy"
    printf '| Action | Count |\n| :-- | --: |\n'
    row '➕ Create' "$create"
    row '🔄 Update' "$update"
    row '♻️ Replace' "$replace"
    row '🗑️ Delete' "$delete"
    row '❓ Other' "$other"
    printf '\n'
    section create '➕ Create' "$create"
    section update '🔄 Update' "$update"
    section replace '♻️ Replace' "$replace"
    section delete '🗑️ Delete' "$delete"
    section other '❓ Other' "$other"
  fi

  if [ -s "$text_file" ]; then
    printf '<details>\n<summary>📄 Full plan output</summary>\n\n````hcl\n'
    head -c "$max_text" "$text_file"
    if [ "$(wc -c <"$text_file")" -gt "$max_text" ]; then
      printf '\n… truncated, see the workflow logs for the full plan …'
    fi
    printf '\n````\n\n</details>\n\n'
  fi
  footer
} >"$out"

printf 'add=%s\nchange=%s\ndestroy=%s\n' "$add" "$change" "$destroy"
