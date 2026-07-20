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

Default SQLite path:

```txt
data/donation.db
```

Override with your own local bind address and database path:

```sh
ADDR=127.0.0.1:<your-port> DB_PATH=data/dev.db go run ./cmd/donation
```

For Pakasir return links, set your own local base URL:

```txt
PUBLIC_BASE_URL=http://127.0.0.1:<your-port>
```

Environment:

```txt
APP_ENV=development
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

Production startup guards:

- `APP_ENV=production` refuses to start with `ADMIN_SESSION_SECRET` empty or `change-me`
- `APP_ENV=production` refuses to start with `PAYMENT_MODE=mock`
- `APP_ENV=production` refuses to start without SMTP configuration

Pakasir placeholders:

```txt
PAKASIR_BASE_URL=https://app.pakasir.com
PAKASIR_API_KEY=
PAKASIR_MERCHANT_SLUG=
PAYMENT_RECONCILE_INTERVAL=5m
PAYMENT_RECONCILE_LOOKBACK=48h
```

When `PAYMENT_MODE=pakasir`, successful webhooks are verified against Pakasir before local status changes. A background reconciler also checks recent pending payments every five minutes by default. Set `PAYMENT_RECONCILE_INTERVAL=0` to disable it.

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
- if SMTP is not configured and `APP_ENV=development` with local `PUBLIC_BASE_URL`, the sign-in link is written to the app log
- outside that development case, admin login fails closed unless SMTP is configured

## Donation Amounts

- Suggested quick amounts start at `Rp25.000`
- Donors can enter a custom amount
- Minimum amount is enforced at `Rp25.000`
- Admin-recorded manual transfers may use any positive whole IDR amount

## Manual Transfers

Admin donation management can record a new bank transfer or mark an existing pending payment as paid manually. Manual entries default to hidden, stay out of public totals and timelines, preserve any existing Pakasir order metadata, and can be made public after verification.

## License

Licensed under `AGPL-3.0-or-later`. See [LICENSE](LICENSE).
