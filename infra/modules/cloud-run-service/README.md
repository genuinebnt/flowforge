# cloud-run-service

One Flowforge Cloud Run (v2) service and its runtime identity (spec 17.1,
17.2, 17.5).

Creates:

- `google_service_account` `sa-<service>` (one per service, no keys).
- Project-level predefined roles from `project_roles` (only roles that have
  no narrower resource, such as `roles/cloudsql.client`; the variable rejects
  primitive and wildcard roles).
- `roles/secretmanager.secretAccessor` on each secret in `secret_ids` and
  nothing else; `secret_env` mounts them as environment variables.
- The service: image by digest (validated), `h2c` port when `use_http2`,
  `concurrency`, `min_instances`/`max_instances`, CPU always allocated when
  `cpu_always_allocated`, gen2 execution environment, Direct VPC egress
  (`ALL_TRAFFIC`) on the given subnet with network tags, the Cloud SQL
  connector volume at `/cloudsql` when `cloudsql_instances` is set.
- `roles/run.invoker` for each member of `invoker_members`.

Ingress is limited to `INGRESS_TRAFFIC_INTERNAL_ONLY` or
`INGRESS_TRAFFIC_INTERNAL_LOAD_BALANCER`; `public = true` disables the invoker
check and is allowed only for the gateway (`infra/policy` enforces it).

The image, revision name and traffic split are in `ignore_changes`: the
deploy workflow rolls images forward by digest with 10% then 100% traffic,
and Terraform owns the rest of the configuration.
