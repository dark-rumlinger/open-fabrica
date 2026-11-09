---
slug: llm_endpoints
role: capability
title: Hosted AI endpoints and RAG
summary: A chat/generate endpoint over the project's LLM - SSE vs JSON consumption, structured output, and RAG with a knowledge base.
use_when: The app calls an LLM at runtime - a chatbot or AI assistant, a "generate / summarize / classify X" action, or chat-over-your-documents (RAG). The platform proxies the project's model; no provider key in the browser.
not_when: A plain CRUD or static app with no AI feature, or a one-off text transform that needs no model.
category: ai
position: 26
tools: [manage_llm_proxy, manage_knowledge]
---

# Hosted AI endpoints (any AI assistant / "generate X" / chat-over-docs feature)

The platform exposes the project's configured LLM to end users through `manage_llm_proxy` - no provider client code, no key in the browser. One `create` mints two endpoints: `POST /api/<path>/chat` (SSE token stream) and `POST /api/<path>/complete` (one JSON body). The `system_prompt`, `model_id`, and limits live on the endpoint config, never in the request.

**Request shape (both endpoints):** `{messages:[{role,content}, ...]}` with roles `user`/`assistant` ONLY and the LAST message a `user` turn. The system prompt is on the endpoint - do NOT prepend a system message.

## Choose: stream vs whole-answer

- **`/complete`** returns the whole answer as one JSON body - the default. Use it for "generate X", classification, anything consumed at once; read with `await res.json()`.
- **`/chat`** returns a `text/event-stream` of token deltas for a typing/chat feel. **Do NOT call `res.json()` on it** - read the stream and accumulate.

**The frames are NAMED events carrying JSON, not bare text deltas**, and there is no `[DONE]` sentinel: `event: token` with `{"text":"..."}` (append `.text`), then one `event: done` with `{stop_reason, usage}` and the stream closes. A failure arrives in-band as `event: error` with `{"error":"..."}` on an HTTP 200 - show an error state, never as text. So parse each frame's event name and JSON body (appending the raw line puts `{"text":"Hel"}`, braces and all, in the bubble), and buffer ACROSS reads: a frame straddles two chunks routinely.

```js
const res = await fetch('/api/assistant/chat', { method: 'POST', ... });  // request shape above
const reader = res.body.getReader(), dec = new TextDecoder();
let buf = '';
for (;;) {
  const { value, done } = await reader.read();
  if (done) break;
  buf += dec.decode(value, { stream: true });
  const frames = buf.split('\n\n');
  buf = frames.pop();                       // keep the partial frame for the next read
  for (const frame of frames) {
    let name = 'message', body = '';
    for (const line of frame.split('\n')) {
      if (line.startsWith('event: ')) name = line.slice(7).trim();
      else if (line.startsWith('data: ')) body += line.slice(6);
    }
    let p; try { p = JSON.parse(body); } catch { continue; }
    if (name === 'token') bubble.textContent += p.text;
    else if (name === 'error') showError(p.error);   // NOT into the bubble
  }
}
```

On `requires_auth`, both routes read the **`Authorization` header only** (no `?token=` fallback) - use `apiFetch` from `'auth'`.

## Structured output ("generate X" the UI parses)

For a feature whose UI parses the reply as data (a quiz, extracted fields, a list), set `response_schema` (a JSON Schema) on the endpoint and call `/complete` (`/chat` ignores it). The model must then emit conforming JSON as the reply's `content`: `JSON.parse((await res.json()).content)` is reliable (the reply is `{ content, model, stop_reason, usage }`; no `data` wrapper).

**Keep the schema tight.** List every object field in `required` unless genuinely optional - the tighter the schema, the harder to answer with the wrong shape (the other way `generate` fails: one retry, then **502** "did not match the required JSON shape"). A field holding a whole document (a full page, a complete stylesheet) is the fragile case - thousands of escaped characters inside one JSON string - so prefer a plain endpoint with NO `response_schema` returning text, or split the generation so each call fills one field.

**Size `max_tokens` to the reply you ask for.** Setting `response_schema` at `create` defaults `max_tokens` to 8192 (an `update` does not, so pass `max_tokens` there); an explicit `max_tokens` always wins. That 8192 default fits a *small* structured reply (a quiz, a few fields, a short list), but the WHOLE reply must fit under the cap in one response, including every character of any HTML/CSS/markdown packed into a schema string field. A reply cut off at the cap is unparseable, so the endpoint returns **502** (`error: output truncated at max_tokens`) instead of broken JSON. The cap is a ceiling, not a target: you pay for the tokens actually generated, so sizing it generously is free until they are used.

