#!/bin/bash
# Isolated local PG16. Tests create connection-local TEMP tables only.
set -euo pipefail
cd -- "$(dirname -- "$0")/../../go"
image_batch_container="isureal-image-batch-$$"
docker run -d --rm --name "$image_batch_container" -e POSTGRES_PASSWORD=pw -p 127.0.0.1::5432 postgres:16 >/dev/null
trap 'docker rm -f "$image_batch_container" >/dev/null 2>&1 || true' EXIT
until docker exec "$image_batch_container" pg_isready -U postgres -q 2>/dev/null; do sleep 0.2; done
image_batch_addr=$(docker port "$image_batch_container" 5432/tcp)
image_batch_port=${image_batch_addr##*:}
ISUREAL_TEST_DSN="postgres://postgres:pw@127.0.0.1:$image_batch_port/postgres?sslmode=disable" \
  go test -count=1 -run '^TestImageReadBatchOnPostgres$' -v .
