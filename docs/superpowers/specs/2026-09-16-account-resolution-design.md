# Account-Aware Identity Resolution Design

Date: 2026-09-16
Status: approved (design phase)

## Background

Production incident (network `gay`, ergo): an authenticated user (`shrew`, account
`shrew`, user_id 11) cycled PART/JOIN. At JOIN time the bot resolved them with
`account == ""`, missed nick lookup (their row was active under a different nick)
and host recovery (new cloak host), and created ghost row 990. The account only
attached on their next message ~68s later, and `claimNickFor` had to release the
ghost via `handleNickCollision`.

Investigation found three root causes:

1. **girc handler concurrency race.** girc dispatches all handlers for one event
   concurrently (`handler.go` exec(): every internal and external handler is
   launched as its own goroutine, "no specific order/priority"). dave's JOIN
   handler read `client.LookupUser(nick).Extras.Account` racing against girc's
   internal `handleJOIN`, which is what creates the state user and copies the
   extended-join account param into `Extras.Account`. Even SASL-authed users on
   ergo (which always sends `JOIN #chan <account> :<realname>` to extended-join
   clients) can resolve with an empty account.
2. **No handlers for ACCOUNT / 354 (WHOX) / CHGHOST.** girc learns the account
   asynchronously (WHOX reply to its per-join `WHO nick %tacuhnr,1`, `@account`
   message tags, `ACCOUNT` notifications) but dave never re-resolves, so ghost
   rows persist until the user's next interaction.
3. **Host-based recovery conflates shared hosts.** On networks where ident is
   coerced (`~u`) and hosts are shared cloaks/vhosts, `recoverByKnownHost`
   matches `(ident, host)` across different people. Account-bound rows receive
   no protection from unauthed (or differently-authed) strangers, who inherit
   the row's sessions/bans/history. The released-nick fallback has the same
   hole (documented in AGENTS.md as accepted risk pending "Phase 5").

## Decisions (from Q&A)

- Handle all three account-arrival paths: JOIN event params, WHOX (354)
  deferral, and ACCOUNT messages.
- On ircds without WHOX support (plain 352 replies): do nothing — no row is
  created until first interaction. A WHOIS-based fallback is deferred (no
  current network lacks WHOX).
- Also handle CHGHOST (record new ident@host into known hosts).
- Host recovery eligibility: a candidate row with `IRCAccount != ""` is only
  eligible when the incoming account equals it (blocks conflation in both
  directions).
- The same rule applies to the released-nick fallback (closes the documented
  security hole; unauthed nick-reusers get fresh rows).

## Design

### 1. Account sourcing — `accountFromEvent`

New helper replacing the direct `LookupUser(...).Extras.Account` read in
`resolveIRCUser` (main.go). Resolution order, all race-free for the JOIN path:

1. JOIN events with `len(Params) >= 2`: use `Params[1]`; `"*"` means `""`.
2. Otherwise, `event.Tags.Get("account")` if present (ergo tags messages with
   the account when `account-tag` is negotiated; girc requests it).
3. Otherwise `client.LookupUser(nick).Extras.Account` as a last resort
   (best-effort; may race but only used on paths where nothing better exists).

### 2. JOIN handler (irc_handlers.go)

- Bot's own join: unchanged (skip).
- Account available from params or tag (per helper above): resolve immediately
  with that account, ident/host from `event.Source`. No girc state read.
- No account info anywhere: record a pending deferral
  `pendingJoinWHO[network][normNick] = now` and return without resolving.
  girc already sends `WHO nick %tacuhnr,1` per foreign join; no extra traffic.
- Pending map: mutex-guarded, keyed by normalized nick per network client,
  entries pruned lazily (TTL ~60s) so stale entries from netsplits/no-WHOX
  servers cannot accumulate unboundedly.

### 3. WHOX handler (RPL_WHOSPCRPL / 354)

- If the reply's nick matches a pending deferral: resolve using account
  (`"0"` → `""`), ident, and host from the reply params (same 8-param layout
  girc's builtin parses: `[client, querytype, channel, ident, host, nick,
  account, realname]`), then clear the entry.
- Non-pending 354s (e.g. channel-wide WHO replies from the bot's own joins) are
  ignored — keeps scope tight.

### 4. ACCOUNT handler (girc.CAP_ACCOUNT)

- `ACCOUNT *` (logout): ignored — the row keeps its account as the identity
  key; nick lookup still serves subsequent messages.
- Otherwise: re-resolve the nick immediately with the new account
  (ident/host from `event.Source`). This retro-fixes ghosts created before the
  account was known and attaches accounts for mid-session authentication.

### 5. CHGHOST handler (girc.CAP_CHGHOST)

- Look up the active user by normalized nick; if found,
  `upsertKnownHost(userID, Params[0], Params[1])`. No full re-resolution —
  identity did not change, only the host. If no row exists (user never
  interacted and no pending deferral), nothing to do; the pending-354 path
  carries the fresh host anyway.

### 6. Eligibility rule (users.go)

`recoverByKnownHost` and `getMostRecentReleasedUserByNormalizedNick` (plus the
account branch's released fallback) skip candidate rows where
`row.IRCAccount != "" && row.IRCAccount != incomingAccount`.

Consequences:

- Unauthed stranger on a shared cloak → fresh row (no inheritance).
- Differently-authed user on a shared cloak → fresh row (no account overwrite).
- Authed user matching an account-less row → still matches; account attaches
  (existing `host_recovery+account_link` behavior preserved).
- Released account-bound rows can only be reactivated by the same account.

No DB schema changes; the rules are query-level filters.

### 7. Handler registration

All new handlers registered in `registerIRCHandlers` (irc_handlers.go) which
already receives `bot`, `client`, `network`, and `log`. dave handlers use the
event payload directly (params/tags), so girc's internal concurrent handlers
cannot race them.

### 8. Documentation

Update AGENTS.md: the released-nick fallback security note changes (account
rule applied), and the user-resolution description gains the
event-sourced/deferred account paths.

## Testing

- `accountFromEvent`: param parsing (`*` → empty), tag fallback, state
  fallback, precedence.
- JOIN: immediate resolution with params account; deferral recorded when no
  account info; pending entry consumed by 354 with WHOX account/ident/host;
  stale pending entries pruned.
- ACCOUNT: re-resolve with new account; logout ignored.
- CHGHOST: known host upserted for active user; no-op for unknown users.
- Eligibility: table-driven tests for host recovery (unauthed vs matching vs
  differing account vs account-less row) and released-nick fallback (same
  matrix), including the existing account_link corner cases which must keep
  working.

## Out of scope

- WHOIS-based fallback for ircds without WHOX.
- Any DB migration (no schema change).
- Phase 5 account system beyond these eligibility rules.
