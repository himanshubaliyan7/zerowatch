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

It is wired to Formspree: the form "ZeroWatch waitlist" (`https://formspree.io/f/xljgzvng`) in the owner's Formspree account.
- Submissions appear under Submissions in the Formspree dashboard and are also emailed to the account address.
- The free plan allows 50 submissions a month.
- The form ID is public by design, since it ships in the page.
- Once the site has a domain, set Settings → Restrict to domain in Formspree, so other sites can't post to the form.

The single-file preview made by `build.mjs` clears the endpoint, so it shows "nothing was sent" instead of a failed request.

To switch providers, change the attribute. Any endpoint that accepts a JSON POST and answers 2xx will work.

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
