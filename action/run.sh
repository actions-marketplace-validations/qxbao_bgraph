#!/usr/bin/env bash
# Runs bgraph for the composite action (action.yml) and writes the step outputs.
#
# Environment: BGRAPH (binary), MODE (command | affected | audit | record), BASE (optional), ROOT, ARGS (extra flags,
# split on whitespace), RECORDS (optional records file: written by mode record, imported before the other modes),
# EVENT_NAME, BASE_REF, BEFORE, MERGE_GROUP_BASE.
set -euo pipefail

err() { echo "::error title=bgraph::$*"; exit 1; }
native() {
  if [ "${RUNNER_OS:-}" = "Windows" ] && command -v cygpath >/dev/null 2>&1; then cygpath -w "$1"; else printf '%s' "$1"; fi
}
posix() {
  if [ "${RUNNER_OS:-}" = "Windows" ] && command -v cygpath >/dev/null 2>&1; then cygpath -u "$1"; else printf '%s' "$1"; fi
}

bin=$(posix "${BGRAPH:-bgraph}")
mode=${MODE:-command}
root=${ROOT:-.}
case "$mode" in
  command | affected | audit | record) ;;
  *) err "invalid mode: '$mode' (want command, affected, audit or record)" ;;
esac

if [ "$mode" != "record" ] && [ "$(git -C "$root" rev-parse --is-shallow-repository 2>/dev/null || echo false)" = "true" ]; then
  echo "::warning title=bgraph::shallow clone: the base commit may be missing and the selection is then 'full'. Check out with actions/checkout and fetch-depth: 0."
fi

zero=0000000000000000000000000000000000000000
base=${BASE:-}
if [ "$mode" = "record" ]; then
  : # records the checked-out commit: no base
elif [ -z "$base" ]; then
  case "${EVENT_NAME:-}" in
    pull_request | pull_request_target)
      base="origin/${BASE_REF:-}"
      ;;
    merge_group)
      base=${MERGE_GROUP_BASE:-HEAD~1}
      ;;
    *)
      # push: everything since the commit before the push (all of its commits); otherwise, or for a new branch or a
      # rewritten history whose old head is gone, the last commit only.
      if [ -n "${BEFORE:-}" ] && [ "$BEFORE" != "$zero" ] && git -C "$root" cat-file -e "${BEFORE}^{commit}" 2>/dev/null; then
        base=$BEFORE
      else
        base=HEAD~1
      fi
      ;;
  esac
fi
[ "$mode" = "record" ] || echo "bgraph $mode: base $base"

extra=()
if [ -n "${ARGS:-}" ]; then
  read -r -a extra <<<"$ARGS"
fi

tmp="$(posix "${RUNNER_TEMP:-${TMPDIR:-/tmp}}")"
mkdir -p "$tmp"
out=$(mktemp -d "$tmp/bgraph-XXXXXX") # one directory per invocation: several steps may use the action
json="$out/bgraph.json"
script="$out/commands.sh"
: >"$script"
scope=""
commands=""
clean=""
records=""
if [ -n "${RECORDS:-}" ]; then
  records=$(posix "$RECORDS")
fi

# Records made elsewhere (mode record on the default branch) feed the dynamic layer of the other modes. They are
# optional: without them nothing is narrowed, which is always safe, so a missing or unreadable file is not an error.
if [ "$mode" != "record" ] && [ -n "$records" ]; then
  if [ -f "$records" ]; then
    "$bin" dynamic import --root "$root" "$records" ||
      echo "::warning title=bgraph::cannot import the records in $RECORDS; the dynamic layer will not narrow"
  else
    echo "::notice title=bgraph::no records file at $RECORDS (nothing to import)"
  fi
fi

