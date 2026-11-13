# Unit tests for cost.rego and the lab gateway exception in cloud_run.rego:
# `conftest verify --policy infra/policy`.
package main

import rego.v1

lab_vars(allow) := {"env": {"value": "lab"}, "allow_billable": {"value": allow}}

rc(address, type, after) := {"address": address, "type": type, "change": {"actions": ["create"], "after": after}}

lab_svc(name, ingress, min, max, cpu_idle) := rc(sprintf("module.svc[%q].x", [name]), "google_cloud_run_v2_service", {
	"name": name,
	"ingress": ingress,
	"invoker_iam_disabled": name == "ff-gateway",
	"labels": {"env": "lab", "app": "flowforge"},
	"template": [{
		"scaling": [{"min_instance_count": min, "max_instance_count": max}],
		"containers": [{
			"image": "r/x@sha256:0000000000000000000000000000000000000000000000000000000000000000",
			"resources": [{"cpu_idle": cpu_idle}],
		}],
	}],
})

vm(machine, zone, boot_gb, boot_type) := rc("google_compute_instance.vm", "google_compute_instance", {
	"machine_type": machine,
	"zone": zone,
	"labels": {"env": "lab"},
	"boot_disk": [{"initialize_params": [{"size": boot_gb, "type": boot_type}]}],
})

disk(gb, type) := rc("google_compute_disk.data", "google_compute_disk", {"size": gb, "type": type, "labels": {"env": "lab"}})

lab_ok := [
	lab_svc("ff-gateway", "INGRESS_TRAFFIC_ALL", 0, 2, true),
	lab_svc("ff-engine", "INGRESS_TRAFFIC_INTERNAL_ONLY", 0, 1, true),
	vm("e2-micro", "us-central1-a", 10, "pd-standard"),
	disk(20, "pd-standard"),
	rc("google_compute_address.vm", "google_compute_address", {"address_type": "INTERNAL"}),
]

test_lab_default_plan_allowed if {
	count(deny) == 0 with input as {"variables": lab_vars([]), "resource_changes": lab_ok}
}

test_lab_gateway_ingress_all_allowed_by_label if {
	# no variables section: the env=lab label identifies the plan
	count(deny) == 0 with input as {"resource_changes": [lab_svc("ff-gateway", "INGRESS_TRAFFIC_ALL", 0, 2, true)]}
}

test_lab_engine_ingress_all_denied if {
	count(deny) > 0 with input as {"variables": lab_vars([]), "resource_changes": [lab_svc("ff-engine", "INGRESS_TRAFFIC_ALL", 0, 1, true)]}
}

test_dev_gateway_ingress_all_still_denied if {
	s := json.patch(lab_svc("ff-gateway", "INGRESS_TRAFFIC_ALL", 0, 2, true), [{"op": "replace", "path": "/change/after/labels/env", "value": "dev"}])
	count(deny) > 0 with input as {"variables": {"env": {"value": "dev"}}, "resource_changes": [s]}
}

test_lab_billable_types_denied if {
	every t in object.keys(lab_billable_types) {
		count(deny) > 0 with input as {"variables": lab_vars([]), "resource_changes": [rc("x", t, {"name": "x"})]}
	}
}

test_lab_billable_type_allowed_explicitly if {
	count(deny) == 0 with input as {"variables": lab_vars(["google_sql_database_instance"]), "resource_changes": [rc("x", "google_sql_database_instance", {"name": "x"})]}
}

test_billable_types_fine_outside_lab if {
	count(deny) == 0 with input as {"variables": {"env": {"value": "prod"}}, "resource_changes": [rc("n", "google_compute_router_nat", {"name": "n"}), rc("s", "google_sql_database_instance", {"name": "s"})]}
}

test_lab_external_address_denied if {
	count(deny) > 0 with input as {"variables": lab_vars([]), "resource_changes": [rc("a", "google_compute_address", {"name": "a"})]}
}

test_lab_container_scanning_denied if {
	count(deny) > 0 with input as {"variables": lab_vars([]), "resource_changes": [rc("s", "google_project_service", {"service": "containerscanning.googleapis.com"})]}
}

test_lab_min_instances_denied if {
	count(deny) > 0 with input as {"variables": lab_vars([]), "resource_changes": [lab_svc("ff-engine", "INGRESS_TRAFFIC_INTERNAL_ONLY", 1, 1, true)]}
}

test_lab_min_instances_allowed_explicitly if {
	count(deny) == 0 with input as {"variables": lab_vars(["min_instances"]), "resource_changes": [lab_svc("ff-engine", "INGRESS_TRAFFIC_INTERNAL_ONLY", 1, 1, true)]}
}

test_lab_max_instances_denied if {
	count(deny) > 0 with input as {"variables": lab_vars([]), "resource_changes": [lab_svc("ff-engine", "INGRESS_TRAFFIC_INTERNAL_ONLY", 0, 5, true)]}
}

test_lab_cpu_always_denied if {
	count(deny) > 0 with input as {"variables": lab_vars([]), "resource_changes": [lab_svc("ff-engine", "INGRESS_TRAFFIC_INTERNAL_ONLY", 0, 1, false)]}
}

test_dev_warm_instances_fine if {
	s := json.patch(lab_svc("ff-engine", "INGRESS_TRAFFIC_INTERNAL_ONLY", 1, 3, false), [{"op": "replace", "path": "/change/after/labels/env", "value": "dev"}])
	count(deny) == 0 with input as {"variables": {"env": {"value": "dev"}}, "resource_changes": [s]}
}

test_lab_vm_size_denied if {
	count(deny) > 0 with input as {"variables": lab_vars([]), "resource_changes": [vm("e2-small", "us-central1-a", 10, "pd-standard")]}
}

test_lab_vm_region_denied if {
	count(deny) > 0 with input as {"variables": lab_vars([]), "resource_changes": [vm("e2-micro", "europe-west1-b", 10, "pd-standard")]}
}

test_lab_second_vm_denied if {
	a := vm("e2-micro", "us-central1-a", 10, "pd-standard")
	b := json.patch(a, [{"op": "replace", "path": "/address", "value": "google_compute_instance.other"}])
	count(deny) > 0 with input as {"variables": lab_vars([]), "resource_changes": [a, b]}
}

test_lab_ssd_denied if {
	count(deny) > 0 with input as {"variables": lab_vars([]), "resource_changes": [disk(10, "pd-ssd")]}
}

test_lab_disk_over_30gb_denied if {
	count(deny) > 0 with input as {"variables": lab_vars([]), "resource_changes": [vm("e2-micro", "us-east1-b", 10, "pd-standard"), disk(25, "pd-standard")]}
}

test_lab_too_many_secrets_denied if {
	secrets := [rc(sprintf("s%d", [i]), "google_secret_manager_secret", {"secret_id": sprintf("s%d", [i])}) | some i in numbers.range(1, 7)]
	count(deny) > 0 with input as {"variables": lab_vars([]), "resource_changes": secrets}
}

test_lab_six_secrets_allowed if {
	secrets := [rc(sprintf("s%d", [i]), "google_secret_manager_secret", {"secret_id": sprintf("s%d", [i])}) | some i in numbers.range(1, 6)]
	count(deny) == 0 with input as {"variables": lab_vars([]), "resource_changes": secrets}
}

test_lab_too_many_scheduler_jobs_denied if {
	jobs := [rc(sprintf("j%d", [i]), "google_cloud_scheduler_job", {"name": sprintf("j%d", [i])}) | some i in numbers.range(1, 4)]
	count(deny) > 0 with input as {"variables": lab_vars([]), "resource_changes": jobs}
}
