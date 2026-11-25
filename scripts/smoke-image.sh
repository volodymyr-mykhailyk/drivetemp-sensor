#!/bin/sh
# End-to-end check of a built image against the fake sysfs in testdata/:
# runs as the image's unprivileged user with a named volume for the output,
# checks values and the healthcheck, then stops it and checks the failsafe.
set -eu
image=${1:?usage: smoke-image.sh IMAGE}
name=drivetemp-sensor-smoke-$$
vol=$name
cleanup() { docker rm -f "$name" >/dev/null 2>&1 || true; docker volume rm "$vol" >/dev/null 2>&1 || true; }
trap cleanup EXIT

docker volume create "$vol" >/dev/null
docker run -d --name "$name" \
  -v "$(pwd)/testdata/sysfs:/fakesys:ro" \
  -v "$vol:/run/drivetemp-sensor" \
  -e DRIVETEMP_SENSOR_SYSFS_ROOT=/fakesys \
  -e DRIVETEMP_SENSOR_INTERVAL=1s \
  -e DRIVETEMP_SENSOR_READ_TIMEOUT=1s \
  -e DRIVETEMP_SENSOR_EXCLUDE='Elements*' \
  "$image" >/dev/null

value() { docker run --rm -v "$vol:/out:ro" --entrypoint "" busybox:1.37 cat "/out/$1"; }

i=0
until [ "$(value hdd-max 2>/dev/null)" = 41000 ]; do
  i=$((i + 1))
  if [ "$i" -gt 30 ]; then docker logs "$name"; echo "hdd-max never reached 41000" >&2; exit 1; fi
  sleep 1
done
[ "$(value ssd-max)" = 35000 ] || { echo "ssd-max = $(value ssd-max), want 35000" >&2; exit 1; }
docker exec "$name" /drivetemp-sensor healthcheck || { echo "healthcheck failed" >&2; exit 1; }

docker stop -t 10 "$name" >/dev/null
code=$(docker inspect "$name" --format '{{.State.ExitCode}}')
[ "$code" = 0 ] || { docker logs "$name"; echo "exit code $code, want 0" >&2; exit 1; }
[ "$(value hdd-max)" = 100000 ] || { echo "hdd-max after stop = $(value hdd-max), want 100000" >&2; exit 1; }
[ "$(value ssd-max)" = 100000 ] || { echo "ssd-max after stop = $(value ssd-max), want 100000" >&2; exit 1; }
docker logs "$name" 2>&1
echo "smoke test passed"
