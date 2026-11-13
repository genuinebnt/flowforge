# Flowforge infrastructure (spec 17, 18)

Terraform (>= 1.6, `hashicorp/google ~> 6.0`, `hashicorp/random ~> 3.6`)
for one GCP project per environment, laid out as spec 17.4 asks.

## Environments

| Environment | Purpose | Cost (see [COSTS.md](COSTS.md)) | Applied by |
|---|---|---|---|
| `lab` | learning on GCP at near-zero cost: Cloud Run scaling to zero on `run.app`, one free-tier e2-micro VM running Postgres 16, optional Redis and the egress proxy, Pub/Sub, Cloud Storage, Secret Manager, one Scheduler job, Artifact Registry with cleanup | free tier except the VM's external IPv4 address (while it runs) and two Cloud DNS private zones | you (`make gcp-lab-up`) or a manual `workflow_dispatch` (`lab = apply`), never automatically |
| `dev` | the full spec 17 production topology (Cloud SQL, Memorystore, LB, Cloud Armor, NAT) | billed by the hour; `make gcp-down ENV=dev` when done | CI on merge to `main` |
| `prod` | production | billed; HA | CI on a `v*` tag after manual approval |

Running Flowforge needs no cloud account at all (`flowforge dev`, the
compose stack). GCP is optional; `lab` is the cheap way to try it.

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
    monitoring/         SLO alert policies, log-based metrics, dashboard, budget alerts (+ optional Pub/Sub topic)
    budget-shutdown/    optional budget-triggered shutdown (Pub/Sub push -> tiny Cloud Run handler)
  envs/
    dev/  main.tf variables.tf terraform.tfvars backend.tf tests/
    prod/ main.tf variables.tf terraform.tfvars backend.tf tests/
    lab/  main.tf vm.tf vm/ variables.tf outputs.tf terraform.tfvars backend.tf tests/
  policy/               conftest rules: no public Cloud Run except gateway, no wildcard/primitive IAM,
                        lab cost guardrails (cost.rego)
  scripts/              gcp.sh (make gcp-up/pause/resume/down/cost/status), push-images.sh
  COSTS.md              free vs billed per environment, free-tier assumptions
