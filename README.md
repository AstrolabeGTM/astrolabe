# Astrolabe

**Go-to-market for the everyday programmer.** You built something. Astrolabe finds people who have the problem it solves, drafts outreach you approve, follows up, and shows which efforts turn into users and revenue, without learning five sales tools.

- **Talk to it from Claude** (MCP): "who should I contact this week?", "draft intros for the top 5", "why did activation drop?"
- **Or use the web app**: an inbox, a live signal feed, automations, people, a funnel.
- **Configure it like code**: each product is a folder of YAML and Markdown in git. Your editor autocompletes it, and `astrolabe doctor` checks it.
- **Self-host it**: one Docker image plus Postgres, like Mattermost or Penpot. Open source under AGPL-3.0.

Developer terms come first, and sales terms work too: "enroll my top MQLs in the intro cadence" does the same as "start the intro sequence for the top prospects".

## Try it in five minutes (no accounts needed)

```sh
mkdir my-gtm && cd my-gtm
docker run --rm --user "$(id -u):$(id -g)" -v "$PWD:/work" -w /work ghcr.io/astrolabegtm/astrolabe init -template demo
docker compose up -d
```

Open http://localhost:8080 and sign in with `ASTROLABE_PASSWORD` from `.env`. The demo runs in **sandbox mode**: nothing leaves your machine, and every message lands in a local **Outbox**. Try this:

1. **People:** tick a few demo people, then **Start sequence** with `intro`.
2. **Inbox:** read the draft, then **Save & approve**.
3. **Outbox:** the message is there. Play the recipient: **Simulate reply** (or bounce).
4. **Inbox:** the sequence stopped and the reply waits to be labelled. Mark it *interested* and a to-do appears.
5. **Signals → Add a signal by hand** with type `referral`: the `referral-intro` automation starts a sequence by itself.

## Use it from Claude or Codex

Astrolabe is an MCP server. Every client gets the same tools ([full list](docs/mcp-tools.md)), over one of two transports:

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

Then ask "what's in my inbox?", "who are the top prospects?", "start the intro sequence for Sam and Priya", "simulate a reply from Sam" or "how's the funnel?".

HTTP binds to the same port as the web app. For anything beyond your own machine, put it behind HTTPS; the token is the only credential.

## Your own product

```sh
docker run --rm --user "$(id -u):$(id -g)" -v "$PWD:/work" -w /work ghcr.io/astrolabegtm/astrolabe init -template devtool -name "Hook Check" -url https://hookcheck.dev
```

| Template | For |
| --- | --- |
| `devtool` | Developer tools: GitHub/HN signals become tasks to help in the thread; email for people who gave you an address; help after signup |
| `saas` | B2B SaaS: trials, activation, trial-ending notes, win-back |
| `app` | Consumer apps: product events, push/in-app and email to users |
| `demo` | The fictional product above |

This writes `products/<id>/`:

| File | What goes in it |
| --- | --- |
| `product.yaml` | Name, who it's for (`target`), funnel stages, the offer, sender mailboxes, `fit` rules, `auto_approve`, limits, and for b2b an optional account `pipeline` |
| `claims.md`, `voice.md` | What's true about the product (drafts may only use this) and how you write. It won't load while they start with `STUB:`. |
| `sequences/*.yaml` | Timed message steps, `kind: cold` (strangers) or `kind: users` (existing users) |
| `monitors.yaml` | Where to look for signals: GitHub searches, your repo's stars, issues and discussions, HN/Stack Overflow/RSS keywords, and `github_release` counters (release downloads, stars, forks, watchers, discussions; no people). Add `reception: true` to log results as public mentions |
| `automations.yaml` | `trigger → when → action` rules (sales: plays) |

Every YAML file starts with a `# yaml-language-server: $schema=...` line, so VS Code (with the YAML extension), Zed, Neovim and JetBrains autocomplete keys, explain them on hover and flag typos. Or ask Claude to "set up a new product" and it interviews you and writes the folder.

Then check what's missing:

```sh
docker compose exec astrolabe astrolabe doctor           # add -connect to test logins, keys and DNS
```

## Connect email (app password)

Cold email should come from a separate domain or mailbox (for example `you@try-yourproduct.com`), so a bad day doesn't hurt your main domain. Connect each sender address in **Settings → Mailboxes**, or:

```sh
docker compose exec -e ASTROLABE_MAIL_PASSWORD=... astrolabe astrolabe mailbox add you@try-yourproduct.com -provider gmail
```

