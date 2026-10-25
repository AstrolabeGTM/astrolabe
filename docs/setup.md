# Setting up Astrolabe

The technical reference: running it, connecting Claude and mailboxes, going live, webhooks, configuration and development. The [README](../README.md) explains how Astrolabe works and has the five-minute demo; [astrolabe.md](astrolabe.md) is the design.

## Run it

**With Docker (recommended).** `astrolabe init` writes a workspace with a `compose.yaml` and `.env`:

```sh
docker run --rm --user "$(id -u):$(id -g)" -v "$PWD:/work" -w /work ghcr.io/astrolabegtm/astrolabe init -template demo
docker compose up -d
```

The web app is at http://localhost:8080/app/; sign in with `ASTROLABE_PASSWORD` from `.env` (generated and printed on first start if unset). Commands run inside the container: `docker compose exec astrolabe astrolabe <command>`.

**As a binary.** Install it from source and point it at Postgres and your products folder:

```sh
go install ./cmd/astrolabe
export ASTROLABE_DATABASE_URL='postgres:///astrolabe?host=/tmp'
export ASTROLABE_PRODUCTS=/path/to/products
astrolabe serve          # migrates on start
```

It listens on 127.0.0.1:8080 and needs no sign-in there, as long as `ASTROLABE_PUBLIC_URL` is unset. Mailbox passwords and the MCP token live in `~/.config/astrolabe` (`ASTROLABE_SECRETS_DIR`); the server and the CLI must use the same folder. With Docker it is the `secrets` volume, which is separate from your home folder.

Run `astrolabe help` for every command.

## Use it from Claude or Codex

Astrolabe is an MCP server. Every client gets the same tools ([full list](mcp-tools.md)), over one of two transports:

- **stdio**: the client starts `astrolabe mcp` itself (through `docker compose exec`), one process per client. Nothing extra to set up.
- **HTTP**: the client connects to the running server at `http://localhost:8080/mcp` with a bearer token. The server must be up (`docker compose up -d`).

Get the token (generated on first start and kept in the secrets volume; or set `ASTROLABE_MCP_TOKEN` in `.env`):

```sh
export ASTROLABE_MCP_TOKEN=$(docker compose exec -T astrolabe astrolabe mcp token)
docker compose exec astrolabe astrolabe mcp token -rotate   # replace it; takes effect at once
```

**Claude Code**

```sh
# stdio
claude mcp add astrolabe -- docker compose -f "$PWD/compose.yaml" exec -T astrolabe astrolabe mcp
# stdio, with the binary instead of Docker
claude mcp add astrolabe -e ASTROLABE_DATABASE_URL=... -e ASTROLABE_PRODUCTS=... -- astrolabe mcp
# HTTP
claude mcp add --transport http astrolabe http://localhost:8080/mcp --header "Authorization: Bearer $ASTROLABE_MCP_TOKEN"
```

