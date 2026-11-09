---
slug: seo_and_syndication
role: capability
title: SEO & syndication
summary: Make a public content site discoverable - per-page meta and Open Graph tags, sitemap.xml and robots.txt, URL redirects, and RSS/Atom feeds from a table.
use_when: A public content or marketing site that needs to be found - a blog, articles, a podcast, or anything syndicated over RSS.
not_when: An app, game, tool, or dashboard, and anything behind a login.
evidence_gated: true
category: content
position: 50
tools: [manage_seo, manage_redirects, manage_feeds]
---

# SEO & syndication (public content sites)

For a site meant to be found - a blog, marketing site, docs, a podcast - three platform capabilities make it discoverable. A page already gets its `<title>` from `metadata.title` (the page/layout law); these add everything else.

## Per-page meta, Open Graph, canonical

After a page exists, `manage_seo set_meta` attaches its `description`, `og` (a map keyed `title`/`description`/`image` for link previews), `canonical` URL, plus optional `title`, `structured_data` (JSON-LD), `twitter_card` and `locale`. **`set_meta` REPLACES the whole record**: a second call passing only `og` wipes the description and canonical, so send the complete set each time. Set it per page - the description and OG image are what a search result and a shared link show, so a page without them looks blank in both. `set_meta` needs its page to exist, so set each page's meta right after the save that builds it, in the task that owns the page; `set_robots` follows once the pages it names exist.

## sitemap.xml and robots.txt

`/sitemap.xml` is served **live** - regenerated from the project's published pages on every request - so it always reflects the current set, including pages built later; there is no ordering constraint on it. Templated routes (`/blog/:slug`) are NOT listed (see "Sitemap and robots details"). `manage_seo set_robots` sets **`/robots.txt`** from the content you pass (a permissive default is served until you set one).

## Redirects

`manage_redirects add redirect_from=<path> redirect_to=<path-or-URL>` preserves links after a rename. `301` (the default) is a permanent move - use it for almost everything, since search engines transfer ranking to the target; `302` is temporary, for a move that may revert. Matching is an **exact path**, not a prefix or pattern, so add one per moved path.
## RSS / Atom feeds

{{contract:endpoint-routes}}

`manage_feeds` builds a feed at `/feed/<path>` from a table (the content collection - see how collections are modelled in the plan): map `title_column`, `body_column`, `date_column`, and optionally `link_column`; `format` is `rss` (default) or `atom`. **The feed is public and not row-scoped**: set `where` (for example published-only) or drafts leak; also set `title`, `description` and an absolute `link`. Then advertise it from the layout `<head>` so readers and browsers auto-discover it:

```html
<link rel="alternate" type="application/rss+xml" title="Blog" href="/feed/blog">
```

<!-- extended -->

## Sitemap and robots details
> when: article URLs are missing from the sitemap, or a custom robots.txt lost its Sitemap line.

A templated route (`/blog/:slug`) is a pattern, not a URL: it is not listed and its rows are not enumerated, so article URLs are missing from `/sitemap.xml`; surface them through the feed and internal links. A custom `robots.txt` REPLACES the default one including its `Sitemap:` line, so add `Sitemap: https://<your-domain>/sitemap.xml` yourself. Redirects apply to page requests only (not `/api` or `/files`) and drop the query string. A feed's `item_limit` defaults to 50 and its `link` to the relative `/`.

## Keep the feed and the site reading one source
> when: the site has both HTML pages and a feed over the same content.

A feed is just another VIEW of the same content table the pages read - point `manage_feeds` at that table, don't maintain a parallel list. New rows appear in the feed automatically (bounded by `item_limit`, newest first), so there is nothing to regenerate when content changes - the served `/sitemap.xml` is likewise always current.
