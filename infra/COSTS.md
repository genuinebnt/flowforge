# What the GCP environments cost

This page says which resources each environment creates, which of them a
free tier covers and which are billed, and why. It does not quote prices.
Prices and free-tier terms change, so check these before you apply:

* the free tier page: <https://cloud.google.com/free> (section "Free Tier
  usage limits"),
* each product's pricing page (linked below),
* the pricing calculator: <https://cloud.google.com/products/calculator>.

**Assumptions as of 2026-11-13.** The allowances below are what the Google
Cloud free tier documented when this page was written. Check them again
before relying on them.

* The free tier is per **billing account**, not per project. Other projects
  on the same billing account use up the same allowance (the e2-micro, the
  3 Scheduler jobs, the 6 secret versions, and so on).
* Free Trial credits are a separate program. They pay for billed usage while
  they last, so a new account can hide costs that start once the trial ends.
* Budget notifications lag usage, often by hours. A budget, and the optional
  budget shutdown, is a tripwire, not a hard cap.

`make gcp-cost ENV=<env>` (or `make gcp-lab-cost`) prints the section for one
environment, plus what is running now when the Terraform state is reachable.

<!-- cost:lab -->
## lab (near-zero cost)

### Free at lab volumes

| Resource | Free tier (as documented 2026-11-13) | Lab usage |
|---|---|---|
| Compute Engine VM `ff-lab-vm` | 1 non-preemptible **e2-micro** per month in us-west1, us-central1 or us-east1 | one e2-micro, standard provisioning, in `var.region` (validated to those three) |
| Persistent disk | 30 GB-months of **standard** persistent disk | 10 GB boot + 20 GB data, `pd-standard` (guardrail: ≤ 30 GB) |
| VM network egress | 1 GB a month from North America to all destinations except China and Australia | proxy traffic to targets; anything above 1 GB is billed |
| Cloud Run (7 services, the migrate job, the optional shutdown handler) | Request-based billing: 180,000 vCPU-seconds, 360,000 GiB-seconds and 2 million requests a month | min instances 0, max 1–2, CPU only during requests. The tick is 43,200 short requests a month |
| Pub/Sub (5 topics, 5 dead-letter topics, subscriptions) | 10 GiB of messages a month | topic retention off (no message storage); subscriptions keep undelivered messages for 7 days |
| Cloud Storage bucket `<project>-ff-artifacts` | 5 GB-months of Standard storage in us-west1, us-central1 or us-east1, plus monthly Class A and Class B operation allowances | Standard class in `var.region`, `raw/` deleted after 30 days, `engine/archive/events/` after 90, soft delete off |
| Secret Manager | 6 active secret versions and 10,000 access operations a month | 2 by default (`ff-signing-key-<kid>`, `ff-db-password`); Redis, the LLM key, webhooks and tool secrets each add 1 (guardrail: ≤ 6) |
| Cloud Scheduler | 3 jobs per billing account | 1 (`ff-lab-schedules-tick`) |
| Artifact Registry | 0.5 GB of storage a month | cleanup policy keeps the newest 2 versions of each of the 9 images, deletes the rest after a day; no vulnerability scanning |
| Cloud Logging | 50 GiB of ingestion per project a month, default retention | Cloud Run and VM container logs; no VPC flow logs and no firewall logs |
| Cloud Monitoring | Google Cloud system metrics | no SLO alert policies, log-based metrics or Prometheus metrics in the lab |
| Budget, Pub/Sub budget notifications, IAM, service accounts, Workload Identity Federation, VPC, subnets, firewall rules, internal IP reservation, IAP TCP forwarding for SSH | no charge | used |
| Terraform state bucket | covered by the Cloud Storage allowance (a few KB) | `flowforge-lab-tfstate` |

### Billed in the lab

