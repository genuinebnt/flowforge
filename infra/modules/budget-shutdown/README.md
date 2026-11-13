# budget-shutdown

Optional budget-triggered shutdown (cost guardrail, docs/GAPS.md G-OPS-20).
Off by default in every environment (`budget_shutdown_enabled`).

```
Cloud Billing budget ──(every update, several a day)──▶ Pub/Sub <name>-budget
    ──push, OIDC as sa-<name>-budget-push──▶ Cloud Run <name>-budget-shutdown
```

The handler (`handler.py`, run with `python3 -c` inside a google-cloud-cli
image, so there is nothing to build) acks every notification and acts only
when `costAmount >= budgetAmount x threshold`:

1. gateway ingress → `internal` (no public traffic; Cloud Run then scales
   the services to zero because nothing calls them),
2. pauses the Cloud Scheduler jobs (the schedules tick),
3. min instances → 0 on `scale_down_services` (dev/prod keep warm services),
4. stops the VM (`vm`, lab) and the Cloud SQL instance (`sql_instance`,
   activation policy `NEVER`, dev/prod).

Each step is idempotent; a failed step returns 500 so Pub/Sub redelivers.
`make gcp-resume ENV=<env>` (or `terraform apply`, which restores the
gateway's ingress and min instances) undoes it.

It cannot stop Memorystore, Cloud NAT, the load balancer, reserved IPs,
disks or stored objects; those keep billing until `make gcp-down`. Budget
notifications lag actual usage (often by hours), so spend can exceed the
budget before the shutdown runs: the budget is a tripwire, not a cap.

## Identity and grants

| Identity | Grant | Resource |
|---|---|---|
| `billing-budget-alert@system.gserviceaccount.com` | Pub/Sub Publisher | the budget topic |
| Pub/Sub service agent | Service Account Token Creator | `sa-<name>-budget-push` |
| `sa-<name>-budget-push` | Cloud Run Invoker | the handler service |
| `sa-<name>-stop` (handler) | Cloud Run Developer + Service Account User on the runtime SA | gateway and each scale-down service |
| `sa-<name>-stop` | Compute Instance Admin (v1) | the VM only |
| `sa-<name>-stop` | custom role `cloudscheduler.jobs.get/pause`, `cloudsql.instances.get/update` | project (Cloud Scheduler has no resource-level IAM) |

## Image

`image` must be pinned by digest (infra/policy). Resolve one when enabling,
for example:

```sh
docker buildx imagetools inspect gcr.io/google.com/cloudsdktool/google-cloud-cli:slim   # prints Digest: sha256:…
```

and set `budget_shutdown_image = "gcr.io/google.com/cloudsdktool/google-cloud-cli@sha256:…"`.

The handler service has min instances 0 and max 1, so it costs nothing
between notifications (a few requests a day, inside the Cloud Run free tier).
