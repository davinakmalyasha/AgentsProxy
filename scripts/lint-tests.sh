#!/usr/bin/env bash
# scripts/lint-tests.sh — tenant isolation in tests.
#
# Every test tenant must own a cloned schema. A feature test that writes
# project_centrum directly silently tests the production tenant instead of
# the tenant it seeded; the unique db_schema index also makes shared-schema
# fixtures impossible. The clone source lives in testutil/dbtest, not in a
# feature test.
set -euo pipefail
cd "$(dirname "$0")/.."

fail=0
if grep -rn "project_centrum" projects/centrum/backend --include='*_test.go'; then
  echo "FEATURE TEST HARDCODES project_centrum; use testutil.CloneTenantSchema"
  fail=1
fi
if grep -rn "project_centrum\." AgentsTools --include='*_test.go' | grep -v 'dbtest.SharedSchema'; then
  echo "AGENTSTOOLS TEST HARDCODES project_centrum; use dbtest.SchemaPool"
  fail=1
fi
if [ "$fail" = 1 ]; then exit 1; fi
echo "tests schema-isolated"
