#!/usr/bin/env bash
set -euo pipefail

image="${IMAGE:-shiftpv:dev}"

if ! docker image inspect "${image}" >/dev/null 2>&1; then
  echo "container image ${image} does not exist; run 'make image-combined IMAGE=${image}' first" >&2
  exit 1
fi

# This is deliberately a compatibility probe, not a durability claim. It uses
# the same Debian/rsync runtime and daemon transport as a mobility helper, then
# checks the filesystem semantics that a generic PVC copy must retain.
docker run --rm -i --user 0:0 --entrypoint /bin/sh "${image}" -seu <<'PROBE'
export DEBIAN_FRONTEND=noninteractive
apt-get update >/dev/null
apt-get install -y --no-install-recommends acl attr >/dev/null

probe_root="$(mktemp -d)"
source_dir="${probe_root}/source"
destination_dir="${probe_root}/destination"
mkdir -p "${source_dir}/nested" "${destination_dir}"

cleanup() {
  result_code=$?
  if [ -n "${daemon_pid:-}" ]; then
    kill "${daemon_pid}" >/dev/null 2>&1 || true
    wait "${daemon_pid}" >/dev/null 2>&1 || true
  fi
  if [ -d "${probe_root}" ]; then
    rm -r -- "${probe_root}"
  fi
  exit "${result_code}"
}
trap cleanup EXIT HUP INT TERM

printf 'shiftpv-v04\n' >"${source_dir}/nested/data"
chmod 0640 "${source_dir}/nested/data"
chown 1234:2345 "${source_dir}/nested/data"
ln "${source_dir}/nested/data" "${source_dir}/hardlink"
ln -s nested/data "${source_dir}/symlink"
mkfifo "${source_dir}/fifo"
truncate -s 16777216 "${source_dir}/sparse"
printf x | dd of="${source_dir}/sparse" bs=1 seek=8388608 conv=notrunc status=none
setfattr -n user.shiftpv.proof -v v04 "${source_dir}/nested/data"
setfacl -m u:1235:r-- "${source_dir}/nested/data"
printf 'remove me\n' >"${destination_dir}/obsolete"

password="shiftpv-v04-probe"
printf 'shiftpv:%s\n' "${password}" >"${probe_root}/secrets"
chmod 0600 "${probe_root}/secrets"
cat >"${probe_root}/rsyncd.conf" <<EOF
uid = 0
gid = 0
use chroot = no
read only = yes
strict modes = yes
port = 1873
[data]
path = ${source_dir}
auth users = shiftpv
secrets file = ${probe_root}/secrets
EOF

rsync --daemon --no-detach --config="${probe_root}/rsyncd.conf" &
daemon_pid=$!
ready=0
attempt=0
while [ "${attempt}" -lt 50 ]; do
  if RSYNC_PASSWORD="${password}" rsync "rsync://shiftpv@127.0.0.1:1873/data/" >/dev/null 2>&1; then
    ready=1
    break
  fi
  attempt=$((attempt + 1))
  sleep 0.1
done
if [ "${ready}" -ne 1 ]; then
  echo "rsync daemon did not become ready" >&2
  exit 1
fi

RSYNC_PASSWORD="${password}" rsync \
  -aHAXS --numeric-ids --one-file-system --no-devices --delete --fsync \
  "rsync://shiftpv@127.0.0.1:1873/data/" "${destination_dir}/"

cmp "${source_dir}/nested/data" "${destination_dir}/nested/data"
test "$(stat -c '%a' "${destination_dir}/nested/data")" = "640"
test "$(stat -c '%u:%g' "${destination_dir}/nested/data")" = "1234:2345"
test "$(readlink "${destination_dir}/symlink")" = "nested/data"
test -p "${destination_dir}/fifo"
test "$(stat -c '%i' "${destination_dir}/nested/data")" = "$(stat -c '%i' "${destination_dir}/hardlink")"
test "$(stat -c '%s' "${destination_dir}/sparse")" = "16777216"
test "$(stat -c '%b' "${destination_dir}/sparse")" -lt 1024
test "$(getfattr --only-values -n user.shiftpv.proof "${destination_dir}/nested/data" 2>/dev/null)" = "v04"
getfacl -cp "${destination_dir}/nested/data" | grep -Fxq 'user:1235:r--'
test ! -e "${destination_dir}/obsolete"

dry_run_output="$(RSYNC_PASSWORD="${password}" rsync \
  -aHAXS --numeric-ids --one-file-system --no-devices --checksum --delete --dry-run --itemize-changes \
  "rsync://shiftpv@127.0.0.1:1873/data/" "${destination_dir}/")"
if [ -n "${dry_run_output}" ]; then
  echo "checksum validation still reports differences:" >&2
  echo "${dry_run_output}" >&2
  exit 1
fi

echo "ShiftPV 0.4 rsync filesystem primitives passed"
PROBE
