# lb-edge

Public entry point (spec 17.1 edge row, 18.2 abuse controls).

- Global external Application Load Balancer (`EXTERNAL_MANAGED`) with a
  serverless NEG pointing at the gateway Cloud Run service. The gateway's
  ingress is `INGRESS_TRAFFIC_INTERNAL_LOAD_BALANCER`, so this is its only
  public path.
- Cloud Armor policy: a per-IP `throttle` rule (`rate_limit_per_minute`,
  429 on excess) as the first layer, the preconfigured OWASP rule sets in
  `waf_rules` (deny 403), default allow.
- Google-managed certificate for `domains`, SSL policy `MODERN` with
  minimum TLS 1.2, and a port-80 forwarding rule that only redirects to
  HTTPS.

Output `ip_address` is the address to put in DNS before the certificate can
provision.