```

The modules are shared: `lab` turns billed pieces off through module
variables (`network`: `nat_enabled`, `psa_enabled`, `flow_logs_enabled`,
`firewall_logs_enabled`, `private_dns_enabled`; `monitoring`:
`alert_policies_enabled`, `log_metrics_enabled`; `pubsub-topic`:
`topic_retention_enabled`; `cloud-run-service` accepts
`INGRESS_TRAFFIC_ALL` for the lab gateway only). dev/prod keep the defaults,
and `moved` blocks keep their state addresses.

`envs/dev/main.tf` and `envs/prod/main.tf` (and `variables.tf`) are
byte-identical; CI fails if they drift. Every difference between the
environments is in `terraform.tfvars`: project, domain, scaling and max
instances, Cloud SQL HA, Redis tier, NAT IP count, budget, the protected
GitHub environment for prod federation, deletion protection.

## What an environment contains

| Spec | Resources |
|------|-----------|
| 3.1, 17.1 | Cloud Run services `ff-gateway`, `ff-engine`, `ff-scope`, `ff-graph`, `ff-notifier`, `ff-similarity`, `ff-tool-runner`, `ff-content`; gen2; h2c (HTTP/2) on every gRPC service; concurrency 80 gateway / 20 engine, graph and content; CPU always allocated for engine, notifier and content (and similarity, a pull consumer); min instances 1 for engine and scope; `ff-content` at most one instance (G-CST-03); ingress `INTERNAL_LOAD_BALANCER` for the gateway, `INTERNAL_ONLY` for the rest |
| 17.1, V2 §9 | Egress proxy VM `ff-<env>-egress` (`modules/egress-vm`, e2-small COS, G-OPS-15/G-SVC-14): HTTP + CONNECT on 3128, SOCKS5 on 1080, DNS (UDP+TCP) on 8053, internal static address in the egress subnet, no external IP (Cloud NAT's reserved IPs); every port from the tool-runner subnet only, the HTTP port also from the services subnet (notifier notify path); SSH through IAP; image rolled by the metadata key `ff-egress-proxy-image` |
| V2 §6 | Content bucket `<project>-ff-content` (`chunks/t/<tenant>/...` packs, indexes and refs; `index/<tenant>/<program>/...` text indexes), `roles/storage.objectUser` for `sa-content` only, no lifecycle delete rule (the service's GC deletes unreferenced packs after the ref swap), soft delete 7 days |
| 17.1 | Cloud Run job `ff-migrate` (runs `./cmd/migrate`) |
| 17.1, 17.3 | tool-runner on its own subnet, default-deny egress except the private Google VIP (scope, content, GCS), Memorystore and the egress VM's ports; egress proxy VM on the only NATed subnet (reserved static IPs, output `egress_ips`) |
| 17.1 | Cloud SQL Postgres 16, private IP only, TLS only, `REGIONAL` in prod; users `ff_<service>` with passwords in `ff-db-<service>-password` (`ff_content` included: migration 0031's role, owner of the `content` schema, subject to RLS) |
| 17.1 | Memorystore Redis, AUTH + TLS (`ff-redis-auth`, `ff-redis-ca`), `STANDARD_HA` in prod |
| 11.7, 17.1 | Topics `ff.run.events`, `ff.approval.events`, `ff.graph.changes`, `ff.scope.events`, `ff.auth.events`, `ff.content.changes` (content → graph, notifier); subscription `<topic>.<consumer>` per consumer with ordering, exponential retry 10 s–600 s, ack deadline 60 s, dead-letter after 10 attempts to `<topic>.dead-letter` (14 days) |
| 17.1 | Cloud Scheduler job `ff-<env>-schedules-tick` posting `/internal/schedules:tick` on the engine every minute with an OIDC token of `sa-scheduler` (G-OPS-10: the engine fires due schedules in process); optional per-schedule jobs from `var.schedules` posting `/internal/schedules/{id}:fire` |
| 17.1 | Artifact bucket `<project>-ff-artifacts` (uniform access, public access prevention, optional CMEK) |
| 17.1 | Secret Manager: `ff-signing-key-<kid>`, `ff-llm-api-key`, `ff-webhook-<channel>`, `ff-db-<service>-password`, `ff-redis-auth`, `ff-redis-ca`, `ff-tool-<name>`, `ff-content-kek` (the content key-encryption key, added out of band) |
| 17.2 | One SA per service; grants on the narrowest resource (topic, subscription, bucket prefix via IAM condition, Cloud Run service, secret). Only `roles/cloudsql.client` is project-level |
| 17.2 | `sa-ci-deploy` (spec "sa-ci"; account ids need 6+ characters) via Workload Identity Federation for this repository: Artifact Registry Writer on the repo, Cloud Run Admin on its services and the migrate job, Service Account User on the runtime SAs |
| 17.1 | Artifact Registry `flowforge` (immutable tags; Container Scanning API on) |
| 17.1 | Global HTTPS LB → serverless NEG → gateway; Cloud Armor OWASP rules + per-IP throttle (429); managed cert; TLS 1.2+ |
| 17.6, 18.1 | Alerts: scope check p99 > 100 ms, submit-to-start p95 > 5 s, graph ingest p95 > 1 s, outbox lag p99 > 10 s, dead letters present; log metrics `ff_scope_denials`, `ff_approvals`; dashboard; budget alerts at 50/80/100 % |

### Environment variables (spec 17.5)

| Variable | Set on |
|----------|--------|
| `FF_ENV`, `FF_BUS=pubsub`, `FF_JWKS_URL`, `FF_JWT_ISSUER`, `FF_JWT_AUDIENCE`, `FF_PUBSUB_PROJECT`, `FF_TOPIC_PREFIX` | all services |
| `FF_DB_DSN` (`host=/cloudsql/<conn> … user=ff_<service>`) + `PGPASSWORD` (secret) | gateway, engine, scope, graph, notifier, similarity, content |
| `FF_REDIS_ADDR`, `FF_REDIS_AUTH` (secret), `FF_REDIS_CA` (secret) | gateway, engine, scope, tool-runner |
| `FF_ARTIFACT_BUCKET` | engine, tool-runner, graph |
| `FF_EGRESS_PROXY` (`http://<egress VM>:3128`) | tool-runner (and engine, notifier: see below) |
| `FF_CONTENT_BUCKET`, `FF_CONTENT_KEK_ID`, `FF_CONTENT_KEK` (secret `ff-content-kek`) | content |
| `FF_EGRESS_SOCKS_LISTEN`/`_ADVERTISE`, `FF_EGRESS_DNS_LISTEN`/`_ADVERTISE`, `PORT` | the egress VM (set by `modules/egress-vm` or the lab VM; tickets carry the advertised addresses) |
| `FF_LLM_ENDPOINT`, `FF_LLM_MODEL`, `FF_LLM_API_KEY` (secret) | engine |
| `FF_FUEL_DEFAULT`, `FF_PLAN_FUEL_DEFAULT` | engine |
| `FF_TICK_OIDC_AUDIENCE` (engine URL), `FF_TICK_SERVICE_ACCOUNT` (`sa-scheduler`) | engine (accepts Cloud Scheduler's ID token on the tick) |
| `FF_<SERVICE>_ADDR` | callers (gateway → engine/scope/graph/notifier/similarity/content, engine → scope/graph/tool-runner, tool-runner → scope/content, egress VM → scope) |
| `FF_SIGNING_KEY_<KID>` (secrets `ff-signing-key-<kid>`), `FF_SIGNING_KID`, `FF_SIGNING_KIDS` | every service that calls another: gateway, engine, graph, similarity, tool-runner, egress proxy (scope and notifier only verify, through `FF_JWKS_URL`) |
| `FF_WEBHOOK_<CHANNEL>` (secrets) | notifier |

The gateway gets `FF_JWT_AUDIENCE=flowforge-api` (access tokens); internal
services get `flowforge-internal` (internal JWT, spec 13.2).

Signing keys (G-OPS-12): `<KID>` is the key id upper-cased with `-` replaced
by `_` (kid `2026-11` -> `FF_SIGNING_KEY_2026_11`), which is the name
`platform.SigningKeyVar` reads. `FF_SIGNING_KID` is the key to sign with,
`FF_SIGNING_KIDS` lists every mounted id (current and next, spec 13.7). All
three environments mount them the same way. The gateway publishes the keys
in its JWKS (`FF_JWKS_URL = <gateway>/.well-known/jwks.json`), and every
service verifies against it with `FF_JWT_ISSUER`.

Redis TLS (G-OPS-13): dev/prod mount `FF_REDIS_CA` (Memorystore's server CA,
secret `ff-redis-ca`); the Go client uses TLS whenever it is set. The lab's
Redis is plaintext (see the lab section).

## Bootstrap (dev, prod; once per environment)

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

## The lab environment

`infra/envs/lab` is not byte-identical to dev/prod. It is a separate root
that reuses the modules. What it creates:

* **Cloud Run** `ff-gateway`, `ff-engine`, `ff-scope`, `ff-graph`,
  `ff-notifier`, `ff-similarity`, `ff-tool-runner`, `ff-content`. Min
  instances 0, max 1 (gateway 2; content exactly 1 at most, G-CST-03), CPU only during requests (request-based billing), Direct VPC
  egress (no connector). The gateway uses ingress `INGRESS_TRAFFIC_ALL` with
  the invoker check off and serves the API on its `run.app` URL (output
  `api_url`, Google-managed TLS). The other services are internal-only. There
  is no load balancer, Cloud Armor or managed certificate.
* **VM** `ff-lab-vm`: e2-micro, Container-Optimized OS, in `var.region`
  (us-central1 by default; us-west1 and us-east1 are also allowed), 10 GB
  boot + 20 GB data disk, both `pd-standard`. It has an internal static
  address in the `egress` subnet and an ephemeral external IPv4 address.
  cloud-init (rewritten on every boot) runs systemd units for:
  * `ff-postgres`: Postgres 16 from `mirror.gcr.io/library/postgres:16`, data
    on the data disk. The extensions come from `deploy/initdb` (pg_trgm,
    citext, and vector if the image has it).
  * `ff-redis`: optional (`redis_enabled`).
  * `ff-egress-proxy`: HTTP + CONNECT on port 3128 (`FF_EGRESS_PROXY =
    http://<vm ip>:3128`), SOCKS5 on 1080 and DNS over UDP and TCP on 8053
    (`socks_port`, `dns_port`; not 53, the container is unprivileged),
    image from Artifact Registry.
  * `ff-image-watch`: restarts the proxy when the metadata key
    `ff-egress-proxy-image` changes (the deploy workflow sets it).

  Secrets come from Secret Manager through the metadata-server token into
  `/run/ff` (tmpfs). Firewall: Postgres/Redis only from the services subnet,
  the proxy's HTTP port from the services and tool-runner subnets, SOCKS5
  and DNS from the tool-runner subnet only, SSH only from IAP
  (`35.235.240.0/20`). The VPC firewall and the host's iptables both enforce
  this. The tool-runner's default-deny egress gets a single exception: the
  proxy.
* **Data**: one database `flowforge` with one role `flowforge` and one
  password secret `ff-db-password` (generated by Terraform). There are no
  per-service roles, which keeps the lab within Secret Manager's free
  versions. G-OPS-01 role separation is therefore exercised in dev/prod only.
  `FF_DB_DSN = host=<vm ip> port=5432 dbname=flowforge user=flowforge
  sslmode=prefer` plus `PGPASSWORD`. The VM generates a self-signed Postgres
  certificate on first boot, so traffic is encrypted but the server is not
  authenticated. The password is applied only when the data directory is
  first initialised.
* **Redis decision (G-OPS-13)**: plaintext with AUTH (`ff-redis-auth`) on the
  private network, reachable only from the services subnet. `FF_REDIS_CA` is
  not set, so the Go client does not use TLS. A self-signed CA would cost one
  more secret and buy little on a single-tenant private subnet.
* **Pub/Sub** topics and subscriptions as in dev, with dead-letter topics.
  Topic retention is off and subscriptions keep messages 7 days.
  `FF_BUS=pubsub`.
* **Cloud Storage** `<project>-ff-artifacts`: Standard class, in the region,
  `raw/` deleted after 30 days, archives after 90, soft delete off.
* **One Cloud Scheduler job** (the tick), **Artifact Registry** with
  cleanup policies (keep the newest 2 versions per image) and no scanning,
  `sa-ci-deploy` through WIF (only jobs in the GitHub environment `lab`),
  a **budget** (default 5 units of the billing account's currency) and the
  optional **budget shutdown**.
* Monitoring alert policies and log-based metrics are off. `FF_ENV=prod`
  (`ff_env`): the Go config accepts dev or prod, and the lab runs with
  production checks.

What request-based billing means here: outside requests, CPU is throttled.
Outbox relays and Pub/Sub pull consumers then progress only while an
instance is serving, for example the engine during each per-minute tick.
That is enough for learning. For continuous consumers set `cpu_always =
true` per service and add `"cpu_always"` to `allow_billable`, which leaves
the free tier.

**Guardrails.** `terraform_data.guardrails` preconditions refuse min
instances > 0, max > 2, CPU always allocated, a machine type other than
e2-micro, disks over 30 GB or not `pd-standard`, and more than 6 secrets.
The region variable accepts only the three free-tier regions.
`infra/policy/cost.rego` denies, in any plan whose `env` variable or
resource labels say `lab`: Cloud NAT, load-balancer forwarding rules, Cloud
Armor, Memorystore, Cloud SQL, AlloyDB, GKE, VPC Access connectors, external
address reservations, container scanning, more than one VM, VMs outside the
free-tier regions, and more than 3 Scheduler jobs. It also repeats the
Cloud Run and disk checks. Each check is lifted only by naming it in
`allow_billable`.

### Bootstrap the lab (operator steps; nothing here has been applied yet)

1. **Project and billing.** Create a project, link a billing account (`gcloud
   billing projects link <project> --billing-account=<id>`), and put both in
   `infra/envs/lab/terraform.tfvars` (`project_id`, `billing_account`), with
   `alert_emails` and `github_repository`. The budget needs
   `roles/billing.costsManager` on the billing account for whoever runs
   Terraform.
2. **APIs** needed before the first apply (Terraform enables the rest):
   `gcloud services enable cloudresourcemanager.googleapis.com
   serviceusage.googleapis.com iam.googleapis.com
   billingbudgets.googleapis.com --project <project>`.
3. **State bucket** (named in `backend.tf`): `gcloud storage buckets create
   gs://flowforge-lab-tfstate --project <project> --location us-central1
   --uniform-bucket-level-access --public-access-prevention` then `gcloud
   storage buckets update gs://flowforge-lab-tfstate --versioning`. Change
   the name in `backend.tf` if it is taken.
4. **Credentials**: `gcloud auth login` and `gcloud auth
   application-default login` (local apply), or for CI a `sa-terraform`
   identity with a WIF provider for the GitHub environments `lab-plan` and
   `lab`, outside this configuration (as for dev/prod).
5. **Registry, secrets, images**: `terraform -chdir=infra/envs/lab init`,
   then `terraform -chdir=infra/envs/lab apply
   -target=google_project_service.apis
   -target=google_artifact_registry_repository.images
   -target=google_secret_manager_secret.this`. Next run `gcloud auth
   configure-docker us-central1-docker.pkg.dev` and `make gcp-lab-images`,
   which builds the 10 images and writes
   `infra/envs/lab/images.auto.tfvars.json`.
6. **Signing key** (out of band, so its value never enters Terraform state):
   `openssl rand -hex 32 | gcloud secrets versions add
   ff-signing-key-2026-11 --data-file=- --project <project>`. Cloud Run
   revisions do not start until every mounted secret has a version.
   `ff-db-password` (and `ff-redis-auth`) are written by Terraform.
7. **Apply**: `make gcp-lab-up` (plan, conftest policy check, apply). Then
   run migrations: `gcloud run jobs execute ff-migrate --region us-central1
   --wait`. Check with `curl $(terraform -chdir=infra/envs/lab output -raw
   api_url)/readyz`.
8. **CI (optional)**: GitHub environments `lab-plan` and `lab` with variables
   `GCP_PROJECT`, `GCP_REGION`, `GCP_WIF_PROVIDER` (output
   `workload_identity_provider`), `GCP_DEPLOY_SA` (output
   `ci_service_account`), `GCP_TERRAFORM_SA`, `FF_API_DOMAIN` (the host of
   `api_url`), `GCP_VM_NAME` and `GCP_VM_ZONE` (output `vm`). Deploys and
   applies run only from `workflow_dispatch` (deploy: environment `lab`;
   terraform: input `lab = plan | apply`).
9. **When you are done**: `make gcp-lab-pause` stops the VM (releasing the
   external IP), pauses the tick and closes the gateway. `make
   gcp-lab-resume` undoes it. `make gcp-lab-down` destroys everything,
   including the database disk. `make gcp-lab-cost` shows what bills.

## Lifecycle commands (all environments)

| Target | Does |
|---|---|
| `make gcp-up ENV=<env>` / `gcp-lab-up` | `terraform init`, plan, conftest on the plan, apply |
| `make gcp-plan ENV=<env>` | the same without apply |
| `make gcp-pause ENV=<env>` / `gcp-lab-pause` | gateway ingress internal, scheduler jobs paused, min instances 0, VM stopped (lab), Cloud SQL stopped (dev/prod). Memorystore, NAT, the LB and reserved IPs keep billing in dev/prod |
| `make gcp-resume ENV=<env>` / `gcp-lab-resume` | undo pause (`gcp-up` also restores ingress and min instances) |
| `make gcp-down ENV=<env>` / `gcp-lab-down` | `terraform destroy` (prod: `CONFIRM=prod` and `deletion_protection = false`) |
| `make gcp-cost ENV=<env>` / `gcp-lab-cost` | the environment's section of COSTS.md plus what is running |
| `make gcp-status ENV=<env>` | services, VM, Cloud SQL and scheduler state |
| `make gcp-images ENV=<env>` | build and push every image, write `images.auto.tfvars.json` |

Pause and resume use gcloud with names from the `power` output, so they need
the Terraform state but no plan. The optional budget shutdown
(`budget_shutdown_enabled`, all environments) does the same as pause when the
budget is exhausted.

## Plan and apply dev

```sh
cd infra/envs/dev
terraform init                      # GCS backend, state locking
terraform plan -out=tfplan
terraform show -json tfplan > plan.json
conftest test --policy ../../policy plan.json
terraform apply tfplan
```

Without credentials, `terraform init -backend=false && terraform validate`,
the mock-provider plan tests (`terraform test`, `infra/envs/*/tests`) and
`conftest verify --policy infra/policy` still work (`make tf-validate tf-test
policy-test`). Lock files are not committed; generate them with
`terraform providers lock -platform=linux_amd64 -platform=darwin_arm64`
if you want them pinned.

CI (`.github/workflows/terraform.yml`): fmt, validate (lab, dev, prod),
dev/prod parity, mock-provider plan tests and policy unit tests on every PR,
plan for dev and prod with the policy check on the plan JSON, apply dev on
merge to main, apply prod on a `v*` tag after manual approval in the `prod`
environment. The lab is planned or applied only by `workflow_dispatch` with
input `lab`.

## Intentional simplifications and assumptions

- **tool-runner** runs as a gen2 Cloud Run *service* (gVisor/microVM
  sandbox, no credentials beyond its SA), not one Cloud Run Job per tool
  run or GKE Autopilot with gVisor (spec 17.1 allows starting simpler).
- **Egress for engine and notifier**: spec 17.1 makes the egress proxy the
  only identity with internet access, so webhook deliveries (notifier) and an
  external LLM endpoint (engine) also go through it; both get
  `FF_EGRESS_PROXY` (the VM's HTTP port, open to the services subnet).
  Notifier webhook and Slack
  deliveries use the proxy's notify path (`X-Flowforge-Egress-Purpose:
  notify`, see `services/egressproxy/README.md`): no scope decision, but the
  destination host must be on the proxy's `FF_NOTIFY_ALLOWLIST`
  (comma-separated host names, `.suffix` entries for subdomains; empty
  refuses every delivery), and the proxy resolves it and connects only to
  public addresses (the webhook SSRF rules of spec 15.4). Set
  `FF_NOTIFY_ALLOWLIST` (`notify_allowlist`, on the egress VM) (for example
  `hooks.slack.com`); the notifier refuses to start in prod without
  `FF_EGRESS_PROXY`. With Vertex AI the engine needs
  no proxy (set `llm_api_key_enabled = false`).
- **Grants beyond the 17.2 table**, each the narrowest possible: Subscriber on
  their own subscription for gateway (run.events SSE fan-out), tool-runner
  and egress proxy (scope.events cache invalidation) as 11.7 lists them as
  consumers; Secret Accessor on each service's own DB password and the Redis
  AUTH/CA secrets that 17.5 mounts; Invoker on similarity (gateway), on
  tool-runner (gateway: `ListTools`), on scope (graph, similarity, the
  egress VM's account), on content (gateway, tool-runner) and on graph
  (similarity), matching the `FF_*_ADDR` each binary dials. `sa-similarity`,
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
- **Cloud Scheduler**: one tick job per environment; the engine's in-process
  scheduler fires the schedules that are due, including those created at
  runtime, so no identity needs to manage Scheduler jobs. `var.schedules`
  can still create per-schedule `:fire` jobs.
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
- Free-tier assumptions for the lab are dated in [COSTS.md](COSTS.md);
  check the pricing calculator before applying.

## Known gaps seen from the infrastructure side

- Cloud Run service-to-service calls: Terraform sets `FF_<SERVICE>_ADDR` to
  `https://<service>-<number>.<region>.run.app`, and callers hold Cloud Run
  Invoker. Spec 17.3 also wants the caller to attach a Google-signed ID
  token, but `platform.Dial` dials without TLS and sends only the internal
  JWT in `authorization`. Calls between Cloud Run services will fail until
  the client uses TLS on port 443 and sends the ID token (for example in
  `X-Serverless-Authorization`, leaving `authorization` for the internal
  JWT). This affects all three environments and is outside `infra/`.
- The egress proxy runs on one VM per environment (dev/prod:
  `modules/egress-vm`; lab: the shared VM), because a Cloud Run front end
  carries neither CONNECT to arbitrary ports nor raw TCP/UDP (G-OPS-15,
  G-SVC-14). It is one VM, not a MIG: a restart or `make gcp-pause` drops
  open tunnels, and the per-target limiter is in process. The deploy
  workflow must roll it through the metadata key `ff-egress-proxy-image`
  in dev/prod as it does in the lab (CI holds `roles/compute.instanceAdmin.v1`
  on the VM and Service Account User on its account).
- The content service writes one chunk store per tenant from one process
  (G-CST-03, v3); Terraform pins `ff-content` to one instance, and the GCS
  adapter's generation preconditions make a second writer fail loudly.
