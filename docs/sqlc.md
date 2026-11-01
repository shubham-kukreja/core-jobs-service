# Typed queries (sqlc)

Each module that owns tables keeps its own sqlc setup:

```
internal/<module>/
  sqlc.yaml            # config below
  queries/*.sql        # named queries
  internal/db/         # generated (package db); private to the module
```

`internal/<module>/sqlc.yaml`:

```yaml
version: "2"
sql:
  - engine: postgresql
    schema: ../../migrations
    queries: queries
    gen:
      go:
        package: db
        out: internal/db
        sql_package: pgx/v5
        emit_pointers_for_null_types: true
        omit_unused_structs: true
        overrides:
          - {db_type: uuid, go_type: github.com/google/uuid.UUID}
          - {db_type: uuid, nullable: true, go_type: {import: github.com/google/uuid, type: UUID, pointer: true}}
          - {db_type: timestamptz, go_type: time.Time}
          - {db_type: timestamptz, nullable: true, go_type: {type: time.Time, pointer: true}}
```

Run `make sqlc` after editing queries; CI runs `make sqlc-check`. Queries
are always parameterised. Never build SQL from strings.
