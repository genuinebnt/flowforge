# storage-bucket

The per-environment artifact bucket (spec 17.1): uniform bucket-level access,
`public_access_prevention = enforced`, soft delete, optional CMEK
(`kms_key_name`).

`lifecycle_rules` deletes objects under a prefix after a number of days. The
environments use:

| Prefix | Content | Age |
|--------|---------|-----|
| `raw/` | raw tool output (tool-runner writes, graph reads) | 90 days |
| `engine/archive/events/` | archived run events (zstd JSON Lines, spec 10) | 400 days |

`iam` grants object-level roles only; a `prefix` turns the grant into an IAM
condition on `resource.name.startsWith(...)`, which implements spec 17.2's
"on its prefix" grants. Note that object listing is checked against the
bucket, so prefix-scoped principals must address objects by name.
