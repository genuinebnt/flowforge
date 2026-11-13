# cloud-sql

Cloud SQL for PostgreSQL 16 (spec 17.1): private IP only (`ipv4_enabled =
false`, private services access range), `ssl_mode = ENCRYPTED_ONLY`,
`REGIONAL` availability when `high_availability` (prod), automated backups
with point-in-time recovery, query insights, one database (`flowforge`).

For every entry in `users` it creates a built-in user `ff_<service>` with a
generated 32-character password. The caller stores the passwords in Secret
Manager as `ff-db-<service>-password`; they never appear in tfvars.

Schema creation, extensions (`pg_trgm`, `citext`, `vector`) and the
per-schema grants are done by the migrations job (`cmd/migrate`, migration
`0018_v1_db_roles.sql` and `public.ff_sync_roles()`), not by Terraform: the
Google provider cannot issue SQL. Each service role gets USAGE and DML on its
own schema only, PUBLIC gets nothing, row-level security is forced, and the
service roles are removed from `cloudsqlsuperuser`; the job fails if a
service role is left a superuser, `cloudsqlsuperuser` member, or table owner
(see `migrations/README.md`). Run the job as the `migrator` user, which owns
the tables.

Services connect through the Cloud Run built-in Cloud SQL connector
(`/cloudsql/<connection_name>`), which needs only `roles/cloudsql.client`.
