#!/usr/bin/env bash
#
# Rewrite go source package paths in a forked repository.

oldpkg="$1" && shift
newpkg="$1" && shift

if [[ $# -eq 0 ]]; then
  echo >&2 "Usage: $0 <old package> <new package> [path path ....]"
  exit 1
fi

echo >&2 "Rewriting $oldpkg into $newpkg at $@"
find "$@" -name '*.go' -print | \
  while read f; do
    # echo >&2 "Rewriting $f"
    sed -i "s,$oldpkg,$newpkg,g" "$f"
  done
