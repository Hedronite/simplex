#!/usr/bin/env bash
# Grafana and Prometheus smoke test for the checked-in dashboard.
#
# What it proves, in the order it proves it:
#
#   1. Grafana, pinned by digest, provisions the provider file and the dashboard
#      from files, and serves the dashboard under its frozen uid with every
#      panel using the datasource uid the repo fixes.
#   2. Prometheus, pinned by digest, scrapes a static exposition built from a
#      fixture's golden, and every panel expression returns data from it.
#   3. The vantage selectors discriminate: a status the fixture does not have
#      returns nothing, which is what makes the blind panel meaningful.
#
# The expressions come from the dashboard JSON itself, so there is no second
# list to drift. Grafana template variables are substituted the way Grafana
# would: `$__rate_interval` becomes a fixed window and the label variables
# become `.*`.
#
# Everything runs in containers on the runner's host network. No secret is
# used, nothing outside the runner is contacted, and every container and helper
# process is removed on exit, including on failure.
set -euo pipefail

cd "$(dirname "$0")/.."

GRAFANA_IMAGE=${GRAFANA_IMAGE:-"grafana/grafana@sha256:7b22f3edf947329d75eba7a62d9f6ea1ac1e786736813ef7a98cfb275a52e49b"}
PROMETHEUS_IMAGE=${PROMETHEUS_IMAGE:-"prom/prometheus@sha256:86b17a25c2db1d16a61b16b3c8f336679eb19e26333d03e808da387206e40faa"}

DASHBOARD=grafana/dashboards/simplex.json
PROVIDER=grafana/provisioning/dashboards/simplex.yaml
DASHBOARD_UID=simplex
PROM_UID=lea-prom
GRAFANA_PORT=3000
PROM_PORT=9090
EXPO_PORT=9109

# The fixture the exposition and the expectations come from: a RED pair, so the
# vantage is unchecked and every family has data.
FIXTURE=both-prom-down

WORK=$(mktemp -d)
CONTAINERS=()
EXPO_PID=""

cleanup() {
	status=$?
	if [ "$status" -ne 0 ]; then
		for c in "${CONTAINERS[@]}"; do
			echo "--- docker logs $c" >&2
			docker logs "$c" 2>&1 | tail -40 >&2 || true
		done
	fi
	for c in "${CONTAINERS[@]}"; do
		docker rm -f "$c" >/dev/null 2>&1 || true
	done
	if [ -n "$EXPO_PID" ]; then
		kill "$EXPO_PID" >/dev/null 2>&1 || true
	fi
	rm -rf "$WORK"
}
trap cleanup EXIT

fail() {
	echo "FAIL: $*" >&2
	exit 1
}
note() { echo "== $*"; }

# docker_run starts a detached container and remembers it for cleanup.
docker_run() {
	name=$1
	shift
	docker run -d --name "$name" "$@" >/dev/null
	CONTAINERS+=("$name")
}

# wait_http polls a URL until it answers, or fails after the given seconds.
wait_http() {
	url=$1
	seconds=$2
	for _ in $(seq 1 "$seconds"); do
		if curl -fsS -o /dev/null "$url" 2>/dev/null; then
			return 0
		fi
		sleep 1
	done
	fail "$url did not answer within ${seconds}s"
}

note "fixture $FIXTURE: its document sets the expectation, its golden is the exposition"
cp "testdata/$FIXTURE/document.json" "$WORK/document.json"
mkdir -p "$WORK/served"
cp "internal/metrics/testdata/$FIXTURE.txt" "$WORK/served/metrics"

note "static exposition on 127.0.0.1:$EXPO_PORT"
python3 -m http.server "$EXPO_PORT" --directory "$WORK/served" >"$WORK/expo.log" 2>&1 &
EXPO_PID=$!
wait_http "http://127.0.0.1:$EXPO_PORT/metrics" 15

note "Prometheus on 127.0.0.1:$PROM_PORT"
cat >"$WORK/prometheus.yml" <<YAML
global:
  scrape_interval: 1s
  evaluation_interval: 1s
scrape_configs:
  - job_name: simplex
    static_configs:
      - targets: ["127.0.0.1:$EXPO_PORT"]
YAML
docker_run simplex-prom \
	--network host \
	-v "$WORK/prometheus.yml:/etc/prometheus/prometheus.yml:ro" \
	"$PROMETHEUS_IMAGE" \
	--config.file=/etc/prometheus/prometheus.yml \
	--web.listen-address="127.0.0.1:$PROM_PORT"
wait_http "http://127.0.0.1:$PROM_PORT/-/ready" 60

note "Grafana on 127.0.0.1:$GRAFANA_PORT, provisioning from files"
mkdir -p "$WORK/provisioning/dashboards" "$WORK/provisioning/datasources" "$WORK/dashboards"
cp "$PROVIDER" "$WORK/provisioning/dashboards/simplex.yaml"
cp "$DASHBOARD" "$WORK/dashboards/simplex.json"
cat >"$WORK/provisioning/datasources/simplex.yaml" <<YAML
apiVersion: 1
datasources:
  - name: Prometheus (lea-lathe)
    uid: $PROM_UID
    type: prometheus
    access: proxy
    url: http://127.0.0.1:$PROM_PORT
    isDefault: true
