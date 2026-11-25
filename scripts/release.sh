#!/usr/bin/env bash
# Tag a release on the merge commit of a merged pull request.
#
#   scripts/release.sh <pr-number> <tag>...
#   scripts/release.sh 28 v1.9.1 kilncrypt/v0.2.1
#
# Refuses unless the PR is merged, each tag is new, and every module tag's
# go.mod requires a kiln version that exists (or is tagged in this run).
# Core tags (vX.Y.Z) also get a GitHub release with generated notes.
set -euo pipefail

die() { echo "release: $*" >&2; exit 1; }
[ $# -ge 2 ] || die "usage: scripts/release.sh <pr-number> <tag>..."
pr=$1; shift

state=$(gh pr view "$pr" --json state -q .state)
[ "$state" = MERGED ] || die "PR #$pr is $state, not MERGED: merge it first"
commit=$(gh pr view "$pr" --json mergeCommit -q .mergeCommit.oid)
git fetch -q origin --tags
git merge-base --is-ancestor "$commit" origin/main || die "merge commit $commit is not on origin/main"

core=""
for t in "$@"; do
  [[ "$t" =~ ^([a-z/]+/)?v[0-9]+\.[0-9]+\.[0-9]+$ ]] || die "bad tag $t"
  if git rev-parse -q --verify "refs/tags/$t" >/dev/null; then
    die "tag $t already exists (tags are immutable: use the next version)"
  fi
  [[ "$t" == */* ]] || core=$t
done
for t in "$@"; do
  [[ "$t" == */* ]] || continue
  dir=${t%/v*}
  need=$(git show "$commit:$dir/go.mod" |
    awk '$1=="require" && $2=="github.com/mhdaslam790/kiln" {print $3} $1=="github.com/mhdaslam790/kiln" {print $2}' | head -1)
  [ -n "$need" ] || continue
  if [ "$need" != "$core" ] && ! git rev-parse -q --verify "refs/tags/$need" >/dev/null; then
    die "$dir requires kiln $need, which is neither tagged nor being tagged now"
  fi
done

echo "tagging $(git log --oneline -1 "$commit")"
for t in "$@"; do
  git tag -a "$t" -m "${t%/v*} ${t##*/}" "$commit"
done
git push origin "$@"
if [ -n "$core" ]; then
  gh release create "$core" --title "kiln $core" --generate-notes --verify-tag
fi
echo "done: $*"
