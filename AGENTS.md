# AGENTS.md

## Scope

Instructions for this repository.

## Defaults

- Keep changes minimal and verifiable.
- Discuss product and architecture tradeoffs before broad implementation.
- Do not add dependencies without approval.
- Run relevant tests, build, or lint before deploy or commit.
- Keep private planning notes under `.local/`.
- Keep `.local/` ignored.

## Safety

- Never commit secrets, tokens, production credentials, or private deployment details.
- Keep environment-specific config in env files or ignored local notes.
- Ask before destructive actions.
- Prefer read-only checks before infrastructure changes.
