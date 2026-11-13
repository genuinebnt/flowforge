# Flowforge infrastructure (spec 17, 18)

Terraform (>= 1.6, `hashicorp/google ~> 6.0`, `hashicorp/random ~> 3.6`)
for one GCP project per environment, laid out as spec 17.4 asks.

```
infra/
  modules/
    cloud-run-service/  service, runtime SA, project roles, secret accessors, env/secret mounts, scaling
    cloud-sql/          Postgres 16, private IP, HA in prod, backups/PITR, per-service users
    memorystore/        Redis 7.2, private services access, AUTH + TLS
    pubsub-topic/       topic, dead-letter topic (14 d), one subscription per consumer, IAM
    storage-bucket/     artifact bucket, lifecycle (raw/ 90 d, engine/archive/events/ 400 d), prefix IAM
    network/            VPC, subnets, PSA, Cloud NAT + static IPs, private DNS, firewall
    lb-edge/            HTTPS LB, serverless NEG, Cloud Armor (WAF + per-IP rate limit), managed cert
    monitoring/         SLO alert policies, log-based metrics, dashboard, budget alerts
  envs/
    dev/  main.tf variables.tf terraform.tfvars backend.tf
    prod/ main.tf variables.tf terraform.tfvars backend.tf
  policy/               conftest rules: no public Cloud Run except gateway, no wildcard/primitive IAM
```

`envs/dev/main.tf` and `envs/prod/main.tf` (and `variables.tf`) are
byte-identical; CI fails if they drift. Every difference between the
environments is in `terraform.tfvars`: project, domain, scaling and max
instances, Cloud SQL HA, Redis tier, NAT IP count, budget, the protected
GitHub environment for prod federation, deletion protection.

## What an environment contains

| Spec | Resources |
|------|-----------|
| 3.1, 17.1 | Cloud Run services `ff-gateway`, `ff-engine`, `ff-scope`, `ff-graph`, `ff-notifier`, `ff-similarity`, `ff-tool-runner`, `ff-egress-proxy`; gen2; h2c (HTTP/2) on every gRPC service; concurrency 80 gateway / 20 engine and graph; CPU always allocated for engine and notifier (and similarity, a pull consumer); min instances 1 for engine and scope; ingress `INTERNAL_LOAD_BALANCER` for the gateway, `INTERNAL_ONLY` for the rest |
| 17.1 | Cloud Run job `ff-migrate` (runs `./cmd/migrate`) |
| 17.1, 17.3 | tool-runner on its own subnet, default-deny egress except the private Google VIP (egress proxy, scope, GCS) and Memorystore; egress proxy on the only NATed subnet (reserved static IPs, output `egress_ips`) |
| 17.1 | Cloud SQL Postgres 16, private IP only, TLS only, `REGIONAL` in prod; users `ff_<service>` with passwords in `ff-db-<service>-password` |
| 17.1 | Memorystore Redis, AUTH + TLS (`ff-redis-auth`, `ff-redis-ca`), `STANDARD_HA` in prod |
| 11.7, 17.1 | Topics `ff.run.events`, `ff.approval.events`, `ff.graph.changes`, `ff.scope.events`, `ff.auth.events`; subscription `<topic>.<consumer>` per consumer with ordering, exponential retry 10 s–600 s, ack deadline 60 s, dead-letter after 10 attempts to `<topic>.dead-letter` (14 days) |
| 17.1 | Cloud Scheduler jobs (from `var.schedules`) posting to `/internal/schedules/{id}:fire` with an OIDC token of `sa-scheduler` |
| 17.1 | Artifact bucket `<project>-ff-artifacts` (uniform access, public access prevention, optional CMEK) |
| 17.1 | Secret Manager: `ff-signing-key-<kid>`, `ff-llm-api-key`, `ff-webhook-<channel>`, `ff-db-<service>-password`, `ff-redis-auth`, `ff-redis-ca`, `ff-tool-<name>` |
| 17.2 | One SA per service; grants on the narrowest resource (topic, subscription, bucket prefix via IAM condition, Cloud Run service, secret). Only `roles/cloudsql.client` is project-level |
| 17.2 | `sa-ci-deploy` (spec "sa-ci"; account ids need 6+ characters) via Workload Identity Federation for this repository: Artifact Registry Writer on the repo, Cloud Run Admin on its services and the migrate job, Service Account User on the runtime SAs |
| 17.1 | Artifact Registry `flowforge` (immutable tags; Container Scanning API on) |
| 17.1 | Global HTTPS LB → serverless NEG → gateway; Cloud Armor OWASP rules + per-IP throttle (429); managed cert; TLS 1.2+ |
| 17.6, 18.1 | Alerts: scope check p99 > 100 ms, submit-to-start p95 > 5 s, graph ingest p95 > 1 s, outbox lag p99 > 10 s, dead letters present; log metrics `ff_scope_denials`, `ff_approvals`; dashboard; budget alerts at 50/80/100 % |

