# Token Usage

[中文](README.md) | **English**

Local token usage and API-equivalent cost for **Codex**, **Claude Code** and **Antigravity**, on your own machine. All three sources live in one local dashboard: view them combined or switch to a single source. Nothing leaves the computer.

![Token Usage overview](docs/images/dashboard.png)

## Features

- **Source switcher**: icon buttons in the header switch between **All · Claude · Codex · Antigravity**. The choice is remembered in the browser; `?provider=claude` in the URL opens a source directly.
- **Overview**: today, 7 days, 30 days, all time, or a custom range to the minute. Total tokens with Input / Cached / Cache Write / Output / Reasoning, API-equivalent cost with pricing coverage, and hourly and daily trends.
- **Daily**: calendar heat map; pick a day for its model mix and cost.
- **Details**: group by model, source, agent, project or thread; session list and task tree; session search.
- **Export**: CSV or JSON for the current source and filters.
- Traditional Chinese / English, light / dark theme, type size and density settings.

![Daily usage](docs/images/daily.png)

## What is counted

| Source | Local data read | Recorded |
|---|---|---|
| Codex | JSONL under `sessions/` and `archived_sessions/` in `CODEX_HOME` (default `~/.codex`), plus paths and titles from the Codex state database | Token categories, model, source, project, thread, agent, Fast mode |
| Claude Code | JSONL under `~/.claude/projects` and `~/.config/claude/projects` (including subagents and SSH sessions synced locally) | Token categories, model, 5-minute / 1-hour cache writes, thinking tokens, speed and inference region |
| Antigravity | `~/.gemini/antigravity*/brain/*/.system_generated/logs/transcript.jsonl` and the matching `conversations/*.db` | Token categories, model, thinking tokens, model selections |

**All** is the sum of the three sources. A model reported by both Claude Code and Antigravity (for example `claude-opus-5-5`) is merged into one row when grouping by model.

Replies, reasoning text and tool output are never stored. The Claude Code and Antigravity ledgers keep only token counts, models, times and hashed identifiers; Codex also keeps the conversation titles and project paths that Codex itself records, and a title is usually the first prompt. When Antigravity has no native counts, its text is read to estimate tokens from character counts, and is not stored either. Credentials such as `auth.json`, real bills and account quotas are never read.

Deduplication, attribution and pricing rules are described in [Accounting and pricing](docs/accounting.md) (Chinese).

## Quick start

### 1. Build

Requires Go 1.26 or later.

```bash
go build -o codex-usage.exe ./cmd/codex-usage
```

Or run `scripts/build.ps1` (Windows) / `scripts/build.sh` (Linux, macOS); both run the full test suite and write binaries for every platform to `dist/`.

### 2. Choose where data lives

Create a `data` folder beside the executable to run in **portable mode**: all three ledgers are kept in that folder, so the program and its data can sit together in OneDrive or another synced folder. Without a `data` folder, data goes to the per-user application data folder (see "Where data lives").

### 3. Install and start

```bash
codex-usage install
```

This runs a full scan of Codex records, registers a per-user background start at login and starts the service, which then scans Claude Code and Antigravity. No administrator rights are needed. In portable mode the program runs where it is; otherwise it is copied into your user folder.

### 4. Open

```bash
codex-usage
```

Or open **http://127.0.0.1:43189/**.

## Commands

```text
codex-usage                         Open the dashboard
codex-usage summary --since 7d      Codex summary for the last 7 days (--json / --csv)
codex-usage scan                    Incrementally scan all three sources
codex-usage scan --rebuild          Rebuild Codex statistics from existing JSONL
codex-usage serve                   Run the local service in the foreground
codex-usage doctor                  Check Codex paths, sources and service state
codex-usage config add-home PATH    Add another CODEX_HOME
codex-usage uninstall               Stop the service, remove the login start (and the program in standard mode); keep ledgers
codex-usage uninstall --purge       Standard mode: also delete Codex statistics
```

The background service scans once at start. After that, Codex files are checked for changes every 30 seconds and scanned only when they change, with a fallback scan every 10 minutes; Claude Code and Antigravity are scanned every 10 minutes. The **Scan** button scans all three immediately.

**Removal**: in standard mode `uninstall` deletes the program copied into your user folder, and `--purge` also deletes Codex statistics; Claude Code and Antigravity ledgers are always kept, delete them yourself if needed. In portable mode `uninstall` only stops the service and removes the login start; it never deletes the program or the `data` folder, and `--purge` is refused.

