# Donation

Project-based donation app for solo builders.

Visitors can choose which project to support, see funding progress, and follow a public activity timeline. The current version is a server-rendered Go app with SQLite and a mock payment flow.

## Current Scope

- Single builder profile
- Multiple projects
- Public builder page with project cards and timeline
- Project detail page with donation form
- SQLite-backed seed data
- Mock payment flow for local development

## Stack

- Go
- SQLite
- Server-rendered HTML templates
- Plain CSS

## Development

Run locally:

```sh
go run ./cmd/donation
```

Default address:

```txt
127.0.0.1:8094
```

Default SQLite path:

```txt
.local/donation.db
```

Override:

```sh
ADDR=127.0.0.1:3000 DB_PATH=.local/dev.db go run ./cmd/donation
```

## Roadmap

- Admin login
- Project management
- Public update publishing
- Donation management
- QRIS payment integration
