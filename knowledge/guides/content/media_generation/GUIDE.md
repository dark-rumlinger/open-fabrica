---
slug: media_generation
role: capability
title: Images - generate, fetch, transform
summary: Produce project imagery at build time - AI generation from a prompt, stock-photo fetch, and resize/thumbnail/optimize - all saved as /files artifacts you reference by URL.
category: content
position: 51
tools: [manage_image_gen, manage_media, manage_stockphotos]
---

# Project imagery (generate, fetch, transform)

Three tools produce images while the project is being BUILT, and they all land the same way: the image is saved as a project artifact (`kind=file`) under `/files`, and you reference it by that stable `/files` URL in a page, CSS `background-image`, or a table column.

- **`manage_image_gen generate`** - an AI image from a text `prompt` (marketing art, an illustration, a hero). Saved to `/files`; the result's `images[].path` is a bare filename, so reference it as `/files/<path>`.
- **`manage_stockphotos`** - `search` to preview, then `fetch` to download N real photos by query into `/files` (Pexels / Unsplash / Pixabay).
- **`manage_media`** - `resize` / `thumbnail` / `optimize` a JPEG or PNG ALREADY in `/files` (from generation, a stock fetch, or an upload), writing a new `/files` asset. So the pipeline is generate-or-fetch first, then transform.

The load-bearing fact: these bake an asset **into the project at build time** and hand you a `/files` URL - they are not runtime endpoints, so a page cannot call them when a visitor loads it. Reference the produced `/files` path; do not hotlink an external stock URL or guess a path.

<!-- extended -->

## Limits and availability
> when: a fetch or transform does not behave as expected, or a tool is missing.

`manage_stockphotos fetch` re-runs the query and takes the top N: previewed photos cannot be picked individually, and files are named `<slug>-<n>.jpg`, overwriting an earlier fetch of the same query. `manage_media` accepts JPEG/PNG up to 10 MB, writes JPEG bytes whatever extension you name, resizes nearest-neighbour, and caps output at 20k px per side / 40 MP. `manage_image_gen` and `manage_stockphotos` exist only when an image or stock provider is configured: if absent, tell the owner rather than hotlinking.

## Runtime, user-supplied images go through uploads
> when: a visitor needs to supply their own image while using the app.

Build-time generation/fetch is for the project's OWN imagery. When the END USER contributes an image at runtime (an avatar, a photo attachment), that is the `file_uploads` capability - a `POST /api/<path>/upload` whose returned `url` you store - not `manage_image_gen`. Reach for image generation only for assets you're producing as part of the build.
