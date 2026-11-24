#!/usr/bin/env bash
# Delete a throwaway staged production deployment and verify the DELETE
# endpoint actually removed it. This exercises the same project-scoped API
# path deploy.yml uses when a staged smoke fails.
set -euo pipefail
cd "$(dirname "$0")/.."

: "${VERCEL_TOKEN:?VERCEL_TOKEN is required}"
: "${VERCEL_PROJECT_ID:?VERCEL_PROJECT_ID is required}"
: "${DEPLOY_ID:?DEPLOY_ID is required}"
: "${DEPLOY_URL:=unknown}"
: "${CONFIRM:?set CONFIRM=CLEANUP-DRILL}"
if [ "$CONFIRM" != "CLEANUP-DRILL" ]; then
    echo "refusing: CONFIRM must be CLEANUP-DRILL" >&2
    exit 1
fi
case "$DEPLOY_ID" in dpl_*) ;; *) echo "invalid deployment ID $DEPLOY_ID" >&2; exit 1 ;; esac

api() {
    curl -sS --retry 3 --retry-connrefused --retry-delay 2 \
        -H "Authorization: Bearer $VERCEL_TOKEN" \
        -H "Content-Type: application/json" "$@"
}

echo "deleting staged deployment $DEPLOY_URL ($DEPLOY_ID)"
code=$(api -o /tmp/cleanup-drill-body.json -w '%{http_code}' \
    -X DELETE "https://api.vercel.com/v13/deployments/${DEPLOY_ID}")
case "$code" in
    200|202|204) ;;
    *) echo "DELETE returned HTTP $code"; cat /tmp/cleanup-drill-body.json; exit 1 ;;
esac

after=$(api -o /tmp/cleanup-drill-get.json -w '%{http_code}' \
    "https://api.vercel.com/v13/deployments/${DEPLOY_ID}")
if [ "$after" != "404" ]; then
    echo "deployment still visible after DELETE (HTTP $after)" >&2
    cat /tmp/cleanup-drill-get.json >&2
    exit 1
fi
echo "cleanup drill passed: DELETE removed $DEPLOY_ID (GET now 404)"
