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
- Pakasir QRIS integration for production payments

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

Admin auth:

```txt
ADMIN_EMAIL=you@example.com
ADMIN_SESSION_SECRET=change-me
SMTP_HOST=
SMTP_PORT=587
SMTP_USERNAME=
SMTP_PASSWORD=
MAIL_FROM=
```

Generate a session secret:

```sh
python3 -c "import secrets; print(secrets.token_hex(32))"
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

## Admin Login

Admin access now uses an email magic link.

- `ADMIN_EMAIL` controls who can request the link
- the sign-in token is one-time and expires after 15 minutes
- if SMTP is not configured and `PUBLIC_BASE_URL` is local (`localhost` or `127.0.0.1`), the sign-in link is written to the app log
- on non-local deployments, admin login fails closed unless SMTP is configured

## Donation Amounts

- Suggested quick amounts start at `Rp25.000`
- Donors can enter a custom amount
- Minimum amount is enforced at `Rp25.000`
