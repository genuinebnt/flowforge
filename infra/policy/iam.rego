# Spec 17.2 / 17.4 policy: least-privilege IAM.
#   - no primitive roles (owner, editor, viewer) anywhere
#   - no wildcards in roles, members or custom role permissions
#   - no allUsers / allAuthenticatedUsers outside Cloud Run (see cloud_run.rego)
#   - no authoritative project IAM policies
package main

import rego.v1

primitive_roles := {"roles/owner", "roles/editor", "roles/viewer"}

is_iam_resource(t) if endswith(t, "_iam_member")

is_iam_resource(t) if endswith(t, "_iam_binding")

# Members granted by an IAM member/binding resource.
iam_members(rc) := {m | m := rc.change.after.member; is_string(m)} | {m | some m in object.get(rc.change.after, "members", [])}

# Roles granted by *_iam_member, *_iam_binding and *_iam_policy resources.
granted_roles contains [rc.address, role] if {
	some rc in live_changes
	is_iam_resource(rc.type)
	role := rc.change.after.role
}

granted_roles contains [rc.address, b.role] if {
	some rc in live_changes
	endswith(rc.type, "_iam_policy")
	is_string(rc.change.after.policy_data)
	some b in json.unmarshal(rc.change.after.policy_data).bindings
}

deny contains msg if {
	some [addr, role] in granted_roles
	role in primitive_roles
	msg := sprintf("%s: primitive role %s is not allowed; grant a predefined role on the narrowest resource", [addr, role])
}

deny contains msg if {
	some [addr, role] in granted_roles
	contains(role, "*")
	msg := sprintf("%s: wildcard role %s is not allowed", [addr, role])
}

deny contains msg if {
	some rc in live_changes
	is_iam_resource(rc.type)
	some member in iam_members(rc)
	contains(member, "*")
	msg := sprintf("%s: wildcard member %s is not allowed", [rc.address, member])
}

deny contains msg if {
	some rc in live_changes
	is_iam_resource(rc.type)
	not startswith(rc.type, "google_cloud_run")
	some member in iam_members(rc)
	member in public_members
	msg := sprintf("%s: %s is not allowed on %s", [rc.address, member, rc.type])
}

deny contains msg if {
	some rc in live_changes
	endswith(rc.type, "_iam_custom_role")
	some p in rc.change.after.permissions
	contains(p, "*")
	msg := sprintf("%s: custom role permission %s uses a wildcard", [rc.address, p])
}

deny contains msg if {
	some rc in live_changes
	rc.type in {"google_project_iam_policy", "google_organization_iam_policy", "google_folder_iam_policy"}
	msg := sprintf("%s: authoritative IAM policies are not allowed; use *_iam_member", [rc.address])
}

# Service account keys are long-lived credentials; CI uses Workload Identity
# Federation instead (spec 17.2 sa-ci).
deny contains msg if {
	some rc in live_changes
	rc.type == "google_service_account_key"
	msg := sprintf("%s: service account keys are not allowed; use Workload Identity Federation", [rc.address])
}
