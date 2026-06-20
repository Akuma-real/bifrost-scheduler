# AGENTS.md

This file gives working instructions for AI agents and contributors in this repository.

## Project Summary

`bifrost-scheduler` is a Go-based unattended scheduler for GGAPI Bifrost. It talks directly to the Bifrost management API, reads Virtual Key provider state and recent logs, builds a guarded scheduling plan, and optionally applies limited provider weight/key changes.

The project is also used as a Go learning project for the owner. Keep code comments beginner-friendly and in Chinese.

## Safety Rules

- Do not touch the live production environment unless the user explicitly asks for it in the current turn.
- Do not run commands that write to Bifrost unless the user explicitly approves.
- Default to dry-run behavior. Production writes require both:
  - `config.json` has `"mode": "guarded_write"`.
  - The command includes `--apply`.
- Never add secrets to git:
  - `.env`
  - `config.json`
  - real Bifrost admin passwords
  - Telegram bot tokens
  - chat IDs if the user treats them as private
- Do not commit or stage private runtime files. `config.example.json` is the public template; `config.json` is private.
- Do not delete providers, Virtual Keys, API keys, model mappings, Sub2API groups, prices, or balances. This scheduler must only perform the limited actions already represented by the code.

## Architecture

The repository uses a lightweight DDD-style split:

- `cmd/bifrost-scheduler`: CLI, daemon loop, environment variables, logging, and process exit codes.
- `internal/domain/scheduler`: domain models, config defaults, and scheduling decisions. This package must not import HTTP, filesystems, CLI, Bifrost, Telegram, or report rendering.
- `internal/app/scheduler`: application orchestration. It loads state/metrics through the `Store` interface, calls the domain decider, and applies guarded changes.
- `internal/bifrost`: Bifrost management API adapter. Put login, VK/log loading, provider weight PUTs, and key enable/disable API compatibility here.
- `internal/report`: JSON and Chinese Markdown rendering.
- `internal/notify`: optional external notifications such as Telegram.

When adding features, keep the boundary:

- New health or weight rules belong in `internal/domain/scheduler`.
- New Bifrost API request/response details belong in `internal/bifrost`.
- New orchestration behavior belongs in `internal/app/scheduler` or `cmd/bifrost-scheduler`.
- New output formats belong in `internal/report`.
- New notification channels belong in `internal/notify`.

## Go Style For This Repo

- Use Go standard library first. Avoid new dependencies unless they remove real complexity.
- Keep comments in Chinese for code explanations.
- This is a learning-oriented repo. Explain beginner concepts when adding or changing code:
  - `package`
  - `import`
  - `func`
  - parameters and return values
  - `struct`
  - `interface`
  - pointers
  - `error`
  - `if err != nil`
  - `defer`
  - `for range`
  - `map` and `slice`
- Do not add noisy comments that merely repeat a variable name, but do explain non-obvious control flow and project-specific safety behavior.
- Preserve existing Chinese Markdown report wording unless changing behavior intentionally.
- Run `gofmt` on changed Go files.

## Configuration Rules

- Bifrost API paths are intentionally built into the code and normalized defaults. Do not require users to write these paths in JSON unless Bifrost API compatibility truly changes.
- `config.example.json` must stay sanitized and generic.
- `config.json` is local/private and should not be committed.
- Authentication belongs in environment variables or CLI flags:
  - `BIFROST_API_USERNAME`
  - `BIFROST_API_PASSWORD`
  - `BIFROST_API_URL`
- Telegram notification secrets also belong in environment variables or CLI flags:
  - `BIFROST_SCHEDULER_TG_BOT_TOKEN`
  - `BIFROST_SCHEDULER_TG_CHAT_ID`
  - `BIFROST_SCHEDULER_TG_THREAD_ID`

## Scheduling Behavior To Preserve

- The scheduler must not hard-code different strategies just because a pool is named `low` or `stable`.
- `cost_weight` is the healthy target weight, not a quality score.
- Runtime quality is derived from Bifrost logs.
- Low sample size must not cause strong disable decisions.
- Single bad windows should not directly zero a provider.
- Consecutive bad windows are required before zeroing or disabling.
- `min_active_providers` must protect against emptying a pool.
- Image pools should not be managed unless explicitly configured by the user.
- Telegram notifications are optional and must not block scheduling if sending fails.
- In daemon mode, Telegram notifications should avoid repeating the same unchanged decision set every interval.

## Testing

Before handing off code changes, run:

```bash
go test ./...
```

If Go cache permissions fail in this environment, use a repo-local cache:

```bash
GOCACHE="$PWD/.cache/go-build" go test ./...
```

For formatting:

```bash
gofmt -w <changed-go-files>
```

Do not use live Bifrost for unit tests. Use fake stores or fake HTTP transports.

## Documentation

Update docs when behavior or configuration changes:

- `README.md` for user-facing quick usage.
- `docs/configuration.md` for every configurable field and environment variable.
- `docs/learning-go.md` when adding patterns that a Go beginner should understand.

Keep docs in Chinese where they are user-facing.

## Deployment Notes

- Docker Compose examples should remain generic and safe.
- Production compose should mount:
  - `config.json` read-only
  - `logs/` for rotating scheduler logs
- Container stdout should stay concise; full reports should go to rotating log files when configured.
- Do not restart or modify production containers unless the user explicitly asks.

