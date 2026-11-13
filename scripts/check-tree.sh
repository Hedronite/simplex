#!/bin/sh
# Gate the tracked tree: the private overlay is never tracked, and no address,
# hostname or secret reaches a public file. The allowlist is exactly the one
# frozen in docs/semantics.md section 14.
#
# Two implementation rules, both learned the hard way:
#  - Findings are collected in a file, not a variable, so a finding raised
#    inside a pipeline still fails the run.
#  - The file list is read line by line with IFS=, not word-split, so a tracked
#    path containing a space is scanned as one path. A path containing a
#    newline is not representable in this list; the parser refuses one below.
set -eu

cd "$(dirname "$0")/.."

list=$(mktemp)
findings=$(mktemp)
trap 'rm -f "$list" "$findings"' EXIT

record() { printf '%s\n' "$*" >>"$findings"; }

git ls-files >"$list"

# A newline in a tracked path would be read as two paths, and the gate would
# silently scan a partial name. Count both separators and compare: one newline
# per path means no name contains one.
n_nul=$(git ls-files -z | tr -cd '\0' | wc -c | tr -d ' ')
n_line=$(wc -l <"$list" | tr -d ' ')
if [ "$n_nul" -ne "$n_line" ]; then
	record "a tracked path contains a newline; the gate cannot scan it safely"
fi

count=0

while IFS= read -r f; do
	[ -n "$f" ] || continue

	# 0. The prose contract has one home. A file named semantics.md anywhere
	#    else is a copy that drifts (it already did once), so it fails.
	lower=$(printf '%s' "$f" | tr '[:upper:]' '[:lower:]')
	case "$lower" in
	semantics.md|*/semantics.md)
		[ "$f" = "docs/semantics.md" ] || record "semantics.md is only allowed at docs/semantics.md: $f" ;;
	esac

	# 1. The overlay is private user data and never tracked. A tracked file fails
	#    if its base name is simplex.overlay.yaml, ends in .overlay.yaml,
	#    .overlay.yml or .overlay.json, or is exactly overlay.yaml, overlay.yml
	#    or overlay.json, anywhere (testdata included); anything under .simplex/
	#    fails too. The overlay schema is allowed by its exact path. Code and
	#    test files may carry the word (internal/overlay); the scrub below stays
	#    the second line of defence for overlay-shaped data under any other name.
	base=$(basename "$f" | tr '[:upper:]' '[:lower:]')
	case "$f" in
	schema/simplex.overlay.v0.schema.json) ;;
	.simplex/*|*/.simplex/*) record "overlay appears tracked: $f" ;;
	*)
		case "$base" in
		simplex.overlay.yaml|*.overlay.yaml|*.overlay.yml|*.overlay.json|overlay.yaml|overlay.yml|overlay.json)
			record "overlay appears tracked: $f" ;;
		esac ;;
	esac

	# 2. Scrub. The pattern list mirrors docs/semantics.md section 14.
	#    Allowed address literals: 127.0.0.0/8, 0.0.0.0, 255.255.255.255,
	#    100.64.0.0/10. TEST-NET is a warning, not a failure.
	if ! grep -Iq . "$f" 2>/dev/null; then
		continue
	fi
	count=$((count + 1))

	grep -nEo '([0-9]{1,3}\.){3}[0-9]{1,3}(/([0-9]|[12][0-9]|3[0-2]))?' "$f" 2>/dev/null |
		while IFS=: read -r line match; do
			case "$match" in
			100.64.0.0/10|127.0.0.1/32|0.0.0.0/0) continue ;;
			esac
			host=${match%%/*}
			o1=$(printf '%s' "$host" | cut -d. -f1)
			o2=$(printf '%s' "$host" | cut -d. -f2)
			o3=$(printf '%s' "$host" | cut -d. -f3)
			o4=$(printf '%s' "$host" | cut -d. -f4)
			if [ "$o1" = 127 ]; then continue; fi
			if [ "$host" = 0.0.0.0 ] || [ "$host" = 255.255.255.255 ]; then continue; fi
			case "$o1.$o2.$o3" in
			192.0.2|198.51.100|203.0.113)
				printf 'warn %s:%s: test-net placeholder %s\n' "$f" "$line" "$match"
				continue
				;;
			esac
			if [ "$o1" -gt 255 ] 2>/dev/null || [ "$o2" -gt 255 ] 2>/dev/null ||
				[ "$o3" -gt 255 ] 2>/dev/null || [ "$o4" -gt 255 ] 2>/dev/null; then
				continue
			fi
			record "$f:$line: ipv4 $match"
		done

	for pat in \
		'[fF][dD]7[aA]:115[cC]:[aA]1[eE]0' \
		'[a-z0-9-]+\.(ts\.net|tailnet)' \
		'[a-z0-9-]+\.(local|lan)\b' \
		'-----BEGIN [A-Z ]*PRIVATE KEY-----' \
		'(ghp|gho|ghu|ghs|ghr)_[A-Za-z0-9]{20,}' \
		'github_pat_[A-Za-z0-9_]{20,}' \
		'AKIA[0-9A-Z]{16}' \
		'xox[abps]-[A-Za-z0-9-]{10,}' \
		'sk-[A-Za-z0-9]{20,}' \
		'[Bb]earer [A-Za-z0-9._~+/-]{20,}=*' \
		'eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.' \
		'/(Users|home)/[A-Za-z][A-Za-z0-9._-]*'
	do
		grep -nE "$pat" "$f" 2>/dev/null |
			while IFS=: read -r line text; do
				record "$f:$line: $text"
			done
	done

	# Emails must be placeholders.
	grep -nEo '[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}' "$f" 2>/dev/null |
		while IFS=: read -r line addr; do
			case "$addr" in
			*@example.com|*@example.org|*@users.noreply.github.com) continue ;;
			esac
			record "$f:$line: email $addr"
		done
done <"$list"

if [ -s "$findings" ]; then
	sed 's/^/FAIL /' "$findings"
	printf 'FAIL tracked tree: %s finding(s)\n' "$(wc -l <"$findings" | tr -d ' ')"
	exit 1
fi

printf 'PASS tracked tree: %s text files, no tracked overlay, no addresses, hostnames or secrets\n' "$count"