(The HTTP form stores the token in Claude's config; `--scope project` writes it to `.mcp.json`, so keep that file out of git.)

**Claude Desktop** takes stdio in `claude_desktop_config.json` (use the absolute path to your `compose.yaml`):

```json
{
  "mcpServers": {
    "astrolabe": {
      "command": "docker",
      "args": ["compose", "-f", "/path/to/compose.yaml", "exec", "-T", "astrolabe", "astrolabe", "mcp"]
    }
  }
}
```

**Codex CLI**

```sh
# stdio
codex mcp add astrolabe -- docker compose -f "$PWD/compose.yaml" exec -T astrolabe astrolabe mcp
# HTTP (Codex reads the token from the environment variable, so it never lands in config.toml)
codex mcp add astrolabe --url http://localhost:8080/mcp --bearer-token-env-var ASTROLABE_MCP_TOKEN
```

These write the following to `~/.codex/config.toml`, which you can also edit by hand:

```toml
# stdio
[mcp_servers.astrolabe]
command = "docker"
args = ["compose", "-f", "/path/to/compose.yaml", "exec", "-T", "astrolabe", "astrolabe", "mcp"]

# HTTP (use one of the two)
[mcp_servers.astrolabe]
url = "http://localhost:8080/mcp"
bearer_token_env_var = "ASTROLABE_MCP_TOKEN"
```

HTTP binds to the same port as the web app. For anything beyond your own machine, put it behind HTTPS; the token is the only credential.

## Connect email (app password)

Connect each sender address on the classic **Settings** page (**Mailboxes**), or:

```sh
astrolabe mailbox add you@try-yourproduct.com -provider gmail
```

It asks for the app password (or reads `ASTROLABE_MAIL_PASSWORD`; avoid that on the command line, where it lands in your shell history). Both logins are tested before saving, and the password is stored in the secrets folder, never in the database. Astrolabe sends over SMTP, reads replies and bounces over IMAP, and files a copy in Sent where the provider doesn't.

- **Presets** exist for Gmail/Google Workspace, Fastmail, iCloud and Zoho (hosts, ports and username rules checked against each provider's docs in September 2026). `-provider custom -smtp host:587 -imap host:993` works for anything else.
- **Zoho outside the US data centre** (the admin console is at `zoho.in`, `zoho.eu` and so on) needs your region's hosts: `-provider zoho -smtp smtppro.zoho.in:465 -imap imappro.zoho.in:993`. Each user must also turn on IMAP in their own Zoho Mail settings; the admin policy only allows it.
- **Gmail OAuth** (`astrolabe gmail auth <address>`) is still available if you prefer it.
- `astrolabe mailbox list|test|remove` manage what's saved.

Cold email can come from a separate domain or mailbox (for example `you@try-yourproduct.com`), so a bad day doesn't hurt your main domain. At a handful of personal, approved messages a day, your own address on the main domain is fine; a new domain has no reputation and needs weeks of warm-up.

## Go live

1. Use a **new database** for live sending: a database stays in the mode it started in, so demo data never mixes with real sends. In `.env`, set `ASTROLABE_MODE=live`, then `docker compose down -v && docker compose up -d`. Delete the demo product folder first.
2. Fill in `claims.md` and `voice.md`, connect the mailboxes, and run `astrolabe doctor -connect` until nothing is ✗. It tests the SMTP/IMAP logins, tokens and SPF/DMARC without sending anything.
3. Send a message to a second address you own, reply to it, and check that the sequence stopped and the copy is in Sent.
4. For tracked links, webhooks and updates-list links, put the server behind HTTPS and set `ASTROLABE_PUBLIC_URL`. [deploy/terraform](../deploy/terraform) sets up one small GCP VM with Caddy and backups; any Docker host works. Without a public address, forms are pulled from Netlify and updates-list replies confirm or unsubscribe.
5. Optional: set `ASTROLABE_GITHUB_TOKEN` (strongly recommended for monitors) and `ASTROLABE_ANTHROPIC_API_KEY` (AI research, drafts and reply sorting; without it, Claude writes drafts over MCP).

## Send product events, payments and forms

Your app sends events to `POST /hooks/events/<product>`, and Stripe sends to `POST /hooks/stripe/<product>`. Both need `ASTROLABE_PUBLIC_URL` and a signing secret (see **Settings → Webhooks**). Signing an event from a shell:

```sh
body='{"id":"evt-1","type":"signed_up","user_id":"u42","email":"ana@acme.example"}'
t=$(date +%s); sig=$(printf '%s.%s' "$t" "$body" | openssl dgst -sha256 -hmac "$ASTROLABE_EVENTS_SECRET_HOOK_CHECK" -hex | awk '{print $NF}')
curl -X POST https://gtm.example.com/hooks/events/hook-check -H "Astrolabe-Signature: t=$t,v1=$sig" -d "$body"
```

An event whose `type` matches a funnel stage records that stage. Sending `user_id` with `email` links the signup to earlier outreach.

A form on your own server posts its fields as JSON to `POST /hooks/forms/<product>` the same way, signed with `ASTROLABE_FORMS_SECRET_<PRODUCT>` (add `?form=<monitor id>` when the product has several `form_webhook` monitors). A form on Netlify needs no webhook, because a `netlify_forms` monitor pulls it with `ASTROLABE_NETLIFY_TOKEN`. Each request becomes a person, a signal, an account and a reply task; see [§15.5](astrolabe.md#155-form-intake).

## How sending stays safe

- Approval covers the exact sender, recipient, subject and body. Any edit withdraws it.
- Each message has one row. The sender claims it in a transaction that rechecks approval, the sequence, the do-not-contact list, pauses and limits, then sends once.
- If the provider's answer is lost, the message is marked **unknown** and never resent automatically. The inbox offers **Check Sent mail**, which looks it up by its Message-ID.
- **No email from GitHub data.** GitHub's Acceptable Use Policies forbid using GitHub information to send unsolicited email, so a person Astrolabe knows only from GitHub (a monitor, an issue, a profile email) is never emailed: enrolment is refused with the reason and the send step checks again. Help them in the issue thread instead. They become emailable once they give an address (signup, reply, your import). A product can opt out with `outreach.email_from_github: true`; `astrolabe doctor` warns when it does.
- Tracked links are optional: `links.personal: false` keeps the plain offer link in every message.
- Replies stop the sequence. Bounces, "no" and unsubscribes put the person on the do-not-contact list for every product.
- **The updates list is double opt-in.** A sign-up (a form automation, or you adding someone who asked in person) is *pending* and gets one confirmation email; they are not on the list until they reply to it (it works with no public address) or open its link (when `ASTROLABE_PUBLIC_URL` is set). Unconfirmed sign-ups expire. Every message carries `List-Unsubscribe` (a `mailto:`, plus one-click https when a public URL is set) and a visible line, and an unsubscribe is honoured before the next send. It leaves only the list: do-not-contact needs a "stop all" or a STOP to outreach.
- **Broadcasts are approved text, sent once.** You read the exact subject and body and approve it; an edit withdraws the approval and flagged copy (a never-claim, a placeholder) can't be approved. It goes to confirmed subscribers only, one row each, claimed in a transaction, at most `updates.per_day` (default 100) in any 24 hours, and never twice. If the provider's answer is lost the row is marked unknown and not resent. No broadcast is approved or sent without your `updates.postal_address`.
- **Erase on request.** `astrolabe forget <email>` (or the `forget_person` tool) deletes everything about one person in one transaction, deletes their Netlify form submissions through Netlify's API, and keeps only a salted hash so they are never contacted or recreated. See [§15.12](astrolabe.md#1512-forgetting-a-person).

## Configuration

| Variable | |
| --- | --- |
| `ASTROLABE_MODE` | `sandbox` (local outbox) or `live` |
| `ASTROLABE_DATABASE_URL` | Postgres 15+ (default `postgres:///astrolabe?host=/tmp`) |
| `ASTROLABE_PRODUCTS` | The products folder (default `./products`) |
| `ASTROLABE_ADDR` | Listen address (default `127.0.0.1:8080`) |
| `ASTROLABE_PASSWORD` | Web app password; generated and printed on first start if unset. Not needed on 127.0.0.1 without a public URL |
| `ASTROLABE_SECRETS_DIR` | Mailbox passwords and the MCP token (default `~/.config/astrolabe`) |
| `ASTROLABE_MCP_TOKEN` | Bearer token for MCP at `/mcp` (default: generated into the secrets folder) |
| `ASTROLABE_PUBLIC_URL` | Public https address (tracked links, webhooks, and links to confirm or leave the updates list; without it, replies do that) |
| `ASTROLABE_GITHUB_TOKEN` | GitHub monitors and enrichment (no scopes needed; 5,000 requests an hour instead of 60) |
| `ASTROLABE_STACKEXCHANGE_KEY` | Optional Stack Exchange key |
| `ASTROLABE_ANTHROPIC_API_KEY` | AI research, drafts, reply sorting, content |
| `ASTROLABE_EVENTS_SECRET_<PRODUCT>`, `ASTROLABE_STRIPE_SECRET_<PRODUCT>`, `ASTROLABE_FORMS_SECRET_<PRODUCT>` | Webhook signing secrets (product events, Stripe, forms) |
| `ASTROLABE_NETLIFY_TOKEN` | `netlify_forms` monitors: a Netlify personal access token that reads form submissions |
| `ASTROLABE_RESEND_API_KEY` | Optional: Resend for email to existing users, and for the updates list when `updates.via: resend` |
| `ASTROLABE_WHATSAPP_*`, `ASTROLABE_NOTIFY_SECRET_<PRODUCT>` | WhatsApp and push/in-app channels |
| `ASTROLABE_DIGEST_TO`, `ASTROLABE_DIGEST_FROM`, `TZ` | Weekly digest email (Mondays 08:00) |
| `ASTROLABE_RENDER_URL` | Optional creative renderer, a headless-Chromium sidecar (Gotenberg 8). Start it with `docker compose --profile creative up -d`; the compose files default to `http://gotenberg:3000`. Without it, campaign creatives keep their copy, checks and approval, and rendering says what to start |

## Development

```sh
docker compose up --build            # the demo, from source, in sandbox mode
go test ./...                        # needs Postgres; set ASTROLABE_TEST_DATABASE_URL if it isn't at /tmp
go run ./cmd/astrolabe schema schemas   # after changing config structs
go run ./cmd/astrolabe mcp tools -md > docs/mcp-tools.md   # after changing MCP tools
```
