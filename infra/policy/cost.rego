# Cost guardrails for the lab environment (docs/GAPS.md G-OPS-19, G-OPS-20;
# infra/COSTS.md). A lab plan must not create resources without a free tier
# unless the lab variable allow_billable names them; the Terraform
# preconditions in infra/envs/lab/main.tf check the same inputs, this checks
# the plan itself, so a later edit to the lab root cannot slip past.
#
# A plan is a lab plan when its variable env is "lab" or any resource
# carries the label env=lab.
package main

import rego.v1

plan_variables := object.get(input, "variables", {})

is_lab if object.get(object.get(plan_variables, "env", {}), "value", "") == "lab"

is_lab if {
	some rc in live_changes
	some field in ["labels", "terraform_labels", "effective_labels"]
	rc.change.after[field].env == "lab"
}

lab_allowed contains a if {
	some a in object.get(object.get(plan_variables, "allow_billable", {}), "value", [])
}

lab_denied(key) if {
	is_lab
	not key in lab_allowed
}

free_tier_regions := {"us-west1", "us-central1", "us-east1"}

# Resource types with no free tier that the lab never creates by default.
lab_billable_types := {
	"google_compute_router_nat": "Cloud NAT",
	"google_compute_global_forwarding_rule": "a global load balancer",
	"google_compute_forwarding_rule": "a regional load balancer",
	"google_compute_security_policy": "Cloud Armor",
	"google_redis_instance": "Memorystore",
	"google_redis_cluster": "Memorystore Cluster",
	"google_sql_database_instance": "Cloud SQL",
	"google_alloydb_instance": "AlloyDB",
	"google_container_cluster": "GKE",
	"google_vpc_access_connector": "a Serverless VPC Access connector",
}

deny contains msg if {
	some rc in live_changes
	what := lab_billable_types[rc.type]
	lab_denied(rc.type)
	msg := sprintf("%s: lab must not create %s (%s, billed); add %q to allow_billable to opt in", [rc.address, what, rc.type, rc.type])
}

# Reserved external addresses (regional or global) are billed; the lab VM
# uses an ephemeral address and internal reservations are free.
deny contains msg if {
	some rc in live_changes
	rc.type in {"google_compute_address", "google_compute_global_address"}
	object.get(rc.change.after, "address_type", "EXTERNAL") != "INTERNAL"
	lab_denied(rc.type)
	msg := sprintf("%s: lab must not reserve an external IP address (billed); add %q to allow_billable to opt in", [rc.address, rc.type])
}

# On-push vulnerability scanning is billed per image.
deny contains msg if {
	some rc in live_changes
	rc.type == "google_project_service"
	rc.change.after.service == "containerscanning.googleapis.com"
	lab_denied("containerscanning")
	msg := sprintf("%s: lab must not enable container scanning (billed per image); add \"containerscanning\" to allow_billable to opt in", [rc.address])
}

# Cloud Run: scale to zero, at most two instances, request-based billing.
deny contains msg if {
	some rc in live_changes
	rc.type == "google_cloud_run_v2_service"
	some tpl in rc.change.after.template
	some s in tpl.scaling
	object.get(s, "min_instance_count", 0) > 0
	lab_denied("min_instances")
	msg := sprintf("%s: lab services scale to zero (min_instance_count %v); add \"min_instances\" to allow_billable to opt in", [rc.address, s.min_instance_count])
}

deny contains msg if {
	some rc in live_changes
	rc.type == "google_cloud_run_v2_service"
	some tpl in rc.change.after.template
	some s in tpl.scaling
	s.max_instance_count > 2
	lab_denied("max_instances")
	msg := sprintf("%s: lab services run at most 2 instances (max_instance_count %v); add \"max_instances\" to allow_billable to opt in", [rc.address, s.max_instance_count])
}

deny contains msg if {
	some rc in live_changes
	rc.type == "google_cloud_run_v2_service"
	some tpl in rc.change.after.template
	some c in tpl.containers
	some r in c.resources
	r.cpu_idle == false
	lab_denied("cpu_always")
	msg := sprintf("%s: CPU always allocated is instance-based billing; add \"cpu_always\" to allow_billable to opt in", [rc.address])
}

# Compute Engine: one e2-micro in a free-tier region, pd-standard, 30 GB total.
deny contains msg if {
	some rc in live_changes
	rc.type == "google_compute_instance"
	rc.change.after.machine_type != "e2-micro"
	lab_denied("vm_size")
	msg := sprintf("%s: lab VMs are e2-micro (machine_type %s); add \"vm_size\" to allow_billable to opt in", [rc.address, rc.change.after.machine_type])
}

deny contains msg if {
	some rc in live_changes
	rc.type == "google_compute_instance"
	region := regex.replace(rc.change.after.zone, `-[a-z]$`, "")
	not region in free_tier_regions
	lab_denied("vm_region")
	msg := sprintf("%s: the e2-micro free tier applies only in us-west1, us-central1 and us-east1 (zone %s); add \"vm_region\" to allow_billable to opt in", [rc.address, rc.change.after.zone])
}

deny contains msg if {
	count([rc | some rc in live_changes; rc.type == "google_compute_instance"]) > 1
	lab_denied("vm_size")
	msg := "lab: the e2-micro free tier covers one VM; add \"vm_size\" to allow_billable to opt in"
}

lab_disk_types contains [rc.address, rc.change.after.type] if {
	some rc in live_changes
	rc.type == "google_compute_disk"
}

lab_disk_types contains [rc.address, p.type] if {
	some rc in live_changes
	rc.type == "google_compute_instance"
	some b in rc.change.after.boot_disk
	some p in b.initialize_params
}

deny contains msg if {
	some [addr, t] in lab_disk_types
	t != "pd-standard"
	lab_denied("disk_size")
	msg := sprintf("%s: lab disks are pd-standard (type %s); add \"disk_size\" to allow_billable to opt in", [addr, t])
}

lab_disk_gb := sum([g |
	some rc in live_changes
	rc.type == "google_compute_disk"
	g := object.get(rc.change.after, "size", 10)
]) + sum([g |
	some rc in live_changes
	rc.type == "google_compute_instance"
	some b in rc.change.after.boot_disk
	some p in b.initialize_params
	g := object.get(p, "size", 10)
])

deny contains msg if {
	lab_disk_gb > 30
	lab_denied("disk_size")
	msg := sprintf("lab: %v GB of persistent disk exceeds the 30 GB free tier; add \"disk_size\" to allow_billable to opt in", [lab_disk_gb])
}

# Secret Manager: 6 active versions free; Cloud Scheduler: 3 jobs free.
deny contains msg if {
	n := count([rc | some rc in live_changes; rc.type == "google_secret_manager_secret"])
	n > 6
	lab_denied("secret_versions")
	msg := sprintf("lab: %v secrets exceed Secret Manager's 6 free active versions; add \"secret_versions\" to allow_billable to opt in", [n])
}

deny contains msg if {
	n := count([rc | some rc in live_changes; rc.type == "google_cloud_scheduler_job"])
	n > 3
	lab_denied("scheduler_jobs")
	msg := sprintf("lab: %v Cloud Scheduler jobs exceed the 3 free jobs per billing account; add \"scheduler_jobs\" to allow_billable to opt in", [n])
}
