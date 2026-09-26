# Contributing to Nuno

Read [`README.md`](README.md), [`REQUIREMENTS.md`](REQUIREMENTS.md) and [`ARCHITECTURE.md`](ARCHITECTURE.md) before writing code, and check [`ROADMAP.md`](ROADMAP.md): work should belong to the current milestone.

## Decisions

Read [`docs/decisions.md`](docs/decisions.md) from the bottom. A later ADR overrides an earlier one where they conflict, and a superseded entry carries a pointer at the top. Treating ADR-0007 or ADR-0010 as current produces wrong code. No decision is immune: challenge one by proposing an ADR that supersedes it, not by editing the entry. If your change is architectural, open the ADR first and discuss it in an issue.

If the spec does not cover a case you hit, do not stall and do not guess silently. Implement the most conservative behavior available, leave a `TODO(spec):` comment naming the gap, and list it under "Spec gaps found" in the pull request. That turns an invisible guess into a reviewable line.

## The rules

1. The spec is the source of truth. `REQUIREMENTS.md` defines what, `ARCHITECTURE.md` defines how. If you think the spec is wrong, change the spec first, with an ADR if it is architectural, then the code.
2. Never invent scope. Features marked Later or Won't are not part of the current milestone. Do not add them while you are in the area.
3. Small, runnable milestones. Each milestone ends with something demonstrable.
4. Core has no I/O. `internal/core` holds the model, the pure functions and the port interfaces, and imports no other internal package; CI checks this. Orchestration that sequences I/O lives in `internal/reconcile`.
5. Providers declare capabilities, they do not assume them. Never call an optional API without checking the capability flag.
6. Fail loud. No silent skips. A failed provider call stops the run for that provider and is reported, with honest applied/failed/skipped/guarded counts: never claim nothing happened when writes already landed. The sanctioned exceptions are the guardrails (ADR-0009, ADR-0016), and they are not silent.
7. Secrets never touch git, the database or logs. Redact provider credentials everywhere.
8. Idempotency is a tested property, not a hope. Plan, apply, plan again must produce an empty second plan.
9. Document decisions. Non-trivial choices go in `docs/decisions.md` with context and alternatives, not just the outcome. Open decisions stay open until explicitly accepted.

## Development

The stack is Go, see [ADR-0001](docs/decisions.md#adr-0001-tech-stack).

```shell
make check         # vet, tests with the race detector, gofmt
make integration   # the real stack, needs docker-compose.test.yml up
make image
```

Integration tests are tagged `//go:build integration` and are not part of `make check`.

## Conventions

- Code, comments, commit messages, docs and issues are in English.
- Every provider adapter needs contract tests against recorded fixtures. Domain logic needs unit tests. See ARCHITECTURE section 10.
- Prefer the standard library and well-maintained packages. Justify anything heavy in the PR.
- Configuration is file or env based, documented in `.env.example`. No interactive setup.

## Style

- Do not use em-dashes. Use a comma, a colon, parentheses, or a plain hyphen. This applies to code comments, docs, commit messages and PR descriptions.
- Do not hard-wrap Markdown. Write one paragraph per line and let the renderer wrap it. Tables and code blocks are the only exception.
- Keep comments short and rare. Explain why, never what. If a comment restates the code, delete it.
- No comment banners, no section dividers, no decorative ASCII beyond what already exists.
- Write plainly. No emoji.
- Commit messages have an imperative subject, and a one-line subject is usually enough. Add a body only when the why is genuinely non-obvious.

## Pull requests

- One concern per PR, with a focused diff. Do not reformat unrelated code.
- Reference the requirement ID you implement, or update the spec first.
- Include tests for the new behavior, unit and/or contract.
- For anything that writes, the dry-run path is considered and correct.
- No secrets in code, logs or fixtures.
- Update the docs, including the "known to work with" line in the README and any ADR.

## Reporting bugs

Include the Nuno version, the provider and its version, what you expected, what happened, and the relevant logs with credentials redacted. Never paste credentials.