| Resource | Why it is billed | How to avoid it |
|---|---|---|
| **The VM's external IPv4 address** | Google Cloud bills external IPv4 addresses in use, and the free tier does not cover them. This is the lab's main fixed cost while the VM runs. The address is ephemeral: it is released, and stops billing, when the VM is stopped. | `make gcp-lab-pause` when you are not using the lab. The egress proxy needs it: the lab has no Cloud NAT. |
| **Cloud DNS private zones** (`googleapis.com`, `run.app`) | Cloud DNS has no free tier. It bills per managed zone per month and per query ([pricing](https://cloud.google.com/dns/pricing)). These zones send internal-ingress service calls and the tool-runner's default-deny egress through the VPC. | `private_dns_enabled = false` removes them, but the tool-runner and service-to-service calls then break. Keep them. |
| VM egress over 1 GB a month | above the free allowance ([pricing](https://cloud.google.com/vpc/network-pricing)) | keep scans small |
| Cloud Run above the free tier | for example `cpu_always = true` (instance-based billing) on a service that the per-minute tick keeps warm, or min instances > 0 | the guardrails refuse both unless `allow_billable` lists `cpu_always` or `min_instances` |
| Snapshots, extra disks, a second VM, a non-free-tier region or machine type | outside the free tier | the guardrails and `infra/policy/cost.rego` refuse them by default |
| Storage above 5 GB, Artifact Registry above 0.5 GB, more than 6 secret versions | above the allowance | lifecycle rules, cleanup policies, the secret-count guardrail. Rotating a signing key adds a version until you disable or destroy the old one |

### What the lab does not create (billed, no free tier)

Cloud SQL, Memorystore, Cloud NAT, external load balancers and forwarding
rules, Cloud Armor, reserved static external IPs, Serverless VPC Access
connectors (they bill while they run; the lab uses Direct VPC egress), GKE,
container scanning. `infra/policy/cost.rego` denies each of these in a lab
plan unless `allow_billable` names it.

### Cost guardrails

* Budget `Flowforge lab monthly`, default **5** units of the billing
  account's currency (`monthly_budget`). Alerts by email at 50, 80 and 100 %
  of actual spend and at 100 % of forecast.
* Optional budget shutdown (`budget_shutdown_enabled`, off by default). When
  spend reaches the budget, it closes the gateway, pauses the tick and stops
  the VM. See `infra/modules/budget-shutdown`.
* `make gcp-lab-pause` / `gcp-lab-resume` / `gcp-lab-down`.
* Max instances on every Cloud Run service. Every resource carries the labels
  `env=lab` and `app=flowforge`, so the billing reports can filter on them.
<!-- /cost:lab -->

<!-- cost:dev -->
## dev (the full spec 17 topology)

dev is for learning the production design, and it is **not** near zero cost.
Several of its resources have no free tier and bill every hour they exist,
whether or not anything uses them:

| Resource | Billed because |
|---|---|
| Cloud SQL for PostgreSQL (`db-custom-1-3840`, zonal in dev) | instance hours, storage and backups; no free tier ([pricing](https://cloud.google.com/sql/pricing)) |
| Memorystore for Redis (BASIC, 1 GB) | capacity per hour; no free tier ([pricing](https://cloud.google.com/memorystore/docs/redis/pricing)) |
| External HTTPS load balancer (forwarding rules) and its global IP | forwarding rules per hour plus data processed ([pricing](https://cloud.google.com/vpc/network-pricing#lb)) |
| Cloud Armor policy and rules | per policy, per rule and per request ([pricing](https://cloud.google.com/armor/pricing)) |
| Cloud NAT and its reserved IPs | per gateway hour, per GB processed and per IP ([pricing](https://cloud.google.com/nat/pricing)) |
| Cloud Run `ff-engine` and `ff-scope` (min instances 1); engine, notifier and similarity with CPU always allocated | warm instances bill continuously (instance-based billing), well above the free tier |
| Cloud DNS private zones (2) | per zone and per query |
| Container scanning (Artifact Analysis) | per image scanned on push |
| Secret Manager | dev has more than 6 secrets (signing keys, LLM key, webhooks, 7 database passwords, Redis AUTH and CA), so versions beyond the free 6 are billed |
| Pub/Sub topic retention (7 days, 14 for dead letters) | message storage |
| VPC flow logs, firewall logs, log-based metrics, Managed Prometheus samples, alert policies | Logging and Monitoring charges beyond their free allotments ([pricing](https://cloud.google.com/stackdriver/pricing)) |
| Artifact Registry | no cleanup policy, so images accumulate past 0.5 GB |

Free at dev volumes: Pub/Sub throughput (10 GiB), Cloud Storage (5 GB in a
US region), Scheduler (the tick job, if it is within the account's 3 free
jobs), Logging ingestion (50 GiB), IAM, VPC and the budget.

`make gcp-pause ENV=dev` stops Cloud SQL, sets min instances to 0, pauses the
tick and closes the gateway. **Memorystore, NAT, the load balancer and the
reserved IPs keep billing while paused.** `make gcp-down ENV=dev` destroys
everything. Run it when you are done (dev has `deletion_protection = false`).
<!-- /cost:dev -->

<!-- cost:prod -->
## prod

prod has everything in dev, sized up: Cloud SQL `REGIONAL` (HA, about
twice the instance cost), Memorystore `STANDARD_HA`, 2 NAT IPs, min instances
1 on most services, larger CPU and memory, and the same no-free-tier
services (load balancer, Cloud Armor, NAT, Cloud DNS, container scanning).
Price it with the calculator before applying. It is meant for real use, not
for learning on a budget.

`deletion_protection = true` in prod. `make gcp-down ENV=prod` refuses to run
without `CONFIRM=prod`, and Terraform refuses until you set
`deletion_protection = false` and apply.
<!-- /cost:prod -->
