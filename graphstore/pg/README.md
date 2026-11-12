# graphstore/pg

Postgres implementation of `graphstore.Store` over the spec 10.5 graph
schema and the 21.6 snapshot catalogue, plus the change-event outbox
`Relay`. See `../README.md` for usage, tests and numbers and
`../OPEN_QUESTIONS.md` for the storage decisions (hidden bookkeeping tags,
locking protocol, plan stability).

```sh
FF_TEST_PG_DSN=postgres://postgres@127.0.0.1:55432/postgres?sslmode=disable go test -race ./graphstore/pg/
```
