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

## Git workflow

- Treat `main` as the production branch and `staging` as the integration branch.
- Create changes on short-lived feature branches from `staging`.
- Open feature pull requests against `staging`.
- Open release pull requests from `staging` to `main` after local verification.
- Merge release pull requests with **Create a merge commit**.
- Do not push directly to `main` or force-push protected branches.
- After a release merge, fast-forward `staging` to `main` and push `staging`.
- Keep the commit author email set to `mail@saidwp.com` for GitHub attribution.

## Pull request checks

- Require the repository CI workflow to pass before merging.
- Run `go test ./...`, `go vet ./...`, and `go build ./...` locally before
  opening a release pull request.
- Deploy production manually from `main` only after the release pull request is
  merged.