YAML
# The provider file the repo ships points at /var/lib/grafana/dashboards, and the
# checked-in dashboard is mounted exactly there, so the real path is under test
# rather than a rewritten one.
docker_run simplex-grafana \
	--network host \
	-e GF_SECURITY_ADMIN_PASSWORD=throwaway-ci-only \
	-e GF_AUTH_ANONYMOUS_ENABLED=false \
	-v "$WORK/provisioning:/etc/grafana/provisioning:ro" \
	-v "$WORK/dashboards:/var/lib/grafana/dashboards:ro" \
	"$GRAFANA_IMAGE"
wait_http "http://127.0.0.1:$GRAFANA_PORT/api/health" 90

note "the dashboard is served under its frozen uid, from the provider path"
curl -fsS -u "admin:throwaway-ci-only" \
	"http://127.0.0.1:$GRAFANA_PORT/api/dashboards/uid/$DASHBOARD_UID" >"$WORK/served-dashboard.json" ||
	fail "GET /api/dashboards/uid/$DASHBOARD_UID failed"

jq -e --arg uid "$DASHBOARD_UID" '.dashboard.uid == $uid' "$WORK/served-dashboard.json" >/dev/null ||
	fail "served dashboard uid is not $DASHBOARD_UID"
jq -e '.dashboard.title == "Simplex mesh health"' "$WORK/served-dashboard.json" >/dev/null ||
	fail "served dashboard title is not the frozen one"

served_panels=$(jq '.dashboard.panels | length' "$WORK/served-dashboard.json")
file_panels=$(jq '.panels | length' "$DASHBOARD")
[ "$served_panels" = "$file_panels" ] || fail "served $served_panels panels, the file has $file_panels"

note "every served panel uses the datasource uid the repo fixes"
bad_ds=$(jq -r --arg want "$PROM_UID" '
	[.. | objects | select(has("datasource")) | .datasource
	 | (if type == "object" then (.uid // "missing") else "string:" + tostring end)]
	| map(select(. != $want)) | unique | .[]' "$WORK/served-dashboard.json")
[ -z "$bad_ds" ] || fail "panels use datasources other than $PROM_UID: $bad_ds"

note "served panel titles match the file"
served_titles=$(jq -r '[.dashboard.panels[].title] | sort | join("|")' "$WORK/served-dashboard.json")
file_titles=$(jq -r '[.panels[].title] | sort | join("|")' "$DASHBOARD")
[ "$served_titles" = "$file_titles" ] || fail "served titles differ from the file"

# substitutions turns a Grafana expression into PromQL Prometheus accepts: the
# dashboard's variables widen to everything, and the rate interval becomes a
# window long enough for a one-second scrape.
substitutions() {
	sed -e 's/\${\?__rate_interval}\?/1m/g' \
		-e 's/\${\?host}\?/.*/g' \
		-e 's/\${\?kind}\?/.*/g'
}

# query_len returns the number of series one expression selects at this instant.
query_len() {
	local expr encoded
	expr=$1
	encoded=$(jq -rn --arg q "$expr" '$q | @uri')
	curl -fsS "http://127.0.0.1:$PROM_PORT/api/v1/query?query=$encoded&time=$(date +%s)" |
		jq -er 'if .status == "success" then (.data.result | length) else error(.error // "query failed") end'
}

note "waiting for the first scrape to land"
for _ in $(seq 1 30); do
	if [ "$(query_len 'simplex_build_info')" != "0" ]; then
		break
	fi
	sleep 1
done
[ "$(query_len 'simplex_build_info')" != "0" ] || fail "Prometheus never scraped the exposition"

note "each expression returns what the fixture implies"
VANTAGE=$(jq -r .vantage.status "$WORK/document.json")
checked=0
while IFS= read -r expr; do
	[ -n "$expr" ] || continue
	checked=$((checked + 1))
	len=$(query_len "$(printf '%s' "$expr" | substitutions)")

	# A vantage selector for a status this fixture does not have must return
	# nothing; that emptiness is the thing worth asserting.
	if printf '%s' "$expr" | grep -q 'simplex_vantage_status{status='; then
		status=$(printf '%s' "$expr" | sed -n 's/.*status="\([a-z]*\)".*/\1/p')
		if [ "$status" = "$VANTAGE" ]; then
			[ "$len" != "0" ] || fail "nothing returned where the fixture is $status: $expr"
		else
			[ "$len" = "0" ] || fail "data returned for a status the fixture does not have ($status): $expr"
		fi
		continue
	fi
	[ "$len" != "0" ] || fail "nothing returned: $expr"
done < <(jq -r '[.. | objects | select(has("expr")) | .expr] | unique | .[]' "$DASHBOARD")

[ "$checked" -ge 10 ] || fail "only $checked expressions were checked; the dashboard has at least ten"

note "PASS: Grafana provisioned the dashboard and every expression returns what the fixture implies"
