# Embedded UI

`go:embed`-ed into the binary, so the web UI needs no network and ships as one file.

- `index.html` — the whole app (Alpine.js, inline SVG icons)
- `alpine.min.js` — vendored, not fetched from a CDN
- `md.js` — the transcript's Markdown renderer

## Why a hand-written Markdown renderer

`marked` + `DOMPurify` would be better tested, and vendoring them would have kept the single-binary property. The reason not to: `marked` does not sanitize, so it needs DOMPurify alongside it, and session text is arbitrary — an agent may have pasted a web page into a conversation. `md.js` escapes first and emits only a fixed tag set, so injection is impossible by construction rather than by a second library being present and correctly configured.

The trade-off is real: nested lists, footnotes and task lists are not supported.

## Testing md.js

Not a Go test — it needs a browser. Run it when changing `md.js`:

```bash
node internal/server/web/md_test.mjs
```

It covers the syntax the renderer claims to support and, more importantly, asserts against the **DOM** that no input produces a dangerous element, an `on*` handler, or a non-http link. String matching is not enough there: escaped text can legitimately contain `onerror=` as characters.
