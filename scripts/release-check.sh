#!/bin/sh
# Release checks for the snapshot and release workflows.
#
# Two modes:
#
#   release-check.sh check DIST
#       The build is what a release must be: stamped from ldflags, no fallback
#       stamps, SBOMs that list the shipped modules and not the test-only ones.
#
#   release-check.sh compare DIR_A DIR_B
#       Two builds of one commit are byte-identical: the archive checksums match
#       and so does the checksums file, after path-independent comparison.
#
# Everything here reads the dist directory; nothing contacts the network, and
# nothing is published.
set -eu

fail() {
	printf 'FAIL %s\n' "$*" >&2
	exit 1
}
note() { printf '== %s\n' "$*"; }

mode=${1:-}
case "$mode" in
check | compare) ;;
*) fail "usage: $0 check DIST | compare DIR_A DIR_B" ;;
esac

# --- check -------------------------------------------------------------------

check_dist() {
	dist=$1
	[ -d "$dist" ] || fail "no dist directory at $dist"

	# The stamps and the module table are checked on a binary this host can run:
	# the native target when the build produced one, the linux/amd64 one
	# otherwise (which is the CI case). Both carry the same ldflags.
	goos=$(go env GOOS)
	goarch=$(go env GOARCH)
	bin=$(find "$dist" -type f -name simplex -path "*_${goos}_${goarch}*" | head -n 1)
	if [ -z "$bin" ]; then
		bin=$(find "$dist" -type f -name simplex -path '*linux_amd64*' | head -n 1)
	fi
	[ -n "$bin" ] || fail "no binary under $dist"
	note "checking $bin"

	note "version stamps"
	out=$("$bin" version --json) || fail "version --json failed"
	printf '%s\n' "$out"
	printf '%s' "$out" | grep -q '"version":"[0-9]' || fail "version is not a release version: $out"
	printf '%s' "$out" | grep -q '"version":"dev"' && fail "version fell back to dev"
	printf '%s' "$out" | grep -q '"commit":"unknown"' && fail "commit fell back to unknown"
	printf '%s' "$out" | grep -q '"built":"unknown"' && fail "built fell back to unknown"
	printf '%s' "$out" | grep -Eq '"commit":"[0-9a-f]{40}"' || fail "commit is not a 40-hex sha: $out"
	printf '%s' "$out" | grep -Eq '"built":"[0-9]{4}-[0-9]{2}-[0-9]{2}' || fail "built is not a date: $out"

	note "trimpath and module path"
	go version -m "$bin" >"$dist/.gover" || fail "go version -m failed"
	grep -q 'path	github.com/Hedronite/simplex/cmd/simplex' "$dist/.gover" ||
		fail "module path is not the release path"
	grep -q -- '-trimpath' "$dist/.gover" || fail "the binary was built without -trimpath"

	note "SBOMs"
	sboms=$(find "$dist" -type f -name '*.sbom.cdx.json' | sort)
	[ -n "$sboms" ] || fail "no SBOM under $dist"
	for sbom in $sboms; do
		[ -s "$sbom" ] || fail "empty SBOM: $sbom"
		grep -q '"bomFormat"' "$sbom" || fail "$sbom has no bomFormat"
		grep -q 'CycloneDX' "$sbom" || fail "$sbom is not CycloneDX"
	done
	first=$(printf '%s\n' "$sboms" | head -n 1)
	for shipped in spf13/cobra spf13/pflag go.yaml.in/yaml; do
		grep -q "$shipped" "$first" || fail "SBOM does not list $shipped"
	done
	for testonly in santhosh-tekuri/jsonschema prometheus/client_golang prometheus/common client_model; do
		if grep -q "$testonly" "$first"; then
			fail "SBOM lists the test-only module $testonly"
		fi
	done

	note "archive contents"
	for archive in "$dist"/*.tar.gz; do
		[ -f "$archive" ] || continue
		list=$(tar tzf "$archive")
		printf '%s\n' "$list" | grep -q 'LICENSE' || fail "$archive has no LICENSE"
		printf '%s\n' "$list" | grep -q 'example.yaml' || fail "$archive has no example.yaml"
		printf '%s\n' "$list" | grep -q 'grafana/dashboards/simplex.json' ||
			fail "$archive has no dashboard"
		printf '%s\n' "$list" | grep -q 'simplex$' || fail "$archive has no binary"
	done

	note "PASS: the build is release-shaped"
}

# --- compare -----------------------------------------------------------------

# hashes prints the archive checksums, sorted. SBOMs are excluded on purpose:
# syft stamps each document with a generation time and a document UUID, so two
# runs never produce identical bytes even for identical input. What has to be
# reproducible is what people download and verify, and that is the archive.
hashes() {
	dir=$1
	[ -d "$dir" ] || fail "no dist directory at $dir"
	(
		cd "$dir" || exit 1
		[ -f checksums.txt ] || fail "no checksums.txt in $dir"
		grep -E '  [^ ]+\.tar\.gz$' checksums.txt | sort
	)
}

compare_dists() {
	a=$1
	b=$2
	hashes "$a" >"$a.hashes"
	hashes "$b" >"$b.hashes"
	if ! diff -u "$a.hashes" "$b.hashes"; then
		fail "the two builds differ; the release is not reproducible"
	fi
	note "PASS: $(wc -l <"$a.hashes" | tr -d ' ') checksums match across builds"
}

case "$mode" in
check) shift; check_dist "${1:?dist directory}" ;;
compare) shift; compare_dists "${1:?first dist}" "${2:?second dist}" ;;
esac
