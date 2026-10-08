#!/usr/bin/env bash
# Self-test of setup-gcpemu (.github/workflows/ci.yml, job "action"): the
# exported environment reaches a running, seeded and configured instance.
set -euo pipefail

fail() { echo "FAIL: $*" >&2; exit 1; }

for v in GCPEMU_GATEWAY GCPEMU_DATA_DIR GCPEMU_INSTANCE STORAGE_EMULATOR_HOST PUBSUB_EMULATOR_HOST GCPEMU_SQL_DB; do
  [ -n "${!v:-}" ] || fail "$v is not exported"
done
command -v gcpemu >/dev/null || fail "gcpemu is not on PATH"
[ "$(command -v gcpemu)" = "$EXPECT_BIN" ] || fail "gcpemu on PATH is $(command -v gcpemu), want $EXPECT_BIN"

# The CLI finds the instance through GCPEMU_INSTANCE and GCPEMU_DATA_DIR.
gcpemu status

# Seed: object, topic and subscription, SQL database.
got=$(curl -fsS "$STORAGE_EMULATOR_HOST/storage/v1/b/selftest-bucket/o/hello.txt?alt=media")
[ "$got" = "hello from the seed" ] || fail "seeded object: $got"
curl -fsS "http://$GCPEMU_GATEWAY/pubsub/v1/projects/selftest-proj/subscriptions/events-pull" | grep -q '"topic": *"projects/selftest-proj/topics/events"' ||
  fail "seeded subscription"
host=${GCPEMU_SQL_DB%:*} port=${GCPEMU_SQL_DB##*:}
got=$(docker run --rm --network=host -e PGPASSWORD=selftest-app postgres:17.10-alpine \
  psql "host=$host port=$port user=app dbname=app" -tAc 'select current_user')
[ "$got" = "app" ] || fail "psql as the seeded user: $got"

# Config: strictProjects with only selftest-proj declared.
code=$(curl -s -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' \
  -d '{"name":"undeclared-bucket"}' "$STORAGE_EMULATOR_HOST/storage/v1/b?project=undeclared-proj")
[ "$code" -ge 400 ] || fail "bucket in an undeclared project: HTTP $code (config not applied?)"

echo "setup-gcpemu self-test passed"
