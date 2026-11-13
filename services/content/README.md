# services/content

The content service: `flowforge.v1.ContentService` over `contentstore`
(V2 design §6-§8, §11 items 13, 14, 18). RPCs, authorization, events
(`content.changes` → graph and notifier), background loops, gateway REST
routes, environment variables and what the v2 tools should call are in
`services/README.md` ("Content"); storage, diffs, search, quotas and the
invariants in `contentstore/README.md`.

Tests: `TestContentService_EndToEnd` (in-memory dev stack: ingest over
gRPC, graph observations and `content_changed` through the bus, one alert
per alertable change, diff views, search, first appeared, raw-read
authorization, pinning, paging, quotas, tenant isolation) and
`TestContentService_EndToEndPostgres` (the same on Postgres, with
`FF_TEST_PG_DSN`); the REST routes are covered by
`services/gateway/gatewayd` `TestGatewayContentRoutes`.

Limits: one replica per tenant set (chunkstore single writer), no GCS chunk
backend yet, no Terraform (see `contentstore/OPEN_QUESTIONS.md` and
`services/README.md`).
