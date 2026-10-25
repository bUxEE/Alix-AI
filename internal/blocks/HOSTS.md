# Rich blocks in a host application: renderers and content-security policy

Audience: the developer of an application that embeds AlixAI and renders its conversation.
Source: the schemas are `schemas/conversation/rich-block.schema.json`
and `turn-event.schema.json`. This note describes what the core guarantees, what it does not, and
what the host must do. It belongs with `docs/integration/conversation-objects.md` (section 9).

## What the core guarantees, and what it leaves to you

AlixAI is headless. It ships no renderer and no user interface. Before a block is sent, the core:

- validates it against the published schema and refuses it (`schema_invalid`) if it does not fit;
- rewrites Markdown into a canonical form that contains no raw HTML, no image, no reference-style
  link and no link outside the schemes the deployment allows (by default none: a link is reduced to
  its text);
- refuses any title, label, table cell, chart value or diagram text that contains markup, a URL, a
  template marker, a formula, or a control, zero-width or bidirectional character;
- bounds tables, charts and diagrams, and never truncates one to fit;
- refuses a chart that shows a number that is not an authoritative fact of the turn.

The core does **not** know how your page will use the data. A block that is safe as data becomes
unsafe if a renderer turns a string into code, so everything below is the host's side of the bargain.
Treat the core's checks as defence in depth, never as the only barrier: a renderer must be safe on
its own.

## Renderer enablement

The host decides which renderers run. A client that does not support a block type shows its text
alternative (`description` for a chart or a diagram) and must not fail. Recommended defaults:

| Block | Default | Rule for the renderer |
| --- | --- | --- |
| `markdown` | on | Render with a CommonMark renderer in safe mode (raw HTML off, no images). Pass the HTML through your own sanitiser (for example DOMPurify with an element and attribute allowlist) before it touches the DOM, and set it under a Trusted Types policy. Never assign model text to `innerHTML` directly. |
| `token` events | on | The text of a token event is a fragment of one Markdown document that the core sanitises as a whole before it cuts it into fragments (`Turn.PublishResponse` in `internal/stream`). Accumulate the fragments and render the accumulated text; do not render each fragment on its own, because a construct can span two fragments. Render it like a `markdown` block, under the same rules. |
| `code` | on | Show verbatim with `textContent` (a highlighter must also emit text nodes only). Never execute, evaluate or open it. |
| `table` | on | Render every cell as text. A cell is a string, a number, a boolean or null; the column `kind` is a formatting hint, not a type guarantee. Do not export a cell to a spreadsheet without escaping a leading `=`, `+`, `-` or `@`. |
| `chart` | on, if you have a spec-driven renderer | Build the chart from the declared fields only: the mark, the channel-to-field mapping and the records. Do not hand the spec to a charting library as an options object, and never enable a library feature that evaluates strings (formatter or tooltip templates, expression languages, HTML tooltips). Titles and labels go in as text. Show the `description` as the accessible alternative. |
| `diagram` | off | Show `description` and the source as a code view. Enable only if you run Mermaid in strict mode (`securityLevel: "strict"`, HTML labels off) inside a sandboxed frame (below). The block says `requires_host_renderer: true` for this reason. |
| `citation_list`, `action_card`, `clarification_choices`, `confirmation_panel` | on | These are structured data from the core and the host. Render the fields; do not build markup from them. A confirmation panel's confirm control is yours: it sends the structured confirmation, never text. |

Unknown block types, unknown members and unknown event types are not an error for the page: skip
them and show the text alternative. The core never relies on a client understanding a block in order
to be safe.

## Content-Security-Policy for the page that renders blocks

Serve the page that shows AlixAI content with a policy at least this strict. Replace the origin of
your AlixAI deployment where marked.

```
Content-Security-Policy:
  default-src 'none';
  script-src 'self';
  style-src 'self';
  img-src 'self' blob:;
  font-src 'self';
  connect-src 'self' https://alixai.example.test wss://alixai.example.test;
  frame-src 'none';
  object-src 'none';
  base-uri 'none';
  form-action 'self';
  frame-ancestors 'self';
  require-trusted-types-for 'script';
  trusted-types alixai-markdown
```

Notes on each part:

- `script-src 'self'` with no `'unsafe-inline'` and no `'unsafe-eval'`. Rich blocks never need either.
  Bundle your charting library with your application; do not load it from a third-party origin.
- `style-src 'self'`. If your chart library writes inline `style` attributes, prefer one that does
  not; if you must, use a per-response nonce, not `'unsafe-inline'`.
- `img-src 'self' blob:`. No block contains an image URL. Markdown images are removed by the core;
  generated or uploaded images arrive as opaque file references, which your own server resolves into
  a same-origin URL. Never add a wildcard or `https:` to `img-src`: an image URL is a request to a
  third party and a way to leak what the user is looking at.
- `connect-src` lists the origin that serves the stream: a fetch-based reader and an `EventSource`
  use `https:`; a WebSocket needs the `wss:` form listed explicitly. The context token travels in a
  header or a cookie, never in the URL (an `EventSource` cannot set headers, so use the SDK's
  fetch-based reader, which can).
- `frame-src 'none'` and `frame-ancestors 'self'`. If you enable diagrams, serve the renderer from a
  separate document with its own policy (below) and allow only that origin in `frame-src`.
- `require-trusted-types-for 'script'` makes every `innerHTML` assignment an error unless it goes
  through your named policy. Keep that policy small: it takes the output of your Markdown sanitiser
  and nothing else.

### The diagram frame, if you enable diagrams

Render Mermaid in an iframe with `sandbox="allow-scripts"` and **without** `allow-same-origin`, so it
has an opaque origin and no access to your cookies, storage or DOM. Serve the framed document with:

```
Content-Security-Policy: default-src 'none'; script-src 'self'; style-src 'unsafe-inline'; img-src data:; base-uri 'none'; form-action 'none'
```

Pass the source in with `postMessage` after checking the origin, receive the rendered SVG back as
text, and sanitise it with an SVG-aware sanitiser before inserting it. Do not enable Mermaid's click
handlers or its HTML labels.

### The AlixAI API origin

The core answers JSON and event streams. It sets `X-Content-Type-Options: nosniff` and
`Cache-Control: no-store` on them, and does not set a Content-Security-Policy of its own. At your
reverse proxy, add `Content-Security-Policy: default-src 'none'; frame-ancestors 'none'` to every
response from the API origin, so that nothing it returns can ever be framed or run as a document.
Allow cross-origin browser access to the stream only for your own page origins: the WebSocket
endpoint checks `Origin` against the request host and an explicit allowlist (`AllowedOrigins`), and
refuses any other browser origin.

## Checks you can run

- The valid and invalid examples in `schemas/conversation/examples/rich-block/` are what the core
  accepts and refuses; run your renderer over the valid ones and confirm it shows text only.
- `ChartDigest` (package `internal/blocks`) is a SHA-256 over the mark, the channel-to-field mapping
  and the marks derived from the data. A host that wants to test its chart renderer can compute the
  marks the same way and compare.
- Load the page with the policy above and a block whose text is `<img src=x onerror=alert(1)>`; the
  console should show text, and no report-only violation.
