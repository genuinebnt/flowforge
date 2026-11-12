# cloud-sql

Cloud SQL for PostgreSQL 16 (spec 17.1): private IP only (`ipv4_enabled =
false`, private services access range), `ssl_mode = ENCRYPTED_ONLY`,
`REGIONAL` availability when `high_availability` (prod), automated backups
with point-in-time recovery, query insights, one database (`flowforge`).

For every entry in `users` it creates a built-in user `ff_<service>` with a
generated 32-character password. The caller stores the passwords in Secret
Manager as `ff-db-<service>-password`; they never appear in tfvars.

Schema creation, extensions (`pg_trgm`, `citext`, `vector`) and the
per-schema grants (each role gets USAGE and DML on its own schema only, and is
removed from `cloudsqlsuperuser`) are done by the migrations job, not by
Terraform: the Google provider cannot issue SQL.

Services connect through the Cloud Run built-in Cloud SQL connector
(`/cloudsql/<connection_name>`), which needs only `roles/cloudsql.client`.
