# egress-vm

The v2 egress proxy on one Container-Optimized OS VM, for dev and prod
(docs/GAPS.md G-OPS-15, G-SVC-14). Cloud Run accepts only HTTP requests
through its front end, so the proxy's CONNECT tunnels to arbitrary ports,
SOCKS5 (RFC 1928) and DNS over UDP and TCP (V2_DESIGN §9) cannot run there.
The lab runs the same proxy on its single free-tier VM next to Postgres
(`infra/envs/lab/vm.tf`); this module is that VM's egress-proxy part on its
own.

Creates:

- `google_service_account` (`account_id`) with `roles/logging.logWriter`
  (optional), `roles/artifactregistry.reader` on the image repository and
  `roles/secretmanager.secretAccessor` on each secret in `secret_ids`.
- An internal static address in the egress subnet and the VM (no external
  IP by default: traffic leaves through Cloud NAT's reserved IPs), shielded
  VM, OS Login, project SSH keys blocked.
- cloud-init units: `ff-firewall` (host iptables), `ff-secrets` (Secret
  Manager → `/run/ff/egress-proxy.env` on tmpfs), `ff-egress-proxy`
  (read-only, unprivileged container, all capabilities dropped) and
  `ff-image-watch` (restarts the proxy when the metadata key
  `ff-egress-proxy-image` changes; the deploy workflow sets it).
- Firewall: HTTP, SOCKS5 and DNS (TCP+UDP) from `tool_cidrs`; the HTTP
  port only from `http_client_cidrs` (the notifier's notify path); SSH from
  the IAP range; an EGRESS allow from the tool-runner tag to this address,
  the only exception to its default-deny egress.

Ports: `proxy_port` 3128 (`FF_EGRESS_PROXY` = `proxy_url`), `socks_port`
1080 and `dns_port` 8053 (not 53: the container is unprivileged). The
module sets `PORT`, `FF_EGRESS_SOCKS_LISTEN`/`_ADVERTISE` and
`FF_EGRESS_DNS_LISTEN`/`_ADVERTISE`; tickets carry the advertised
addresses, so the tool runner needs only `FF_EGRESS_PROXY`.

One VM, not a managed instance group: the proxy keeps per-target limiter
state in process (no Redis on this path: the network module denies the
proxy every private range, Memorystore included), and `make gcp-pause`
and the budget shutdown stop it by name. A restart drops open tunnels.
