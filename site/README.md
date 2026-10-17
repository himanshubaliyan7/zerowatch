# ZeroWatch landing page

A static, single-page site: no framework and no runtime dependencies. The fonts are self-hosted.

- `src/page.html`: the source; edit this file.
- `build.mjs`: run `node site/build.mjs [previewDir]`.
  - It writes `index.html`, which loads the fonts from `fonts/`.
  - Given a directory, it also writes a single-file preview there with the fonts inlined.
- `fonts/`: Space Grotesk, DM Sans and JetBrains Mono, latin subset (OFL).

To deploy, serve this folder as-is from any static host (GitHub Pages, Cloudflare Pages, Vercel, or nginx on the VM).

## Waitlist

The form posts JSON to the URL in its `data-endpoint` attribute (`<form id="wl" data-endpoint="">` in `src/page.html`). The body looks like:

```json
{ "email": "...", "size": "20–99", "bill": "$5k–20k", "stack": ["datadog", "cloudwatch"] }
```

While the attribute is empty, the form validates input but sends nothing, and says so. Any endpoint that accepts a JSON POST and answers 2xx will work: Formspree, a Cloudflare Worker, or a small handler on our own server.

## What we took from competitors' pages (2026-10-17)

We reviewed Netdata, Coroot, SigNoz and groundcover.

| They all do | What we do |
|---|---|
| A generic hero ("full-stack observability in minutes", "for engineers and agents") | Lead with the one thing nobody else offers: keep your CloudWatch code |
| Social proof: logos, star counts, testimonials | We have none yet, so we invent none. A public "works today / in development" status list builds trust instead |
| A pricing philosophy ("no surprise bills"), with calculators on separate pages | The calculator sits inline, with every assumption printed under it |
| Comparison pages behind the nav | One factual comparison table on the page, with an invitation to send corrections |
| A live demo or playground | Not yet. The hero plot is a synthetic signal, labelled as such; a hosted demo instance is the next step |
| AI agents + MCP presented as headline features | Listed as in development, because they are |

Remember when editing: never claim a feature as working until it is in `main`, and keep the status list current.