## Where data lives

### Portable mode (a `data` folder beside the executable)

```text
Token Usage Tracker/
├─ codex-usage.exe
└─ data/
   ├─ codex/         Codex statistics (usage.sqlite), settings (config.json), backups
   ├─ claude/        Claude Code ledger (usage.sqlite)
   └─ antigravity/   Antigravity ledger (usage.sqlite)
```

When the folder is in OneDrive:

- Right-click the `data` folder and choose "Always keep on this device" so the files never become cloud-only.
- Run one copy of `data` on one computer at a time.
- The ledgers are SQLite databases with `-wal` and `-shm` side files while running. The cloud copy can lag by a few minutes; before using it on another computer, stop the service on the first one and let sync finish.

### Standard mode

Windows defaults:

| Content | Location |
|---|---|
| Program | `%LOCALAPPDATA%\Programs\codex-usage\codex-usage.exe` |
| Codex statistics and settings | `%LOCALAPPDATA%\codex-usage\` (`usage.sqlite`, `config.json`) |
| Claude Code ledger | `%LOCALAPPDATA%\ClaudeUsageTracker\usage.sqlite` |
| Antigravity ledger | `%LOCALAPPDATA%\AntigravityTokenTracker\usage.sqlite` |

On Linux and macOS, Codex statistics live in `~/.local/share/codex-usage` and `~/Library/Application Support/codex-usage`; the other two ledgers use folders of the same names under the system cache directory.

All three ledgers are append-only: once usage is recorded it stays, even after the source JSONL is cleaned up or moved. When a source correction changes a record, the previous value is kept in the ledger's revision table.

### Settings and environment variables

`config.json`:

```json
{
  "listen_address": "127.0.0.1",
  "port": 43189,
  "scan_interval_seconds": 600
}
```

| Variable | Purpose |
|---|---|
| `CODEX_HOME` | Codex folder (default `~/.codex`) |
| `CODEX_USAGE_HOME` | Codex statistics folder; must be a dedicated empty folder |
| `CODEX_USAGE_TIMEZONE` | Accounting time zone used when the statistics database is first created, e.g. `Asia/Taipei` |
| `CODEX_USAGE_LANG` | CLI language: `en` or `zh-TW` |
| `CLAUDE_CONFIG_DIR` | Claude Code config folder(s), comma-separated |
| `CLAUDE_TRACKER_DATA` | Claude Code ledger folder |
| `ANTIGRAVITY_TRACKER_HOME` | Antigravity folder (default `~/.gemini`) |
| `ANTIGRAVITY_TRACKER_DATA` | Antigravity ledger folder |

## Privacy and security

- The service binds to `127.0.0.1` only and refuses any other address; mutating API calls must come from a local origin.
- The page loads no external resources and ships a strict Content-Security-Policy.
- No uploads, no telemetry, no automatic downloads or updates.
- Prices are built into the program; nothing is fetched at runtime.

## What the cost means

Amounts are equivalents at each vendor's public **Standard API prices**. They are **not** your subscription charges or bill:

- Codex uses the built-in OpenAI price table; usage explicitly marked Fast is weighted by the Codex quota multiplier.
- Claude Code uses Anthropic's official dated rates with separate 5-minute and 1-hour cache-write prices; fast mode and the US inference region use the official multipliers.
- Antigravity uses dated rates per model; long-context Gemini 3.1 Pro requests use the higher tier.

Models or dates without a verified rate are shown as unpriced. Their tokens still count, and they are never treated as free. For Codex models you can set an alias or custom rate under **Pricing**.

## Known limits

- Only this computer's records are counted; check other machines on those machines.
- Records deleted before installation cannot be recovered.
- Only Codex records carry project and conversation titles; Claude Code and Antigravity sessions show as "source · first 8 characters of the ID".
- The **Pricing** dialog edits Codex rates only.
- The `summary` command prints Codex only; use the dashboard or an export for the combined total.

## Development

```bash
go test ./...
go vet ./...
```

The page sources are in `internal/web/static/` and are embedded into the binary at build time.

## Credits and license

This project is a modified version of zJay's [codex-usage](https://github.com/zJay26/codex-usage).

Released under the GNU General Public License v3.0, see [LICENSE](LICENSE). The upstream project's MIT notice and dependency licenses are listed in [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md).
