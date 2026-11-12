# pgrepo

PostgreSQL adapters of the engine ports (spec 10.4): `Repo` (engine.Repo),
`Tools` (engine.ToolRegistry over engine.tools) and `Outbox`
(engine.Publisher over engine.outbox). Use with `eventstore/pgstore` on the
same database: pgstore owns the event log and the scheduling columns of
engine.runs, Repo the rest of the projection.

`EnsureSchema` applies `SchemaSQL`, a proposed migration
(`0013_engine_ext`) for tables the spec does not define: run requests,
fine-grained run status and outbox watermark, version and approval actors.

```sh
eval "$(scripts/devpg.sh start)"
go test ./engine/pgrepo/      # eng_001 (golden log), eng_004, eng_008 on Postgres
scripts/devpg.sh stop
```

Limitations: "latest tool version" is the greatest version text; the
`created_by`/`decided_by` uuid columns hold the uuid of `user:<uuid>`
actors only (the full actor is in the ext tables). See
`engine/OPEN_QUESTIONS.md`.
