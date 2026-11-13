# network

VPC and egress controls for one environment (spec 17.3).

- One custom-mode VPC with three subnets used by Cloud Run Direct VPC egress:
  `services` (gateway, engine, scope, graph, notifier, similarity), `tools`
  (tool-runner) and `egress` (egress proxy).
- Private services access range and peering for Cloud SQL and Memorystore.
- Cloud Router and Cloud NAT with `nat_ip_count` reserved static IPs, applied
  **only** to the `egress` subnet, so the egress proxy is the single identity
  with a path to the internet and programs can allowlist its addresses.
- Private DNS zones mapping `*.googleapis.com` and `*.run.app` to
  `private.googleapis.com`, so service-to-service calls to internal-ingress
  Cloud Run services stay inside the VPC.
- Firewall: default-deny egress for the `ff-tool-runner` tag, allowing only
  443 to the private Google VIP (egress proxy, scope, Cloud Storage) and 6378
  to Memorystore; the egress proxy is denied private ranges (SSRF defence).

No Serverless VPC Access connector is created: every service uses Direct VPC
egress.

Outputs: `network_id`, subnet ids, `psa_range_name`, `psa_connection`,
`nat_ips`, network tags.

Cost toggles (all default `true`, the spec 17.3 design used by dev/prod; the
lab sets the first four to `false`): `nat_enabled` (router, NAT, NAT IPs),
`psa_enabled` (private services access and the tool-runner -> Memorystore
rule), `flow_logs_enabled`, `firewall_logs_enabled`, `private_dns_enabled`
(Cloud DNS zones are billed; the lab keeps them because internal-ingress
calls and the tool-runner's default-deny egress depend on them). `moved.tf`
keeps existing state addresses. Extra outputs: `services_cidr`, `tools_cidr`,
`egress_cidr`.
