# SQLite to PostgreSQL Migration

This tool copies all data from an existing SQLite database into a fresh PostgreSQL database.

## Building

```bash
go build -o tools/migrate-sqlite-to-pgsql/migrate-sqlite-to-pgsql ./tools/migrate-sqlite-to-pgsql
```

## Usage

```bash
./tools/migrate-sqlite-to-pgsql/migrate-sqlite-to-pgsql \
  --sqlite data/dave.db \
  --postgres "postgres://user:pass@localhost:5432/dave?sslmode=disable"
```

### Flags

| Flag | Description |
|------|-------------|
| `--sqlite` | Path to the source SQLite database file (required) |
| `--postgres` | PostgreSQL connection DSN (required) |
| `--dry-run` | Count rows in each table without writing anything |

### Dry Run

Verify row counts before committing:

```bash
./tools/migrate-sqlite-to-pgsql/migrate-sqlite-to-pgsql \
  --dry-run \
  --sqlite data/dave.db \
  --postgres "postgres://user:pass@localhost:5432/dave?sslmode=disable"
```

## What It Does

1. Opens the source SQLite database (read-only via GORM)
2. Connects to the target PostgreSQL database
3. Runs GORM AutoMigrate on PostgreSQL to create the schema
4. Disables FK checks (`session_replication_role = replica`) for bulk insert speed
5. Copies tables in FK-safe order:
   - `sessions`
   - `messages`
   - `pending_jobs`
   - `turn_usage`
6. Re-enables FK checks
7. Resets PostgreSQL sequences to `MAX(id)` so future inserts get correct IDs

## Switching Dave to PostgreSQL

After running the migration, update `config/config.toml`:

```toml
[database]
driver = "postgres"
dsn = "postgres://user:pass@localhost:5432/dave?sslmode=disable"
max_age_days = 90
```

Remove or comment out the `path` line — it's only used when `driver = "sqlite"`.

## Notes

- The source SQLite database is never modified
- The target PostgreSQL database must exist but should be empty (AutoMigrate creates tables)
- Timestamps are read as `time.Time` by GORM — no manual string parsing
- Messages are inserted in batches of 500 for performance
- The tool does not delete or truncate existing data in the target — run against a fresh database
