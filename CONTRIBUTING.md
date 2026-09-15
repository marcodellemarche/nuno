# Contributing to Nuno

M0 closed on 2026-09-15 and implementation has not started, so the most valuable contribution right now is M1 in [`ROADMAP.md`](ROADMAP.md). Design review is still welcome: no decision is immune, and the 2026-09-15 review superseded six of the first ten. Challenge one by proposing an ADR that supersedes it, not by editing the entry.

## Before you code

1. Read [`README.md`](README.md), [`REQUIREMENTS.md`](REQUIREMENTS.md) and [`ARCHITECTURE.md`](ARCHITECTURE.md).
2. Check [`ROADMAP.md`](ROADMAP.md). Work should belong to the current milestone.
3. If your change is architectural, open an ADR in [`docs/decisions.md`](docs/decisions.md) first and discuss it in an issue.
4. Read `docs/decisions.md` from the bottom. ADR-0011 to ADR-0027 override earlier entries where they conflict.

## Development

The stack is Go, see [ADR-0001](docs/decisions.md#adr-0001-tech-stack). The exact commands will be documented here once the scaffold lands:

```shell
go build ./cmd/nuno
go test ./...          # integration tests are behind //go:build integration
go vet ./...
```

## Style

The same rules apply to humans and to AI agents, see [`AGENTS.md`](AGENTS.md). In short: no em-dashes, no hard-wrapped Markdown, short comments, short commit messages.

## Pull requests

- One concern per PR.
- Reference the requirement ID you implement.
- Include tests and update the docs.
- Keep the diff focused. Do not reformat unrelated code.

## Reporting bugs

Include the Nuno version, the provider and its version, what you expected, what happened, and the relevant logs with credentials redacted. Never paste credentials.
