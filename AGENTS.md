# Working in this repository

This file is for AI coding agents, and for humans who want the same rules. Read it together with [`README.md`](README.md), [`REQUIREMENTS.md`](REQUIREMENTS.md) and [`ARCHITECTURE.md`](ARCHITECTURE.md) before writing code.

## Where we are

Nuno is at M0, design freeze. There is no implementation yet. Do not start a large implementation until the open ADRs in [`docs/decisions.md`](docs/decisions.md) that your work depends on are resolved. When in doubt, ask before coding.

## The rules

1. The spec is the source of truth. `REQUIREMENTS.md` defines what, `ARCHITECTURE.md` defines how. If you think the spec is wrong, change the spec first, with an ADR if it is architectural, then the code.
2. Never invent scope. Features marked Later or Won't are not part of the current milestone. Do not add them while you are in the area.
3. Small, runnable milestones. Follow [`ROADMAP.md`](ROADMAP.md). Each milestone ends with something demonstrable.
4. Core has no I/O. Domain logic must not import provider, store or web code. This keeps the system testable and providers pluggable.
5. Providers declare capabilities, they do not assume them. Never call an optional API without checking the capability flag.
6. Fail loud. No silent skips. A failed provider call fails the reconcile and is reported. A backup that fails silently is worse than no backup.
7. Secrets never touch git, the database or logs. Redact provider credentials everywhere.
8. Idempotency is a tested property, not a hope. Plan, apply, plan again must produce an empty second plan.
9. Document decisions. Non-trivial choices go in `docs/decisions.md` with context and alternatives, not just the outcome. Open decisions stay open until explicitly accepted. Do not silently pick one.

## Style

The goal is code and prose that read like a careful human wrote them. Reviewers should focus on the change, not on how it was produced.

- Do not use em-dashes. Use a comma, a colon, parentheses, or a plain hyphen. This applies to code comments, docs, commit messages and PR descriptions.
- Do not hard-wrap Markdown. Write one paragraph per line and let the renderer wrap it. Tables and code blocks are the only exception.
- Keep comments short and rare. Explain why, never what. If a comment restates the code, delete it.
- No comment banners, no section dividers, no decorative ASCII beyond what already exists.
- Avoid the usual tells: "It's not just X, it's Y", "Let me", "I'll now", "Note that", "In conclusion", heavy hedging, and emoji.
- Vary sentence length. Write plainly.
- Commit messages are short. A one-line subject is usually enough. Add a body only when the why is genuinely non-obvious. No "Generated with" trailers, no co-author lines for tools.
- Do not commit AI tooling files: `CLAUDE.md`, `.claude/`, `.cursor/`, `.cursorrules`, `.aider*`, `.github/copilot-instructions.md`, `*.mdc`, and any scratch plan or notes file. They are in `.gitignore`. Keep them local. `AGENTS.md` is the single exception, because it is project documentation and a common repo convention.
- Do not mention in commits, issues or PRs that the work was AI-assisted.

## Conventions

- Language: code, comments, commit messages, docs and issues in English.
- Commits: imperative subject, explain why in the body only when non-obvious.
- Tests: every provider adapter needs contract tests against recorded fixtures. Domain logic needs unit tests. See ARCHITECTURE section 9.
- Dependencies: prefer the standard library and well-maintained packages. Justify anything heavy in the PR.
- Config: file or env based, documented in `.env.example`. No interactive setup.

## Definition of done

- Implements a requirement ID from `REQUIREMENTS.md`, or the spec was updated first.
- Tests cover the new behavior, unit and/or contract.
- The dry-run path is considered and correct for anything that writes.
- No secrets in code, logs or fixtures.
- Docs updated, including the provider matrix in the README and any ADR.
- `go vet` and `go test ./...` pass locally.

## Repository layout

```
nuno/
├── README.md
├── REQUIREMENTS.md
├── ARCHITECTURE.md
├── ROADMAP.md
├── AGENTS.md
├── docs/decisions.md
├── cmd/nuno/            # main, wiring
├── internal/
│   ├── core/            # domain: model, policy, planner, applier
│   ├── providers/       # nextcloud/, immich/, ... one package each
│   ├── identity/        # ldap/, manual/
│   ├── store/           # sqlite repositories and migrations
│   ├── notify/          # apprise or direct channels
│   ├── api/             # HTTP API
│   └── ui/              # html/template plus static assets
├── tests/
└── docker-compose.yml
```
