# CLI / Agent Mode

Golazo ships with a small set of subcommands that emit JSON to stdout. They are intended for agents (Claude Code, Codex, scripts, CI) that need structured match data without driving the TUI.

The default `golazo` invocation still opens the TUI — the subcommands below are additive.

## When to use Golazo

Use this tool when the user asks about football (soccer) matches. Map their question to the right command:

| User asks about... | Command |
|---|---|
| Matches happening right now | `golazo live` |
| Today's results (already finished) | `golazo finished --days 1` |
| Today's full slate (finished + still-to-come) | `golazo finished --include-upcoming` |
| Results over the last N days (≤7) | `golazo finished --days N` |
| Details for a specific match (events, lineups, stats) | `golazo match <id>` — reliable for IDs discovered by `live`/`finished`, see [Match details across processes](#match-details-across-processes) |
| Which competitions are tracked / what league IDs exist | `golazo leagues` (or `--all`) |

If the user's question doesn't map to one of the above, this tool likely cannot answer it. Golazo does not expose: standings/tables, head-to-head history, individual player stats, transfer news, or fixtures beyond today.

## Quick start (worked example)

The reliable agent flow is **discover → detail**. `live` and `finished` return all the per-match metadata an agent typically needs (teams, score, status, kickoff time, league, round), and every discovered match's location is persisted to disk, so a follow-up `golazo match <id>` in a separate process resolves the same match page reliably (see [Match details across processes](#match-details-across-processes)).

```bash
# 0. (Optional but recommended) Self-discover the CLI contract
golazo capabilities | jq '.data[0].commands'

# 1. Discover which competitions are active
golazo leagues

# 2. Get today's full slate across active leagues — this is usually enough
golazo finished --include-upcoming | jq '.data[] | {
  league: .league.name,
  status,
  home: .home_team.name,
  away: .away_team.name,
  score: (if .home_score != null then "\(.home_score)-\(.away_score)" else null end),
  kickoff_utc: .match_time
}'
```

For events / lineups / stats: run `golazo live` or `golazo finished` first to discover and persist match IDs, then call `golazo match <id>`. To validate your jq pipeline without any network, use `--mock` with the bundled mock IDs (2001, 2002, ...).

## Subcommands

> **Agent note for `match <id>`:** call `live` or `finished` first — each discovered match's ID, league, and page path is persisted to a shared on-disk index, so a later `match <id>` invocation (even in a separate process) resolves the match page directly. Stale page paths are repaired automatically via the known league. Only arbitrary IDs never seen by a list command still hit the legacy API fallback and may return `upstream_error`. See [Match details across processes](#match-details-across-processes) for the full contract.

| Command | Description |
|---|---|
| `golazo live` | Live matches across active leagues |
| `golazo finished [--days N] [--include-upcoming]` | Finished matches over the last N days (1..7, default 1); use `--include-upcoming` to also include today's not-yet-started matches |
| `golazo match <id>` | Full match details (events, lineups, stats) |
| `golazo leagues [--all]` | Active leagues (or every supported league) |
| `golazo capabilities` | Machine-readable contract describing every subcommand, flag, error code and env var — call this once at session start to self-discover the CLI |

### Common flags

| Flag | Description |
|---|---|
| `--mock` | Use bundled mock data, no network |
| `--debug` | Emit debug logs to stderr |
| `--timeout <dur>` | Overall request timeout (default `15s`) |
| `--pretty` | Indent JSON output |

## JSON contract

### Success envelope

```json
{
  "status": "ok",
  "count": 2,
  "data": [ ... ]
}
```

Single-item responses (e.g. `match <id>`, `capabilities`) still use a `data` array with `count: 1`. **This is intentional** — every subcommand returns the same envelope shape so agents can write a single parser. Access singleton results with `.data[0]`:

```bash
golazo capabilities | jq '.data[0].tool'            # → "golazo"
golazo match 2001 --mock | jq '.data[0].home_team'  # → match details
```

### Degraded envelope

`finished` over multiple days may partially fail. When at least one day succeeds, the envelope is flagged degraded with the failing dates listed:

```json
{
  "status": "ok",
  "degraded": true,
  "failed_dates": ["2026-06-10"],
  "count": 12,
  "data": [ ... ]
}
```

### Error envelope

Errors go to **stderr**, stdout stays empty:

```json
{
  "status": "error",
  "code": "not_found",
  "message": "no match found for id 99999999"
}
```

Error codes: `invalid_args`, `not_found`, `upstream_error`, `timeout`, `offline`.

CLI-level failures (typo'd subcommand, unknown flag, bad flag value) also flow through this envelope as `invalid_args` (exit code 2). Agents can always parse stderr as JSON when the exit code is non-zero.

## Data schema

Every command's `data` array contains one of three object shapes. All field names are stable across calls. Fields marked `null when ...` are present but null in those states — agents should always nil-check.

### `Match` (returned by `live`, `finished`)

```yaml
id:          int        # FotMob match ID — pass to `golazo match`
league:
  id:        int
  name:      string     # e.g. "Premier League"
  country:   string     # e.g. "England", "International", "Europe"
home_team:
  id:        int
  name:      string     # full name, e.g. "Manchester United"
  short_name: string    # abbreviated, e.g. "Man Utd"
away_team:   { same shape as home_team }
status:      string     # one of: "live" | "finished" | "not_started" | "postponed" | "cancelled"
home_score:  int|null   # null when status == "not_started"
away_score:  int|null   # null when status == "not_started"
match_time:  string     # RFC3339 timestamp in UTC, e.g. "2026-06-12T19:00:00Z"
live_time:   string|null # null unless status == "live"; e.g. "45+2", "HT", "67'"
round:       string     # e.g. "Matchday 17", "Round of 16"
page_url:    string     # FotMob page slug, e.g. "/matches/team-a-vs-team-b/abc123".
                        # Persisted alongside match_id and league to the shared
                        # match-locations index so a later `golazo match <id>`
                        # process can fetch the match page without re-discovery.
```

### `MatchDetails` (returned by `match`)

`MatchDetails` embeds every `Match` field above and adds:

```yaml
events:
  - id:             int
    minute:         int          # base minute, e.g. 45
    display_minute: string       # formatted, e.g. "45+2'"
    type:           string       # "goal" | "card" | "substitution"
    team:           Team
    player:         string|null
    assist:         string|null
    event_type:     string|null  # "yellow" | "red" | "in" | "out"
    own_goal:       bool|null
    timestamp:      string       # RFC3339
home_lineup:        []string     # player names (legacy; prefer home_starting)
away_lineup:        []string
home_starting:                   # full lineup detail
  - { id, name, number, position, rating }
away_starting:      [...]
home_substitutes:   [...]
away_substitutes:   [...]
home_formation:     string       # e.g. "4-3-3"
away_formation:     string
home_score / away_score:         # final score (always set for finished)
half_time_score:    { home, away } | absent
penalties:          { home, away } | absent
venue:              string
referee:            string
attendance:         int
match_duration:     int          # 90, 120
extra_time:         bool
statistics:                       # possession, shots, etc.
  - { key, label, home_value, away_value }
home_xg / away_xg:  number|absent
highlight:                        # YouTube/source link if available
  { url, image, source, title } | absent
aggregate_score:    string       # two-legged ties only, e.g. "5 - 7"
who_lost_on_aggregate: string    # team name eliminated
winner:             "home"|"away"|null
```

### `League` (returned by `leagues`)

```yaml
id:           int
name:         string
country:      string
country_code: string   # often empty for international competitions
```

## Exit codes

| Code | Meaning |
|---|---|
| `0` | Success |
| `1` | Upstream / unknown error |
| `2` | Invalid arguments |
| `3` | Not found |
| `4` | Timeout |
| `5` | Offline (network disabled via env) |

## Environment variables

| Var | Effect |
|---|---|
| `GOLAZO_AGENT=1` | Forces compact JSON, enables stderr debug logging |
| `GOLAZO_OFFLINE=1` | Refuses any network call; subcommands return `offline` unless `--mock` is set |

### Recommended agent invocation

For any non-human caller, always set `GOLAZO_AGENT=1`:

```bash
GOLAZO_AGENT=1 golazo <subcommand> [flags]
```

This single env var:

- Forces compact (single-line) JSON regardless of `--pretty` — lower token cost
- Routes all debug logging to stderr so stdout stays clean for `jq` pipelines
- Acts as a future-proof "I am not a human" signal — if Golazo ever grows interactive prompts or color escapes, this flag will suppress them

A repository-root `tool.json` manifest also describes the CLI for agent indexers; see the top of [the repo](https://github.com/0xjuanma/golazo/blob/main/tool.json).

## Determinism guarantees

Agents that diff outputs across calls (e.g. "did the score change since five minutes ago?") need to know what's stable and what's not. Within a single match:

| Stable across calls | Changes during/after a match |
|---|---|
| `id`, `league`, `home_team`, `away_team` | `status` |
| `match_time` (kickoff time), `venue`, `referee` | `home_score`, `away_score` |
| `home_formation`, `away_formation` (once published) | `live_time` |
| Starting lineup IDs (once published) | `events[]` (appends as goals/cards happen) |
|  | `statistics[]` |
|  | `home_xg`, `away_xg` |

List endpoints (`live`, `finished`, `leagues`) sort by `match_time` then `id` for deterministic ordering — repeated invocations produce diffable output. `leagues --all` sorts by ID.

## Examples

### Basic invocations

```bash
# Live matches, compact JSON
golazo live

# Finished matches over the last 3 days, indented
golazo finished --days 3 --pretty

# Today's full slate (finished + still-to-come)
golazo finished --include-upcoming

# Single match details (use an ID discovered by live/finished; --mock works offline)
golazo match 2001 --mock

# Discover league IDs to interpret results
golazo leagues --all

# Agent mode + offline safety in CI
GOLAZO_AGENT=1 GOLAZO_OFFLINE=1 golazo live --mock
```

### jq recipes

Most agent flows pipe Golazo's output through `jq`. These are the patterns worth memorizing:

```bash
# One-line live score summary
golazo live | jq -r '.data[] | "\(.home_team.name) \(.home_score)-\(.away_score) \(.away_team.name) [\(.live_time)]"'

# Filter by league name (case-insensitive match)
golazo finished --include-upcoming \
  | jq '[.data[] | select(.league.name | test("World Cup"; "i"))]'

# Filter by status (e.g. only what's still to come)
golazo finished --include-upcoming \
  | jq '[.data[] | select(.status == "not_started")]'

# Extract just IDs, ready for chaining to `match`
golazo finished --days 3 | jq -r '.data[].id'

# Check whether the result was degraded (partial-failure-aware retry decision)
golazo finished --days 7 | jq '{ok: ((.degraded // false) | not), failed: (.failed_dates // [])}'

# Goal events only, ordered by minute (mock ID example)
golazo match 2001 --mock \
  | jq '.data[0].events | map(select(.type == "goal")) | sort_by(.minute)'
```

## Notes

- Stdout receives **only** the JSON envelope. All logs go to stderr — safe to pipe through `jq`.
- List output is sorted deterministically (`match_time` then `id`) so repeated invocations diff cleanly.
- The TUI experience is unchanged — no flags here alter the interactive default.

## Failure modes & retry policy

Use this table to decide whether to retry, fix the call, or give up.

| Error code | Exit | Typical cause | Should agent retry? |
|---|---|---|---|
| `invalid_args` | `2` | Bad flag value (e.g. `--days 99`, non-numeric match ID) | **No** — fix the call. Retrying will keep failing. |
| `not_found` | `3` | Unknown match ID (mock mode), or match has no data | **No** — pick a fresh ID via a list call. |
| `timeout` | `4` | Upstream slow or network congested | **Yes**, with a larger `--timeout` (e.g. `--timeout 30s`). |
| `upstream_error` | `1` | FotMob 4xx/5xx, network failure, Cloudflare challenge | **Once** — transient errors recover. For `match <id>` on an ID never discovered by `live`/`finished`, a 404 from the legacy fallback is expected; run a list command first (see [Match details across processes](#match-details-across-processes)). |
| `offline` | `5` | `GOLAZO_OFFLINE=1` is set | **No** — unset the env var, or pass `--mock` for synthetic data. |

The exit code is the most reliable retry signal. The `code` field in the error envelope on stderr says the same thing in machine-readable form; agents should prefer the exit code (no JSON parsing required).

## "No matches" vs "failure" semantics

Two outputs look superficially similar but mean very different things:

```json
{"status":"ok","count":0,"data":[]}            // success, just nothing today
{"status":"error","code":"upstream_error",...} // failure on stderr, exit 1
```

A `count: 0` result means **the request succeeded and there genuinely are no matches** matching the criteria (off-season, no World Cup games today, no live matches at 4am, etc.). It is **not** a silent failure. Do not retry on `count: 0` — you will get the same answer.

Conversely, partial failures (`finished` over multiple days where some days fetched and others didn't) are surfaced as `degraded: true` with a `failed_dates` array — those are still exit code 0, but agents can choose to retry just the failed dates.

## Rate limiting

Golazo internally rate-limits FotMob requests to one every 200ms and caps concurrent requests at 10. Agents calling subcommands in tight loops will not be rejected — requests just queue. There is **no** explicit `rate_limited` error code today. If FotMob itself rate-limits the underlying client, that surfaces as `upstream_error`.

## Match details across processes

`live` and `finished` discover every match's page path (the FotMob match-page slug) while parsing league pages. Those locations were previously held only in the client's in-memory map, so a later one-shot `golazo match <id>` process started cold and fell back to a removed API endpoint. Golazo now persists the location contract to disk.

### The on-disk location index

- **File:** `match-locations.json` in the golazo config directory (`~/.golazo/` on macOS/Linux, `%APPDATA%\golazo` on Windows).
- **Record:** `match_id`, `league_id`, `page_url` (page path), and an `updated_at` timestamp.
- **Bound:** at most 500 entries; the least recently updated records are evicted first, and records expire after 30 days.
- **Durability:** writes go through a temp file plus atomic rename, so readers — including other processes — never see a half-written file. Concurrent writers serialize per path and reload-merge before writing, so one process cannot erase another process's records. A corrupted file degrades to an empty index and is repaired on the next write; one malformed record never invalidates the others.

### Resolution order for `golazo match <id>`

1. **In-process slug map** — populated when the same process listed the match (the interactive TUI path).
2. **Persistent location index** — cold-start read of the match's stored page path.
3. **League re-location** — if the stored path returns an error page, the client re-fetches the known league page, extracts the match's current page path from its fixtures, repairs both the in-memory map and the on-disk index, and retries.
4. **Legacy API fallback** — only for IDs never discovered by a list command; this endpoint is removed upstream and typically returns 404.

### Identity and cache safety

Every page-derived response is checked against the requested match ID before it is returned or cached. A non-200 error page, unparseable props, or a 200 page whose embedded match differs from the requested ID is rejected and never enters the details cache; the call then proceeds to re-location or the API fallback.

### What this means for agents

- The reliable flow remains **discover → detail**: `golazo live` or `golazo finished` first, then `golazo match <id>` — even in a separate OS process, minutes later.
- `--mock` stays fully hermetic and works offline with the bundled mock IDs (e.g. `golazo match 2001 --mock`).
- Arbitrary IDs that no list command has ever seen cannot be located and still rely on the legacy fallback.

## Debug logging is sparse on list endpoints

`--debug` and `GOLAZO_AGENT=1` only emit logs at the FotMob client's match-details fetch path. The list endpoints (`live`, `finished`, `leagues`) are largely silent. This is by design — agents are expected to interpret the JSON envelope (including `degraded` / `failed_dates`), not stderr logs.
