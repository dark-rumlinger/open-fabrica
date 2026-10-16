# Open Fabrica
A tribute to the best software in the world.

## Quick Start

**Important: This is an early release which focuses on getting the core build loop as stable as possible: things will break and tokens will be wasted. Ideally you're on a cheap coding plan or run a local model when your hardware allows for that. Many architectural decisions are not finalized.**

1. **Download latest binary** from [Releases](https://github.com/markdr-hue/open-fabrica/releases).
2. **Run it.** Double-click the file, or run `./openfabrica` in your terminal.
3. **Open** `http://localhost:5734` in your browser.
4. **Pick an AI provider.** The setup wizard asks for an API key (Anthropic, OpenAI, Google, or any OpenAI-compatible provider). A local Ollama is auto-detected, no key needed.
5. **Describe your idea.** Open Fabrica takes it from there.

## Features

- **Fully self-contained** binary which runs everywhere.
- **Instantly live** by eliminating the concept of hosting.
- **Automated testing** of user flows in your projects.
- **Unlimited projects** from a single instance.
- **Accounts and logins** with optional Google/GitHub social login (any OAuth2 provider works), TOTP 2FA, and passwordless magic links.
- **Payments** via Stripe, PayPal, or Mollie; one-off, or recurring on Stripe and PayPal. Stripe reconciles itself from signed webhooks; PayPal and Mollie payments are reconciled manually.
- **Email** (SendGrid, Mailgun, Resend, or any JSON endpoint) and **SMS** (Twilio, MessageBird, or any JSON endpoint).
- **Notifications** in-app, plus **Web Push** with auto-managed VAPID keys.
- **Real-time** chat rooms, live feeds, P2P calling, and file uploads, with optional end-to-end encryption.
- **Image generation** via OpenAI or a local Stable Diffusion (Automatic1111) instance.
- **Stock photos** from Pexels, Pixabay, and Unsplash, picked automatically.
- **PDF generation** for invoices, receipts, tickets, and reports (needs a local Chromium based browser).
- **CSV import/export**, **RSS/Atom feeds** served straight from a table.
- **Keyword and semantic search** out of the box.
- **Analytics** (visits, unique visitors, top pages, referrers, AI usage/cost), all stored locally.
- **SEO** (Open Graph, Twitter cards, JSON-LD, sitemaps, robots.txt) handled automatically.
- **Native PWA support** for offline and installable apps.
- **Auto-generated APIs** with rate limiting and docs.
- **Per-project AI assistants** with configurable personality and memory.
- **AI agents** that plan and act over multiple steps: a hosted endpoint that reasons, calls your app's own functions and reads its data in a loop, and can spawn and manage sub-agents to break down bigger jobs. Stream progress live or run in the background, each bounded by a per-endpoint daily token budget.
- **Server-side AI hooks** to auto-summarize, classify, or embed new rows on insert.
- **Telegram bot** so you can plan/build projects and get push notifications from your phone.
- **Scheduled jobs** (cron or intervals) that run SQL, send emails, hit HTTP endpoints, or invoke the AI.
- **Event-driven actions** reacting to signups, logins, payments, and data changes.
- **Roles and permissions** with wildcard-based access control.
- **Project templates**: save any project as a template and spin up new ones in seconds.
- **Free HTTPS** automatically, no manual actions needed.

## Where's the source?
We will decide on that after our initial preview release reception.