code=0
case "$mode" in
  command)
    "$bin" command --root "$root" --base "$base" ${extra[@]+"${extra[@]}"} >"$out/commands.txt" || code=$?
    [ "$code" -eq 0 ] || err "bgraph command failed (exit $code)"
    "$bin" command --root "$root" --base "$base" --json ${extra[@]+"${extra[@]}"} >"$json" || code=$?
    [ "$code" -eq 0 ] || err "bgraph command --json failed (exit $code)"
    cat "$out/commands.txt"
    scope=$(sed -n 's/^# scope: *//p' "$out/commands.txt" | head -n 1 | tr -d '\r')
    commands=$(grep -v '^#' "$out/commands.txt" | grep -v '^[[:space:]]*$' | tr -d '\r' || true)
    { echo "set -e"; [ -z "$commands" ] || printf '%s\n' "$commands"; } >"$script"
    ;;
  affected)
    "$bin" affected --root "$root" --base "$base" --json ${extra[@]+"${extra[@]}"} >"$json" || code=$?
    [ "$code" -eq 0 ] || err "bgraph affected failed (exit $code)"
    ;;
  audit)
    "$bin" audit --root "$root" --base "$base" --head HEAD --json ${extra[@]+"${extra[@]}"} >"$json" || code=$?
    case "$code" in
      0) ;;
      3) echo "::warning title=bgraph audit::a failed test lies outside the selection (or, with --strict, could not be mapped); see $(native "$json")" ;;
      *) err "bgraph audit failed (exit $code)" ;;
    esac
    cat "$json"
    ;;
  record)
    [ -n "$records" ] || records="$out/records.jsonl"
    mkdir -p "$(dirname "$records")"
    records="$(cd "$(dirname "$records")" && pwd)/$(basename "$records")"
    "$bin" record --root "$root" --out "$records" --json ${extra[@]+"${extra[@]}"} >"$json" || code=$?
    [ "$code" -eq 0 ] || err "bgraph record failed (exit $code)"
    cat "$json"
    ;;
esac

# Top-level fields of the JSON output (jq when available; the output is indented by two spaces otherwise).
field() {
  if command -v jq >/dev/null 2>&1; then
    jq -r --arg k "$1" '.[$k] // empty | tostring' "$json" | tr -d '\r'
  else
    sed -n "s/^  \"$1\": \"\{0,1\}\([^\",]*\)\"\{0,1\},\{0,1\}\$/\1/p" "$json" | head -n 1 | tr -d '\r'
  fi
}
full=""
if [ "$mode" != "record" ]; then
  if [ -z "${scope:-}" ]; then
    scope=$(field scope)
  fi
  [ -n "$scope" ] || err "no scope in the bgraph output"
  [ "$mode" = "audit" ] && clean=$(field clean)
  full=false
  [ "$scope" = "full" ] && full=true
fi

delim="bgraph_$(date +%s)_$RANDOM$RANDOM"
{
  echo "scope=$scope"
  echo "full=$full"
  echo "json=$(native "$json")"
  echo "script=$(native "$script")"
  echo "clean=$clean"
  if [ "$mode" = "record" ]; then echo "records=$(native "$records")"; else echo "records="; fi
  echo "commands<<$delim"
  [ -z "$commands" ] || printf '%s\n' "$commands"
  echo "$delim"
} >>"$GITHUB_OUTPUT"

if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
  {
    echo "### bgraph ($mode)"
    echo
    if [ "$mode" = "record" ]; then
      echo "Recorded $(field recorded) of $(field units) units: $(field complete) complete, $(field incomplete) incomplete, $(field failed) failed (records: \`$(native "$records")\`)"
    else
      echo "Scope: \`$scope\` (base \`$base\`)"
      if [ "$full" = "true" ]; then
        echo
        echo "The full test suite must run."
      elif [ -n "$commands" ]; then
        echo
        echo '```sh'
        printf '%s\n' "$commands"
        echo '```'
      fi
      [ -z "$clean" ] || { echo; echo "Audit clean: \`$clean\`"; }
    fi
  } >>"$GITHUB_STEP_SUMMARY"
fi
