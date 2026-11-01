#!/bin/sh
# Prints the notes of the next release from the commit subjects since the last one.
# The first argument is the next version, for the link to all changes.
set -eu

last=$(git tag --list 'v[0-9]*.[0-9]*.[0-9]*' --sort=-v:refname | head -n 1)
subjects=$(git log "${last:+$last..}HEAD" --no-merges --format='%s (%h)')
dependencies=$(printf '%s\n' "$subjects" | grep -E '^[a-z]+\(deps\)!?:' || true)
others=$(printf '%s\n' "$subjects" | grep -vE '^[a-z]+\(deps\)!?:' || true)

section() {
  lines=$(printf '%s\n' "$2" | sed -nE "s/^$3:[[:space:]]*/- /p")
  if [ -n "$lines" ]; then
    printf '## %s\n\n%s\n\n' "$1" "$lines"
  fi
}

section 'Breaking changes' "$others" '[a-z]+(\([^)]*\))?!'
section 'Features' "$others" 'feat(\([^)]*\))?'
section 'Fixes' "$others" 'fix(\([^)]*\))?'
section 'Dependencies' "$dependencies" '[a-z]+\(deps\)!?'
section 'Documentation' "$others" 'docs?(\([^)]*\))?'

if [ -n "$last" ]; then
  printf '**Full changelog:** https://github.com/%s/compare/%s...%s\n' "${GITHUB_REPOSITORY:-}" "$last" "${1:-HEAD}"
fi
