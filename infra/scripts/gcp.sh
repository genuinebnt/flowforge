#!/usr/bin/env bash
# Lifecycle and cost commands for the GCP environments (G-OPS-20).
#
#   infra/scripts/gcp.sh <lab|dev|prod> <up|plan|pause|resume|down|cost|status>
#
#   up      terraform init + plan + conftest policy check + apply
#   plan    the same without apply
#   pause   stop what bills by the hour without destroying anything:
#           gateway ingress -> internal, scheduler jobs paused, min instances
#           -> 0, the lab VM stopped, Cloud SQL stopped (activation NEVER).
#           Cloud Run then scales to zero. Memorystore, Cloud NAT, the load
#           balancer and reserved IPs keep billing in dev/prod: use down.
#   resume  undo pause (`up` also restores gateway ingress and min instances)
#   down    terraform destroy (prod additionally needs CONFIRM=prod and
#           deletion_protection = false)
#   cost    print what is billable and the free-tier assumptions (infra/COSTS.md)
#           and, when the state is reachable, what is running now
#   status  what is running now
#
# Needs terraform, gcloud (authenticated: gcloud auth login and
# gcloud auth application-default login) and, for `up`, conftest if
# installed. Run from the repository root (the Makefile does).
set -euo pipefail

env="${1:?usage: gcp.sh <lab|dev|prod> <up|plan|pause|resume|down|cost|status>}"
cmd="${2:?usage: gcp.sh <lab|dev|prod> <up|plan|pause|resume|down|cost|status>}"
case "$env" in lab | dev | prod) ;; *) echo "gcp.sh: unknown environment $env" >&2; exit 2 ;; esac

root="$(cd "$(dirname "$0")/../.." && pwd)"
dir="$root/infra/envs/$env"
tf() { terraform -chdir="$dir" "$@"; }

# power <field>: one field of the `power` output (names of what to pause).
power_json=""
power() {
	if [ -z "$power_json" ]; then
		power_json="$(tf output -json power 2>/dev/null)" || {
			echo "gcp.sh: cannot read 'terraform output power' for $env (run 'make gcp-up ENV=$env' or terraform init first)" >&2
			exit 1
		}
	fi
	printf '%s' "$power_json" | python3 -c 'import json,sys; print(json.load(sys.stdin).get(sys.argv[1], ""))' "$1"
}

g() { gcloud --project "$(power project)" --quiet "$@"; }

plan() {
	tf init -input=false
	tf plan -input=false -out="$env.tfplan" # *.tfplan is gitignored
	local json
	json="$(mktemp)"
	tf show -json "$env.tfplan" >"$json"
	if command -v conftest >/dev/null 2>&1; then
		conftest test --policy "$root/infra/policy" "$json"
	else
		echo "gcp.sh: conftest not installed; skipping the policy check (CI runs it)" >&2
	fi
}

pause() {
	local region gw jobs vm zone sql svc
	region="$(power region)"
	gw="$(power gateway)"
	jobs="$(power scheduler_jobs)"
	vm="$(power vm)"
	zone="$(power vm_zone)"
	sql="$(power sql_instance)"
	echo "== pausing $env"
	[ -n "$gw" ] && g run services update "$gw" --region "$region" --ingress internal
	for j in $jobs; do g scheduler jobs pause "$j" --location "$region" || true; done
	for pair in $(power warm_services); do
		svc="${pair%%=*}"
		g run services update "$svc" --region "$region" --min-instances 0
	done
	[ -n "$vm" ] && g compute instances stop "$vm" --zone "$zone"
	[ -n "$sql" ] && g sql instances patch "$sql" --activation-policy NEVER
	echo "paused: Cloud Run scales to zero; the VM/Cloud SQL are stopped (their disks are still billed beyond the free tier)."
	if [ "$env" != lab ]; then
		echo "still billing in $env: Memorystore, Cloud NAT + its IPs, the load balancer + its IP. 'make gcp-down ENV=$env' removes them."
	fi
}

resume() {
	local region gw jobs vm zone sql svc min ingress
	region="$(power region)"
	gw="$(power gateway)"
	jobs="$(power scheduler_jobs)"
	vm="$(power vm)"
	zone="$(power vm_zone)"
	sql="$(power sql_instance)"
	ingress=internal-and-cloud-load-balancing
	[ "$env" = lab ] && ingress=all
	echo "== resuming $env"
	[ -n "$sql" ] && g sql instances patch "$sql" --activation-policy ALWAYS
	[ -n "$vm" ] && g compute instances start "$vm" --zone "$zone"
	for pair in $(power warm_services); do
		svc="${pair%%=*}"
		min="${pair#*=}"
		g run services update "$svc" --region "$region" --min-instances "$min"
	done
	for j in $jobs; do g scheduler jobs resume "$j" --location "$region" || true; done
	[ -n "$gw" ] && g run services update "$gw" --region "$region" --ingress "$ingress"
	echo "resumed."
}

status() {
	local region vm zone sql
	region="$(power region)"
	vm="$(power vm)"
	zone="$(power vm_zone)"
	sql="$(power sql_instance)"
	echo "== $env ($(power project), $region)"
	g run services list --region "$region" --format='table(metadata.name,status.url,spec.template.metadata.annotations."autoscaling.knative.dev/minScale":label=MIN,spec.template.metadata.annotations."autoscaling.knative.dev/maxScale":label=MAX)' || true
	[ -n "$vm" ] && g compute instances describe "$vm" --zone "$zone" --format='table(name,status,machineType.basename(),networkInterfaces[0].accessConfigs[0].natIP:label=EXTERNAL_IP)' || true
	[ -n "$sql" ] && g sql instances describe "$sql" --format='table(name,state,settings.tier,settings.activationPolicy)' || true
	g scheduler jobs list --location "$region" --format='table(name.basename(),schedule,state)' || true
}

cost() {
	# The section of infra/COSTS.md between <!-- cost:<env> --> markers.
	awk -v env="$env" '
		$0 == "<!-- cost:" env " -->" { on = 1; next }
		$0 == "<!-- /cost:" env " -->" { on = 0 }
		on { print }
	' "$root/infra/COSTS.md"
	echo
	echo "Free-tier terms change: check https://cloud.google.com/free and the pricing calculator (https://cloud.google.com/products/calculator) before applying."
	if [ -n "${power_json:-}" ] || tf output -json power >/dev/null 2>&1; then
		echo
		status
	else
		echo "(no reachable Terraform state for $env: nothing deployed, or run terraform init first)"
	fi
}

case "$cmd" in
up)
	plan
	tf apply -input=false "$env.tfplan"
	;;
plan) plan ;;
pause) pause ;;
resume) resume ;;
status) status ;;
cost) cost ;;
down)
	if [ "$env" = prod ] && [ "${CONFIRM:-}" != prod ]; then
		echo "gcp.sh: refusing to destroy prod without CONFIRM=prod (and deletion_protection = false in terraform.tfvars)" >&2
		exit 1
	fi
	tf init -input=false
	tf destroy -input=false
	;;
*)
	echo "gcp.sh: unknown command $cmd" >&2
	exit 2
	;;
esac