### Environment variables (spec 17.5)

| Variable | Set on |
|----------|--------|
| `FF_ENV`, `FF_JWKS_URL`, `FF_JWT_ISSUER`, `FF_JWT_AUDIENCE`, `FF_PUBSUB_PROJECT`, `FF_TOPIC_PREFIX` | all services |
| `FF_DB_DSN` (`host=/cloudsql/<conn> … user=ff_<service>`) + `PGPASSWORD` (secret) | gateway, engine, scope, graph, notifier, similarity |
| `FF_REDIS_ADDR`, `FF_REDIS_AUTH` (secret), `FF_REDIS_CA` (secret) | gateway, engine, scope, tool-runner |
| `FF_ARTIFACT_BUCKET` | engine, tool-runner, graph |
| `FF_EGRESS_PROXY` | tool-runner (and engine, notifier: see below) |
| `FF_LLM_ENDPOINT`, `FF_LLM_MODEL`, `FF_LLM_API_KEY` (secret) | engine |
| `FF_FUEL_DEFAULT`, `FF_PLAN_FUEL_DEFAULT` | engine |
| `FF_<SERVICE>_ADDR` | callers (gateway → engine/scope/graph/notifier/similarity, engine → scope/graph/tool-runner, tool-runner → scope) |
| `FF_SIGNING_KEY_<KID>` (secrets), `FF_SIGNING_KID` | gateway |
| `FF_WEBHOOK_<CHANNEL>` (secrets) | notifier |

The gateway gets `FF_JWT_AUDIENCE=flowforge-api` (access tokens); internal
services get `flowforge-internal` (internal JWT, spec 13.2).

## Bootstrap (once per environment)

1. Create the project, link billing, and create the state bucket named in
   `backend.tf` (versioning on, uniform access).
2. Create a Terraform identity (`sa-terraform`) with the roles needed to
   manage these resources, and a WIF provider for it that GitHub
   environments `<env>-plan` and `<env>` may use. This identity is
   deliberately outside this configuration (it would otherwise manage its
   own permissions).
3. Edit `terraform.tfvars` placeholders (project, domain, billing account,
   emails).
4. First apply needs real images: build and push them once (see
   `deploy/Dockerfile`), then
   `terraform apply -var-file=images.auto.tfvars.json` or edit `images`.
   After that the deploy workflow rolls images forward and Terraform ignores
   the image field.
5. Add the out-of-band secret values (they never enter Terraform state):
   `gcloud secrets versions add ff-signing-key-<kid> --data-file=-`, likewise
   `ff-llm-api-key`, `ff-webhook-<channel>`, `ff-tool-<name>`. Cloud Run
   revisions fail to start until each mounted secret has a version.
6. Point the API domain's A record at output `api_ip`; the managed
   certificate provisions after DNS resolves.
7. Configure GitHub environments `dev`, `prod` (required reviewers),
   `dev-plan`, `prod-plan` with variables `GCP_PROJECT`, `GCP_REGION`,
   `GCP_WIF_PROVIDER`, `GCP_DEPLOY_SA` (output `ci_service_account`),
   `GCP_TERRAFORM_SA`, `FF_API_DOMAIN`.

## Plan and apply dev

```sh
cd infra/envs/dev
terraform init                      # GCS backend, state locking
terraform plan -out=tfplan
terraform show -json tfplan > plan.json
conftest test --policy ../../policy plan.json
terraform apply tfplan
```

