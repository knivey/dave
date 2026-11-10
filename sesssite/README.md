# sesssite — session-history website for dave

A read-only web view over dave's database: full session transcripts
(markdown, IRC formatting, tool calls, reasoning, images, token usage,
compaction dividers), a per-user page listing every session, and live
SSE updates while conversations continue. Pages are unlisted — URLs
carry random capability slugs that dave mints and hands out on request
(`^link` in channel); anyone holding a link can view.

    go build -o sesssite/sesssite ./sesssite
    ./sesssite [--config <file>]     # config.toml next to the binary by default

## Read-only contract

The site NEVER writes dave's database — dave is the single writer and
the sole minter of slugs; sesssite only validates them. There is no
write path, no login, and no mutation surface of any kind.

- **sqlite**: the database is opened `mode=ro` (WAL readers work while
  dave is running, so the two processes coexist on one file). If the
  `mode=ro` open fails — dave is stopped and no `-wal`/`-shm` sidecar
  exists — the site falls back to a normal open with
  `PRAGMA query_only=1`, which rejects writes at the SQL level. Either
  way the contract holds: the site physically cannot modify the data.
- **postgres**: connect with a DSN belonging to a **read-only role**
  (`GRANT SELECT ON ALL TABLES IN SCHEMA public TO sesssite_ro`, no
  INSERT/UPDATE/DELETE). The code only ever issues SELECTs; the role
  recommendation is defense in depth.

## Config

Config is a TOML file (`config.toml` next to the binary by default, or
`--config <path>`; paths in the file resolve relative to the binary).
See `example.toml` for the same reference in comment form.

```toml
[server]
# addr            listen address (default ":8060")
addr = ":8060"
# base_url        external base URL for absolute links/OG tags
base_url = "https://sessions.example.com"
# trusted_proxies peers whose X-Forwarded-For keys the SSE per-IP cap
#                 (default loopback ["127.0.0.1", "::1"];
#                 empty list [] = never trust XFF, key on RemoteAddr)
trusted_proxies = ["127.0.0.1", "::1"]

[database]
# driver  "sqlite" (default) | "postgres"
driver = "sqlite"
# path    sqlite path, binary-relative (default "data/dave.db")
path = "data/dave.db"
# dsn     postgres DSN — use a READ-ONLY role; the code never writes
# dsn = "postgres://sesssite_ro:pass@localhost:5432/dave?sslmode=disable"
```

`trusted_proxies` matters when the site sits behind a reverse proxy:
every viewer then shares the proxy's address, so the SSE connection cap
(10 per IP) keys on `X-Forwarded-For` — but only when the direct peer
is a listed proxy. Forged `X-Forwarded-For` from an untrusted peer is
ignored and cannot multiply capacity.

## Deploying

Run the binary on the same host as dave (or anywhere that can reach the
database), behind a reverse proxy for TLS. The SSE streams send
`X-Accel-Buffering: no` themselves, which nginx honors with zero extra
proxy config — without it, proxy response buffering silently stalls
live updates:

```nginx
server {
    listen 443 ssl;
    server_name sessions.example.com;

    location / {
        proxy_pass http://127.0.0.1:8060;
        proxy_http_version 1.1;
        proxy_set_header Connection "";
        proxy_read_timeout 1h;   # SSE streams are long-lived
    }
}
```

On the dave side, set the site's base URL to activate the IRC
integrations (`[website] url` in dave's `config/config.toml`):

- over-long replies link to their session page
  (`<url>/s/<slug>#m<id>`) instead of uploading to the pastebin
  (ephemeral generator output still uses the pastebin — there is no
  session row to link);
- `^link` in channel hands the caller their active-session URL and
  user-page URL.

## Link lifetime caveat

Session links live exactly as long as the session rows. dave's
`[database] max_age_days` cleanup (default 90) deletes old sessions —
once the row is gone, its link 404s like any unknown slug. Treat a
pasted session URL as durable for weeks, not forever; archive anything
you care about.
