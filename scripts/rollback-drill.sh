#!/usr/bin/env bash
# Controlled production rollback drill.
#
# Promotes the previous READY production deployment, verifies /healthz and the
# contract smoke, then promotes the original deployment back and verifies it.
# This exercises the same API path deploy.yml uses on a post-promotion failure.
#
# Requirements:
#   VERCEL_TOKEN, VERCEL_PROJECT_ID, PROD_URL
#   jq, go, curl
#   CONFIRM=ROLLBACK-DRILL
set -euo pipefail
cd "$(dirname "$0")/.."

: "${VERCEL_TOKEN:?VERCEL_TOKEN is required}"
: "${VERCEL_PROJECT_ID:?VERCEL_PROJECT_ID is required}"
: "${PROD_URL:=https://agents-proxy-nine.vercel.app}"
: "${CONFIRM:?set CONFIRM=ROLLBACK-DRILL to run a production alias drill}"
if [ "$CONFIRM" != "ROLLBACK-DRILL" ]; then
    echo "refusing: CONFIRM must be ROLLBACK-DRILL" >&2
    exit 1
fi

api() {
    curl -fsS --retry 3 --retry-connrefused --retry-delay 2 \
        -H "Authorization: Bearer $VERCEL_TOKEN" \
        -H "Content-Type: application/json" "$@"
}

deployments() {
    api "https://api.vercel.com/v6/deployments?projectId=${VERCEL_PROJECT_ID}&target=production&limit=20"
}

health_version() {
    curl -fsS "$PROD_URL/healthz" | jq -r '.version // empty'
}

wait_version() {
    local want="$1" i hits=0
    for i in $(seq 1 60); do
        if [ "$(health_version)" = "$want" ]; then
            hits=$((hits + 1))
            if [ "$hits" -ge 2 ]; then
                return 0
            fi
        else
            hits=0
        fi
        sleep 2
    done
    echo "timed out waiting for a stable /healthz version $want (got $(health_version))" >&2
    return 1
}

promote() {
    local id="$1"
    api -X POST "https://api.vercel.com/v10/projects/${VERCEL_PROJECT_ID}/promote/${id}" >/dev/null
}

current_version=$(health_version)
echo "current production version: $current_version"

list=$(deployments)
current_id=$(printf '%s' "$list" | jq -r --arg v "$current_version" \
    '[.deployments[] | select(.state == "READY" and (.meta.githubCommitSha // "") == $v)] | (.[0].uid // .[0].id // empty)')
previous_id=$(printf '%s' "$list" | jq -r --arg cur "$current_id" --arg curver "$current_version" \
    '[.deployments[] | select(.state == "READY" and (.uid // .id) != $cur and (.meta.githubCommitSha // "") != "" and (.meta.githubCommitSha // "") != $curver)] | (.[0].uid // .[0].id // empty)')
previous_version=$(printf '%s' "$list" | jq -r --arg id "$previous_id" \
    '[.deployments[] | select((.uid // .id) == $id)] | (.[0].meta.githubCommitSha // empty)')

if [ -z "$current_id" ] || [ -z "$previous_id" ] || [ -z "$previous_version" ]; then
    echo "could not resolve current/previous deployment IDs (current=$current_id previous=$previous_id version=$previous_version)" >&2
    exit 1
fi
echo "current=$current_id ($current_version)"
echo "previous=$previous_id ($previous_version)"

rolled_back=0
restore() {
    if [ "$rolled_back" = "1" ]; then
        echo "restoring original production deployment $current_id" >&2
        promote "$current_id" || true
        wait_version "$current_version" || true
        go run ./cmd/smoke -base-url "$PROD_URL" -expect-version "$current_version" -attempts 5 -retry-delay 5s || true
    fi
}
trap restore EXIT INT TERM

echo "step 1/4: promote previous"
promote "$previous_id"
rolled_back=1
wait_version "$previous_version"
go run ./cmd/smoke -base-url "$PROD_URL" -expect-version "$previous_version" -attempts 5 -retry-delay 5s

echo "step 2/4: restore current"
promote "$current_id"
wait_version "$current_version"
go run ./cmd/smoke -base-url "$PROD_URL" -expect-version "$current_version" -attempts 5 -retry-delay 5s
rolled_back=0

echo "rollback drill passed: $current_id <-> $previous_id"