Without credentials, `terraform init -backend=false && terraform validate`
and `conftest verify --policy infra/policy` still work (`make tf-validate
policy-test`). Lock files are not committed; generate them with
`terraform providers lock -platform=linux_amd64 -platform=darwin_arm64`
if you want them pinned.

CI (`.github/workflows/terraform.yml`): fmt, validate, dev/prod parity and
policy unit tests on every PR, plan for dev and prod with the policy check on
the plan JSON, apply dev on merge to main, apply prod on a `v*` tag after
manual approval in the `prod` environment.

## Intentional simplifications and assumptions

- **tool-runner** runs as a gen2 Cloud Run *service* (gVisor/microVM
  sandbox, no credentials beyond its SA), not one Cloud Run Job per tool
  run or GKE Autopilot with gVisor (spec 17.1 allows starting simpler).
- **Egress for engine and notifier**: spec 17.1 makes the egress proxy the
  only identity with internet access, so webhook deliveries (notifier) and an
  external LLM endpoint (engine) also go through it; both get
  `FF_EGRESS_PROXY` and Invoker on the proxy. With Vertex AI the engine needs
  no proxy (set `llm_api_key_enabled = false`).
- **Grants beyond the 17.2 table**, each the narrowest possible: Subscriber on
  their own subscription for gateway (run.events SSE fan-out), tool-runner
  and egress proxy (scope.events cache invalidation) as 11.7 lists them as
  consumers; Secret Accessor on each service's own DB password and the Redis
  AUTH/CA secrets that 17.5 mounts; Invoker on the egress proxy (tool-runner,
  engine, notifier) and on similarity (gateway). `sa-similarity`,
  `sa-egress-proxy`, `sa-migrate` and `sa-scheduler` are not in 17.2 and
  get only what they use.
- **Database grants**: Terraform creates the database and the users
  (`ff_<name>` for each `db_users` entry). The migrations job
  (`cmd/migrate` → `migrations.Deploy`, migration `0018_v1_db_roles.sql`)
  creates the schemas and extensions, grants each service role USAGE + DML
  on its own schema only (revoking it everywhere else and revoking PUBLIC),
  forces row-level security with role-scoped cross-tenant policies, removes
  the service roles from `cloudsqlsuperuser`, and fails if any of them is
  still over-privileged. `ff_migrator` owns the tables and is made a member
  of the NOLOGIN `ff_maintenance` role used by retention jobs. See
  `migrations/README.md` "Database roles". Built-in passwords are used, not
  IAM DB auth.
- **Cloud Scheduler** jobs come from `var.schedules`; user-defined workflow
  schedules created at runtime would need the engine to manage Scheduler
  jobs, which 17.2 does not grant.
- **Pub/Sub** subscriptions are pull; consumers that must keep pulling run
  with CPU always allocated. Dead letters are kept 14 days on the
  dead-letter topic and its `.ops` subscription.
- **Gateway ingress** is `INTERNAL_LOAD_BALANCER` (17.1 says "all … behind
  the load balancer"); the LB is its only public path and the invoker check
  is disabled on it alone.
- **Images**: Terraform pins the initial digest; afterwards
  `.github/workflows/deploy.yml` owns image and traffic split (both are in
  `ignore_changes`). A Terraform change to a service creates a revision with
  the currently deployed image.
- **SLO metric names**: `ff_graph_ingest_seconds` and `ff_outbox_lag_seconds`
  come from 18.1; `ff_scope_check_seconds` (scope service, one `Check`) and
  `ff_run_submit_to_start_seconds` (engine, run_submitted to run_started)
  are Flowforge choices, exported as unlabelled histograms on `/metrics`
  for Managed Prometheus. Log metrics match `msg="scope.decision"` (scope)
  and `msg="approval.changed"` (engine, fields `status` = requested,
  approved, rejected, expired or cancelled, and `role`); the variables'
  defaults are pinned by `services/engine` `TestRegress_G_OPS_04_*`.
- No Serverless VPC Access connector (Direct VPC egress everywhere), no
  CMEK by default, no VPC Service Controls, single region.
- Placeholders: zero image digests, `000000-000000-000000` billing account,
  `*.flowforge.example` domains and emails.
