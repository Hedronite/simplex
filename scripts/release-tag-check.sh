#!/bin/sh
# release-tag-check.sh
#
# One commit can carry two tags: a release candidate and the final version are
# normally the same tree. goreleaser picks the tag with `git tag --points-at
# HEAD --sort=-version:refname`, so a run for the final tag builds and names the
# candidate's artifacts unless the tag is forced or git ranks the candidate
# below the final version. This puts both tags on one commit, built from the
# working tree, and proves the final tag wins both when it is forced and when it
# is only sorted. The sorted case repeats the workflow's own git setting, and
# fails if the workflow has stopped making it.
#
# Needs goreleaser on PATH; writes nothing outside a temporary directory and
# touches neither the network nor a release.
set -eu

fail() {
	printf 'FAIL %s\n' "$*" >&2
	exit 1
}
note() { printf '== %s\n' "$*"; }

goreleaser=${GORELEASER:-goreleaser}

# The workflow is the only place that sets this for a real release, so the check
# copies the line it depends on and fails if it has gone.
grep -q 'versionsort\.suffix -rc\.' .github/workflows/release.yml ||
	fail "release.yml no longer ranks prerelease tags below their final version"
version=0.0.1
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT INT TERM

# A repository built from the tracked tree, so the checkout's own tags cannot
# reach the fixture and the two tags below are the only ones in it.
origin=$(pwd)
mkdir -p "$work/repo"
cd "$work/repo"
git init -q .
# goreleaser parses the remote URL, so it has to name a real forge; nothing is
# fetched or pushed, because every run here skips publish.
git remote add origin "https://github.com/${GITHUB_REPOSITORY:-Hedronite/simplex}.git"
git config user.email "release-check"
git config user.name "release check"
git -C "$origin" archive HEAD | tar -x -C .
git add -A
git commit -q -m "fixture: one commit, two tags"
git tag "v$version-rc.1"
git tag "v$version"

count=$(git tag --points-at HEAD | wc -l | tr -d ' ')
[ "$count" = 2 ] || fail "the fixture put $count tag(s) on one commit, not 2"
note "two tags on one commit: $(git tag --points-at HEAD | tr '\n' ' ')"

built_tag() {
	[ -f dist/metadata.json ] || fail "no dist/metadata.json after the build"
	grep -o '"tag"[[:space:]]*:[[:space:]]*"[^"]*"' dist/metadata.json |
		sed 's/.*"\([^"]*\)"$/\1/' | head -n 1
}

named_candidate() {
	# shellcheck disable=SC2012 # the names are what is being read
	ls dist | grep -q -- "-rc\.1$"
}

note "forced to the final tag"
GORELEASER_CURRENT_TAG="v$version" "$goreleaser" release --clean --skip=publish,sbom,sign >/dev/null
[ "$(built_tag)" = "v$version" ] || fail "forced v$version but the build carried $(built_tag)"
if named_candidate; then fail "a run for v$version named an artifact after the candidate"; fi

note "sorted, with nothing forced"
# The value the workflow sets, repeated here so the sorted case is the one a
# release without the forced tag would see.
git config --global versionsort.suffix -rc.
"$goreleaser" release --clean --skip=publish,sbom,sign >/dev/null
[ "$(built_tag)" = "v$version" ] || fail "nothing forced, and the build carried $(built_tag)"
if named_candidate; then fail "nothing forced, and an artifact was named after the candidate"; fi

note "PASS the final tag wins over a candidate on the same commit"
