# Spec 17.4 policy: no public Cloud Run service except the gateway.
#
# Input: `terraform show -json <plan>` output. Run with
#   conftest test --policy infra/policy plan.json
package main

import rego.v1

# The only service allowed a public path (through the LB and Cloud Armor).
is_gateway(name) if regex.match(`(^|-)gateway$`, name)

# Resource changes that will exist after apply.
live_changes contains rc if {
	some rc in input.resource_changes
	not rc.change.actions == ["delete"]
	rc.change.after != null
}

public_members := {"allUsers", "allAuthenticatedUsers"}

# Cloud Run v2 services must not accept traffic from the internet directly.
deny contains msg if {
	some rc in live_changes
	rc.type == "google_cloud_run_v2_service"
	rc.change.after.ingress == "INGRESS_TRAFFIC_ALL"
	msg := sprintf("%s: ingress INGRESS_TRAFFIC_ALL is not allowed; use INGRESS_TRAFFIC_INTERNAL_ONLY, or INGRESS_TRAFFIC_INTERNAL_LOAD_BALANCER for the gateway", [rc.address])
}

deny contains msg if {
	some rc in live_changes
	rc.type == "google_cloud_run_v2_service"
	not is_gateway(rc.change.after.name)
	rc.change.after.ingress != "INGRESS_TRAFFIC_INTERNAL_ONLY"
	msg := sprintf("%s: only the gateway may use ingress %v; internal services must be INGRESS_TRAFFIC_INTERNAL_ONLY", [rc.address, rc.change.after.ingress])
}

# Disabling the invoker check makes a service public.
deny contains msg if {
	some rc in live_changes
	rc.type == "google_cloud_run_v2_service"
	rc.change.after.invoker_iam_disabled == true
	not is_gateway(rc.change.after.name)
	msg := sprintf("%s: invoker_iam_disabled is allowed only on the gateway", [rc.address])
}

# allUsers / allAuthenticatedUsers invoker grants on anything but the gateway.
deny contains msg if {
	some rc in live_changes
	startswith(rc.type, "google_cloud_run")
	some member in iam_members(rc)
	member in public_members
	not is_gateway(object.get(rc.change.after, "name", object.get(rc.change.after, "service", "")))
	msg := sprintf("%s: %s may not invoke a non-gateway Cloud Run resource", [rc.address, member])
}

# Legacy v1 resources: the ingress annotation.
deny contains msg if {
	some rc in live_changes
	rc.type == "google_cloud_run_service"
	some md in rc.change.after.metadata
	md.annotations["run.googleapis.com/ingress"] == "all"
	not is_gateway(rc.change.after.name)
	msg := sprintf("%s: ingress 'all' is not allowed on non-gateway services", [rc.address])
}

# Every service image must be pinned by digest (spec 17.1, 18.2).
deny contains msg if {
	some rc in live_changes
	rc.type == "google_cloud_run_v2_service"
	some tpl in rc.change.after.template
	some c in tpl.containers
	not regex.match(`@sha256:[0-9a-f]{64}$`, c.image)
	msg := sprintf("%s: image %s is not pinned by digest", [rc.address, c.image])
}

# Every service must cap its instances (spec 17.6 cost guardrail).
deny contains msg if {
	some rc in live_changes
	rc.type == "google_cloud_run_v2_service"
	some tpl in rc.change.after.template
	not has_max_instances(tpl)
	msg := sprintf("%s: template.scaling.max_instance_count must be set", [rc.address])
}

has_max_instances(tpl) if {
	some s in tpl.scaling
	s.max_instance_count > 0
}