> **Generating a document?** A full HTML page, a complete stylesheet, a long article or report, an email body - the 8192 default truncates these mid-output, and it is the most common cause of a 502 on a `generate` endpoint. Set `max_tokens` to the expected size:
> - one long field (a full HTML `<body>`, or a complete CSS file): `16384`
> - a self-contained HTML document (markup + inline CSS), or several long fields in one object like `{html, css, ...}`: `32768`
> - estimate ~1 token per 4 characters (a 24 KB page is ~6000 tokens of content, more once JSON-escaped) and leave 2-3x headroom.
>
> If the richest output still truncates, split the generation across calls (structure first, styling second) or pin a higher-output `model_id`; one reply can never exceed the model's own output limit. Raise `daily_token_budget` to match the larger per-call spend.

## Cost guard (do this for any public endpoint)

A hosted LLM endpoint spends the owner's tokens on every call. Set `daily_token_budget` (per-endpoint per-UTC-day cap) and keep `rate_limit` (per-IP/min) - an unbudgeted public endpoint is a spend leak. Set `requires_auth` when only signed-in users should reach it.

<!-- extended -->

## RAG: answers grounded in your documents
> when: the assistant must answer FROM uploaded/authored content (a docs bot, a policy Q&A, support over a manual).

Don't stuff documents into the system prompt. Build a knowledge base and bind it:

1. `manage_knowledge create_base` names the base; then fill it from wherever the owner's documents are:
   - `ingest` with `text`: content the owner pasted into the chat.
   - `ingest` with `from_file`: a project file - one you saved, or one the owner uploaded in the Files tab. Text files, `.pdf` and `.docx` are extracted; a scanned PDF has no text layer and yields nothing. For anything longer than a page, ask the owner to upload the file rather than paste it.
   - `ingest_path`: a file or whole folder on the machine running Open Fabrica (`recursive` for subfolders), for an existing docs directory.
2. Create the chat endpoint with `knowledge_base` set to that base's name. The proxy now retrieves the most relevant chunks per request and grounds the reply in them automatically - you do NOT pass context in `messages`.
3. Iterate retrieval quality with `manage_knowledge retrieve` (inspect what a query pulls back) before shipping.

`knowledge_base` can be bound or changed through `update`; pass `"none"` to unbind it, and `list` reports the current binding.

RAG needs an embeddings-capable provider; if `manage_knowledge` is unavailable the project has none configured - tell the owner rather than faking retrieval by prompt-stuffing.

A knowledge base holds DOCUMENTS an AI endpoint answers from. It is not how the app's own table rows become searchable by meaning: that is `manage_vector_search` (below), which only you can query (`action=semantic_search`; an `ai_embed` trigger just keeps the vectors current). The app's visitors have no semantic-search endpoint, so do not build a visitor-facing "search by meaning" box on it; give visitors keyword search (`?q=` on a `create_api` endpoint) or an AI endpoint over a knowledge base instead.

## AI inside the data flow (not a chat UI)
> when: classify / summarise / embed a row as it's written, with no user chatting.

That is automation, not an endpoint: a `manage_triggers` hook of type `llm_call` (summarise/classify/tag a new row; config `{prompt, system?, max_tokens?, save_to_table, save_to_column, save_where}` - without the three `save_*` keys the reply is generated and discarded, so give them to store it, e.g. `save_where: {"id":"{{id}}"}` to update the row that fired) or `ai_embed` (keep a row embedded for `manage_vector_search`) on `data.insert` / `data.update`. `ai_embed` writes into the table's vector index, so create that first with `manage_vector_search action=create_vector_index` (naming the text columns to embed); with that in place its config is just `{}` - the row and its text come from the event and the index. It embeds rows written AFTER the trigger exists; for rows already in the table run `manage_vector_search action=embed` once. The `llm_call` default `max_tokens` is 512, so set it explicitly for a long summary. `llm_call` sends only the expanded `prompt`, not the event payload: pull row fields in with `{{column}}`. See the `event_actions` guide. Reserve `manage_llm_proxy` for a surface the END USER talks to.
