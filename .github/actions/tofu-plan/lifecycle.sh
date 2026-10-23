#!/usr/bin/env bash
# Shared reporting for controller-driven saved-plan lifecycles.
# Usage: lifecycle.sh <plan|apply|direct> <title> <report-prefix> <var-file|empty> <true|false>
set -euo pipefail

phase=$1
title=$2
prefix=$3
var_file=$4
destroy=$5
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
md="$prefix.md"
log="$prefix-$phase.log"
json="$prefix.json"
text="$prefix.txt"
umask 077
trap 'rm -f -- "$json" "$text" "$log"' EXIT

run() {
  echo "::group::$title: $*"
  local rc=0
  "$@" 2>&1 | tee -a "$log" || rc=$?
  echo "::endgroup::"
  return "$rc"
}

: > "$log"
case "$phase" in
  plan)
    rc=0
    planned=false
    run tofu init -input=false -no-color || rc=$?
    if [ "$rc" -eq 0 ]; then run tofu validate -no-color || rc=$?; fi
    if [ "$rc" -eq 0 ]; then
      args=(-input=false -no-color -detailed-exitcode -out=lifecycle.tfplan)
      if [ -n "$var_file" ]; then args+=("-var-file=$var_file"); fi
      if [ "$destroy" = true ]; then args+=("-destroy"); fi
      run tofu plan "${args[@]}" || rc=$?
      planned=true
    fi
    if [ "$planned" = true ] && { [ "$rc" -eq 0 ] || [ "$rc" -eq 2 ]; }; then
      if tofu show -json lifecycle.tfplan > "$json" && tofu show -no-color lifecycle.tfplan > "$text"; then
        bash "$here/summary.sh" "$json" "$text" "$md" "$title" ok
        bash "$here/log.sh" "$json" "$text" "$title: Plan Summary"
        cat "$md" >> "$GITHUB_STEP_SUMMARY"
        exit 0
      fi
    fi
    bash "$here/summary.sh" - "$log" "$md" "$title" failed
    cat "$md" >> "$GITHUB_STEP_SUMMARY"
    exit 1
    ;;
  apply|direct)
    rc=0
    args=(-input=false -no-color)
    if [ "$phase" = direct ]; then
      : > "$md"
      run tofu init -input=false -no-color || rc=$?
      if [ "$rc" -eq 0 ]; then run tofu validate -no-color || rc=$?; fi
      args+=(-auto-approve)
      if [ -n "$var_file" ]; then args+=("-var-file=$var_file"); fi
      if [ "$destroy" = true ]; then args+=(-destroy); fi
    else
      args+=(lifecycle.tfplan)
    fi
    if [ "$rc" -eq 0 ]; then run tofu apply "${args[@]}" || rc=$?; fi
    {
      printf '\n## OpenTofu apply: `%s`\n\n' "$title"
      if [ "$rc" -eq 0 ]; then
        printf '**Apply succeeded.**\n\n'
      else
        printf '**Apply failed. Check the state-persistence step before retrying.**\n\n'
      fi
      printf '<details%s>\n<summary>Apply log</summary>\n\n````text\n' "$([ "$rc" -ne 0 ] && echo ' open')"
      tail -n 300 "$log"
      printf '\n````\n\n</details>\n'
    } | tee -a "$md" >> "$GITHUB_STEP_SUMMARY"
    exit "$rc"
    ;;
  *)
    echo "::error::Unknown Terraform lifecycle phase: $phase"
    exit 1
    ;;
esac
