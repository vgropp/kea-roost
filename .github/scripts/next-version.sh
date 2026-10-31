#!/bin/sh
# Prints the next version from the commits since the last release, or nothing if
# they hold no feat or fix. Before 1.0 a breaking change raises the minor version.
set -eu

first=v0.8.0
last=$(git tag --list 'v[0-9]*.[0-9]*.[0-9]*' --sort=-v:refname | head -n 1)
if [ -z "$last" ]; then
  echo "$first"
  exit 0
fi

subjects=$(git log "$last..HEAD" --format=%s)
bodies=$(git log "$last..HEAD" --format=%b)
major=$(echo "$last" | sed -E 's/^v([0-9]+)\..*/\1/')
minor=$(echo "$last" | sed -E 's/^v[0-9]+\.([0-9]+)\..*/\1/')
patch=$(echo "$last" | sed -E 's/^v[0-9]+\.[0-9]+\.([0-9]+)$/\1/')

if echo "$subjects" | grep -Eq '^[a-z]+(\([^)]*\))?!:' || echo "$bodies" | grep -q '^BREAKING CHANGE'; then
  if [ "$major" -eq 0 ]; then
    echo "v0.$((minor + 1)).0"
  else
    echo "v$((major + 1)).0.0"
  fi
elif echo "$subjects" | grep -Eq '^feat(\([^)]*\))?:'; then
  echo "v$major.$((minor + 1)).0"
elif echo "$subjects" | grep -Eq '^fix(\([^)]*\))?:'; then
  echo "v$major.$minor.$((patch + 1))"
fi