Presets exist for Gmail/Google Workspace, Fastmail, iCloud and Zoho (hosts, ports and username rules checked against each provider's docs in September 2026); `-provider custom -smtp host:587 -imap host:993` works for anything else. Both logins are tested before saving, and the password is stored in the secrets volume, never in the database. Astrolabe sends over SMTP, reads replies and bounces over IMAP, and files a copy in Sent where the provider doesn't. Gmail OAuth (`astrolabe gmail auth`) is still available if you prefer it.

## Go live

1. Use a **new database** for live sending: a database stays in the mode it started in, so demo data never mixes with real sends. In `.env`, set `ASTROLABE_MODE=live`, then `docker compose down -v && docker compose up -d`. Delete the demo product folder first.
2. Fill in `claims.md` and `voice.md`, connect the mailboxes, and run `astrolabe doctor -connect` until nothing is ✗.
3. Put the server behind HTTPS and set `ASTROLABE_PUBLIC_URL` for tracked links and webhooks. [deploy/terraform](deploy/terraform) sets up one small GCP VM with Caddy and backups; any Docker host works.
4. Optional: set `ASTROLABE_GITHUB_TOKEN` (strongly recommended for monitors) and `ASTROLABE_ANTHROPIC_API_KEY` (AI research, drafts and reply sorting).

## How it works, in developer terms

```text
signals ──▶ people + score ──▶ automations ──▶ sequences ──▶ drafts ──▶ you approve ──▶ send
(GitHub issues,  (fit + intent       (trigger → when      (timed      (Claude writes      replies, clicks,
 stars, HN,       + why now,           → action rules,     message     them from your     signups, payments
 product events)  priority A–D)        like CI workflows)  steps)      claims + voice)   ──▶ funnel, digest
```

| You'll see (sales term) | It means |
| --- | --- |
| **Signal** (intent data) | An event about a person: an issue they opened about your problem, a star, an HN post, a signup |
| **Target / fit rules** (ICP) | Who the product is for: sentences for humans, `dep:bullmq`-style facts with weights for the scorer |
| **Priority A–D** (lead score) | Score 0–100 from fit, recent activity (intent) and time-sensitive events (why now). A = contact now |
| **Sequence** (cadence) | Timed message steps. `kind: cold` for strangers, `kind: users` for existing users |
| **Automation** (play, workflow) | `trigger → when → action` in `automations.yaml`, e.g. on a pain-issue signal with fit ≥ 50, start a sequence |
| **Funnel / activation** (lifecycle stages) | Your product's stages for people; activation is the first real success (the number to move) |
| **Pipeline** (deals, opportunities) | b2b, optional: companies (accounts) moving through your stages, each with one next action and a note per move. Qualification checks (BANT, MEDDIC) must be "yes" with evidence before the gate stage. See [§15.2](docs/astrolabe.md#152-account-pipeline-and-qualification) |
| **Offer** | The one concrete thing every message asks for |
| **Do not contact** (suppression list) | People no product will ever message again |

The config accepts sales vocabulary too (`icp`, `autonomy`, `plays.yaml`, `kind: outbound`). The design, with the full glossary, is in [docs/astrolabe.md](docs/astrolabe.md).

## Send product events and payments

Your app sends events to `POST /hooks/events/<product>`, and Stripe sends to `POST /hooks/stripe/<product>`. Both need `ASTROLABE_PUBLIC_URL` and a signing secret (see **Settings → Webhooks**). Signing an event from a shell:

```sh
body='{"id":"evt-1","type":"signed_up","user_id":"u42","email":"ana@acme.example"}'
t=$(date +%s); sig=$(printf '%s.%s' "$t" "$body" | openssl dgst -sha256 -hmac "$ASTROLABE_EVENTS_SECRET_HOOK_CHECK" -hex | awk '{print $NF}')
curl -X POST https://gtm.example.com/hooks/events/hook-check -H "Astrolabe-Signature: t=$t,v1=$sig" -d "$body"
```

An event whose `type` matches a funnel stage records that stage. Sending `user_id` with `email` links the signup to earlier outreach.

## How sending stays safe

- Approval covers the exact sender, recipient, subject and body. Any edit withdraws it.
- Each message has one row. The sender claims it in a transaction that rechecks approval, the sequence, the do-not-contact list, pauses and limits, then sends once.
- If the provider's answer is lost, the message is marked **unknown** and never resent automatically. The inbox offers **Check Sent mail**, which looks it up by its Message-ID.
- **No email from GitHub data.** GitHub's Acceptable Use Policies forbid using GitHub information to send unsolicited email, so a person Astrolabe knows only from GitHub (a monitor, an issue, a profile email) is never emailed: enrolment is refused with the reason and the send step checks again. Help them in the issue thread instead. They become emailable once they give an address (signup, reply, your import). A product can opt out with `outreach.email_from_github: true`; `astrolabe doctor` warns when it does.
- Tracked links are optional: `links.personal: false` keeps the plain offer link in every message.
- Replies stop the sequence. Bounces, "no" and unsubscribes put the person on the do-not-contact list for every product.

## Configuration

| Variable | |
| --- | --- |
| `ASTROLABE_MODE` | `sandbox` (local outbox) or `live` |
| `ASTROLABE_PASSWORD` | Web app password; generated and printed on first start if unset |
| `ASTROLABE_DATABASE_URL` | Postgres 15+ |
| `ASTROLABE_PUBLIC_URL` | Public https address (tracked links, webhooks) |
| `ASTROLABE_GITHUB_TOKEN` | GitHub monitors and enrichment (no scopes needed) |
| `ASTROLABE_ANTHROPIC_API_KEY` | AI research, drafts, reply sorting, content |
| `ASTROLABE_EVENTS_SECRET_<PRODUCT>`, `ASTROLABE_STRIPE_SECRET_<PRODUCT>` | Webhook signing secrets |
| `ASTROLABE_RESEND_API_KEY` | Optional: Resend for email to existing users |
| `ASTROLABE_WHATSAPP_*`, `ASTROLABE_NOTIFY_SECRET_<PRODUCT>` | WhatsApp and push/in-app channels |
| `ASTROLABE_DIGEST_TO`, `ASTROLABE_DIGEST_FROM`, `TZ` | Weekly digest email (Mondays 08:00) |

Run `astrolabe help` for every command.

## Status

Everything in the design is built. The tests run against Postgres with fake providers, and the Docker quickstart above has been run end to end. Nothing has yet been run against real SMTP/IMAP, Gmail, WhatsApp, Stripe or Claude accounts, so the first live run is where rough edges will show. Please open issues.

## Development

```sh
docker compose up --build            # the demo, from source, in sandbox mode
go test ./...                        # needs Postgres; set ASTROLABE_TEST_DATABASE_URL if it isn't at /tmp
go run ./cmd/astrolabe schema schemas   # after changing config structs
```

## Licence

[AGPL-3.0](LICENSE). You can self-host, modify and share it. If you run a modified version as a service for others, you must offer them your changes.
