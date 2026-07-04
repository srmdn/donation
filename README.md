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
- Planned Pakasir QRIS integration for production payments

## Stack

- Go
- SQLite
- Server-rendered HTML templates
- Plain CSS
- Pakasir for QRIS payment integration

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
data/donation.db
```

Override:

```sh
ADDR=127.0.0.1:3000 DB_PATH=data/dev.db go run ./cmd/donation
```

For Pakasir return links:

```txt
PUBLIC_BASE_URL=http://127.0.0.1:8094
```

Admin defaults:

```txt
ADMIN_PASSWORD=admin
ADMIN_SESSION_SECRET=change-me
```

Pakasir placeholders:

```txt
PAKASIR_BASE_URL=https://app.pakasir.com
PAKASIR_API_KEY=
PAKASIR_MERCHANT_SLUG=
```

## Roadmap

- Admin login
- Project management
- Public update publishing
- Donation management
- Pakasir QRIS payment integration

## Payments

The current app uses a mock payment flow for local development.

Production payment flow is planned around Pakasir:

- create QRIS transaction from donation form
- store provider payment data locally
- render `/pay/{donationId}` with hosted QRIS link and QR string fallback
- receive payment webhook at `/api/webhooks/pakasir`
- verify transaction status with Pakasir transaction detail API
- mark donation as paid only after verification

## Donation Amounts

- Suggested quick amounts start at `Rp25.000`
- Donors can enter a custom amount
- Minimum amount is enforced at `Rp25.000`
