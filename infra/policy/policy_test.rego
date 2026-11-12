# Unit tests for the conftest policies: `conftest verify --policy infra/policy`.
package main

import rego.v1

svc(address, name, ingress, iam_disabled) := {
	"address": address,
	"type": "google_cloud_run_v2_service",
	"change": {
		"actions": ["create"],
		"after": {
			"name": name,
			"ingress": ingress,
			"invoker_iam_disabled": iam_disabled,
			"template": [{
				"scaling": [{"max_instance_count": 3}],
				"containers": [{"image": "r/x@sha256:0000000000000000000000000000000000000000000000000000000000000000"}],
			}],
		},
	},
}

iam(address, type, role, member) := {
	"address": address,
	"type": type,
	"change": {"actions": ["create"], "after": {"role": role, "member": member, "name": "ff-engine"}},
}

test_gateway_behind_lb_allowed if {
	count(deny) == 0 with input as {"resource_changes": [svc("module.svc[\"gateway\"].x", "ff-gateway", "INGRESS_TRAFFIC_INTERNAL_LOAD_BALANCER", true)]}
}

test_internal_service_allowed if {
	count(deny) == 0 with input as {"resource_changes": [svc("module.svc[\"engine\"].x", "ff-engine", "INGRESS_TRAFFIC_INTERNAL_ONLY", false)]}
}

test_public_engine_denied if {
	count(deny) > 0 with input as {"resource_changes": [svc("e", "ff-engine", "INGRESS_TRAFFIC_ALL", false)]}
}

test_engine_behind_lb_denied if {
	count(deny) > 0 with input as {"resource_changes": [svc("e", "ff-engine", "INGRESS_TRAFFIC_INTERNAL_LOAD_BALANCER", false)]}
}

test_engine_invoker_disabled_denied if {
	count(deny) > 0 with input as {"resource_changes": [svc("e", "ff-engine", "INGRESS_TRAFFIC_INTERNAL_ONLY", true)]}
}

test_gateway_ingress_all_denied if {
	count(deny) > 0 with input as {"resource_changes": [svc("g", "ff-gateway", "INGRESS_TRAFFIC_ALL", true)]}
}

test_tag_image_denied if {
	s := svc("e", "ff-engine", "INGRESS_TRAFFIC_INTERNAL_ONLY", false)
	bad := json.patch(s, [{"op": "replace", "path": "/change/after/template/0/containers/0/image", "value": "r/x:latest"}])
	count(deny) > 0 with input as {"resource_changes": [bad]}
}

test_all_users_invoker_on_engine_denied if {
	count(deny) > 0 with input as {"resource_changes": [iam("i", "google_cloud_run_v2_service_iam_member", "roles/run.invoker", "allUsers")]}
}

test_service_account_invoker_allowed if {
	count(deny) == 0 with input as {"resource_changes": [iam("i", "google_cloud_run_v2_service_iam_member", "roles/run.invoker", "serviceAccount:sa-gateway@p.iam.gserviceaccount.com")]}
}

test_primitive_role_denied if {
	count(deny) > 0 with input as {"resource_changes": [iam("i", "google_project_iam_member", "roles/editor", "serviceAccount:a@p.iam.gserviceaccount.com")]}
}

test_wildcard_role_denied if {
	count(deny) > 0 with input as {"resource_changes": [iam("i", "google_project_iam_member", "roles/pubsub.*", "serviceAccount:a@p.iam.gserviceaccount.com")]}
}

test_wildcard_member_denied if {
	count(deny) > 0 with input as {"resource_changes": [iam("i", "google_service_account_iam_member", "roles/iam.workloadIdentityUser", "principalSet://iam.googleapis.com/projects/1/locations/global/workloadIdentityPools/p/*")]}
}

test_all_users_on_bucket_denied if {
	count(deny) > 0 with input as {"resource_changes": [iam("i", "google_storage_bucket_iam_member", "roles/storage.objectViewer", "allUsers")]}
}

test_binding_members_checked if {
	rc := {
		"address": "b",
		"type": "google_pubsub_topic_iam_binding",
		"change": {"actions": ["create"], "after": {"role": "roles/pubsub.publisher", "members": ["allAuthenticatedUsers"]}},
	}
	count(deny) > 0 with input as {"resource_changes": [rc]}
}

test_policy_data_primitive_denied if {
	rc := {
		"address": "p",
		"type": "google_storage_bucket_iam_policy",
		"change": {"actions": ["create"], "after": {"policy_data": json.marshal({"bindings": [{"role": "roles/owner", "members": ["user:a@b.c"]}]})}},
	}
	count(deny) > 0 with input as {"resource_changes": [rc]}
}

test_custom_role_wildcard_denied if {
	rc := {
		"address": "r",
		"type": "google_project_iam_custom_role",
		"change": {"actions": ["create"], "after": {"permissions": ["run.*"]}},
	}
	count(deny) > 0 with input as {"resource_changes": [rc]}
}

test_sa_key_denied if {
	rc := {"address": "k", "type": "google_service_account_key", "change": {"actions": ["create"], "after": {}}}
	count(deny) > 0 with input as {"resource_changes": [rc]}
}

test_deleted_resources_ignored if {
	rc := {"address": "d", "type": "google_project_iam_member", "change": {"actions": ["delete"], "after": null}}
	count(deny) == 0 with input as {"resource_changes": [rc]}
}
