#!/bin/bash
# Format the data disk on first boot and mount it at /mnt/disks/ff.
# Postgres and Redis keep their data here, so the VM can be recreated
# (new COS image, machine type) without losing the database.
set -euo pipefail
dev=/dev/disk/by-id/google-ff-data
mnt=/mnt/disks/ff
for _ in $(seq 1 30); do [ -b "$dev" ] && break; sleep 2; done
if ! blkid "$dev" >/dev/null 2>&1; then
  mkfs.ext4 -m 0 -E lazy_itable_init=0,lazy_journal_init=0,discard -F "$dev"
fi
mkdir -p "$mnt"
mountpoint -q "$mnt" || mount -o discard,defaults,nosuid,nodev "$dev" "$mnt"
mkdir -p "$mnt/postgres" "$mnt/postgres-tls" "$mnt/redis"
