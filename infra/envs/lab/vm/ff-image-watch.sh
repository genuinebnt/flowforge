#!/bin/bash
# Restart the egress proxy when the metadata key ff-egress-proxy-image
# changes (long poll on the metadata server).
set -uo pipefail
url='http://metadata.google.internal/computeMetadata/v1/instance/attributes/ff-egress-proxy-image'
etag=NONE
while true; do
  if ! curl -sSf -o /dev/null -D /run/ff/image-watch.hdr -H 'Metadata-Flavor: Google' \
    "$url?wait_for_change=true&timeout_sec=600&last_etag=$etag"; then
    sleep 30
    continue
  fi
  new="$(sed -n 's/^[Ee][Tt][Aa][Gg]: *\([^[:space:]]*\).*/\1/p' /run/ff/image-watch.hdr)"
  if [ "$etag" != NONE ] && [ -n "$new" ] && [ "$new" != "$etag" ]; then
    echo "ff-image-watch: ff-egress-proxy-image changed; restarting the proxy"
    systemctl restart ff-egress-proxy.service
  fi
  etag="${new:-NONE}"
done
