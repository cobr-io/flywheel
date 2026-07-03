# Diagram styleguide

The visual language for explanatory **diagrams / schematics** in this repo's
docs (flows, pipelines, architecture). The dev-loop diagram
([`devloop-diagram.gen.js`](./devloop-diagram.gen.js) →
[`devloop-light.svg`](./devloop-light.svg) /
[`devloop-dark.svg`](./devloop-dark.svg), embedded in the root `README.md`) is
the **reference implementation** — copy its tokens, don't reinvent them.

The goal is a calm, **engineered-schematic** look: precise, hairline, generous
whitespace, a strict role-coded palette, and a clear directional flow. Think
technical product docs, not a clip-art flowchart.

---

## Non-negotiables

1. **Vector, not raster.** Author an SVG and embed it; never paste a PNG into
   Markdown. SVG scales crisply and reflows on narrow viewports (the whole
   reason we moved off ASCII / GitHub-Mermaid).
2. **Light is primary, dark is the fallback.** Ship a `-light.svg` and a
   `-dark.svg` and select between them with `<picture>` +
   `prefers-color-scheme` (snippet below). Both must pass contrast.
3. **Generate, don't hand-edit.** Layout math and palettes live in a small
   `*.gen.js` next to the output, so wording/spacing stays re-editable and
   consistent. Regenerate after any change:
   ```
   node docs/assets/<name>.gen.js docs/assets
   ```
4. **Assets live in `docs/assets/`** — *not* `docs/guides/`, which is embedded
   verbatim into client repos. Diagram assets are flywheel-internal.
5. **No emoji, no clip-art icons.** Identity comes from the role palette, the
   numbered stations, and type — not decoration.
6. **Wording is faithful** to the prose it illustrates. The diagram restates the
   surrounding text; it doesn't invent new claims.

---

## Role palette

Exactly **three accent roles** carry meaning; everything else is neutral. Decode
them with a footer legend in every diagram.

| Role | Meaning | | Light accent | Light chip text / bg | Dark accent | Dark chip text / bg |
|---|---|---|---|---|---|---|
| `you` | the developer's actions | 🟢 | `#0BA37F` | `#07795E` / `#E1F6EF` | `#2DD4A6` | `#74ECC9` / `#0E2A24` |
| `comp` | Flywheel components | 🔵 | `#3D63F5` | `#2B45B8` / `#E8EDFE` | `#6A92FF` | `#AAC1FF` / `#17223A` |
| `flux` | Flux / GitOps | 🟣 | `#7C5CFF` | `#5A3FCF` / `#EEEAFE` | `#A78BFA` | `#CBB8FF` / `#211A3A` |

**Accent** = bars, station rings, the loop edge. **chip text on chip bg** =
small role tags and station numbers. Don't introduce a fourth accent hue; if a
new actor appears, map it to the closest existing role or extend the palette
here first.

### Neutrals

| Token | Light | Dark |
|---|---|---|
| panel (container) | `#F4F6F9` | `#10151C` |
| panel stroke / rules | `#E5E9EF` | `#232B35` |
| card | `#FFFFFF` | `#171D26` |
| card stroke | `#E7EBF1` | `#2A323D` |
| ink (titles) | `#10141A` | `#E8EEF5` |
| muted (body) | `#586172` | `#98A2AF` |
| faint / eyebrow | `#9AA3B0` / `#7B8493` | `#69727E` / `#7E8896` |
| connector / arrow | `#CDD4DE` / `#A9B2BE` | `#313A46` / `#4C5663` |
| boundary region (fill / stroke) | `#E9EEF7` / `#CFDAE8` | `#151C27` / `#3D4A5C` |
| boundary label (bg / text) | `#DBE3F0` / `#5A6473` | `#242F40` / `#9FABBC` |

In dark mode the boundary fill delta vs the panel is inevitably subtle at
these luminances, so the **stroke carries the edge** — keep it clearly
visible, not hairline-faint.

---

## Typography

System stacks only (self-contained, render anywhere an `<img>`-loaded SVG runs):

```
sans = -apple-system, BlinkMacSystemFont, 'Segoe UI', system-ui, sans-serif
mono = ui-monospace, 'SF Mono', 'JetBrains Mono', Menlo, Consolas, monospace
```

The core typographic rule: **mono for literal machine identifiers, sans for
human narration.** Component/command names (`image-builder-controller`,
`git-auto-sync pushes`) are mono; sentences about what *you* do are sans. This
gives a quiet rhythm — prose bookends, machinery in the middle.

Mono **promises literal**: a mono name must be the exact identifier the reader
can go and see — the workload name in `kubectl get pods`, the real command —
never a paraphrase ("Flux image-automation") of one
(`image-automation-controller`). Qualifiers ("in-cluster", "warm") belong in
the sans subtitle, not inside the mono name.

| Element | Font | Size | Weight | Notes |
|---|---|---|---|---|
| Card title — system identifier | mono | 16.5 | 650 | letter-spacing −0.2 |
| Card title — human narration | sans | 17.5 | 650 | letter-spacing −0.1 |
| Card subtitle | sans | 13.5 | 400 | `muted` |
| Header eyebrow | mono | 11 | 600 | UPPERCASE, letter-spacing 2.4, `eyebrow` |
| Role chip | mono | 10.5 | 600 | UPPERCASE, letter-spacing 0.8 |
| Station number | mono | 12.5 | 700 | role chip-text |
| Edge label (pill / loop) | mono | 11.5–12 | 400–600 | pill text takes the acting role's chip color; loop label the `you` chip color |
| Legend | mono | 12.5 | 400 | `muted` |

---

## Layout system

Canvas is **720px** wide (natural); height is computed from the content. The
spine is centred at **x=360**; the panel insets 12px each side; cards inset to
**x=92 (536px wide)**, leaving **80px side gutters** — the left gutter is the
return-edge lane.

| Token | Value |
|---|---|
| canvas width | 720 |
| panel inset / radius | 12px / `rx=22`, 1px stroke (soft shadow, light only) |
| card x / width / radius | 92 / 536 / `rx=16`, 1px stroke |
| spine center x | 360 |
| header height | 84 |
| gap between cards | 52 (78 before a labelled transition pill) |
| station node | `r=15`, 2px ring |
| accent bar | 4px wide, inset 16px top/bottom, `rx=2` |
| connector | 2px line + 5px chevron arrowhead |
| loop edge | 2px **dotted** (`stroke-dasharray="6 7"`), exits the last card's bottom, runs left below the boundary region, up the gutter at card-left −40, corner `r=18` |
| boundary region | rounded rect (`rx=18`) behind the cards, 12px padding around the grouped stages, hairline stroke, corner label chip |

**Depth is a whisper.** Cards: `feDropShadow` dy 3 / blur 8 / opacity .07
(light), dy 2 / blur 6 / opacity .40 (dark). Panel shadow light-only (dy 6, blur
20, opacity .06). Borders are hairline; never use a hard 1px black outline.

### Anatomy of a stage

```
            ( NN )                 ← station: numbered ring on the spine, role-colored
              │  ▼                   connector: solid 2px line + chevron, flowing DOWN
   ┌──────────────────────────┐
   ┃ Title                YOU │    ← accent bar (left) · role chip (top-right)
   ┃ subtitle in muted sans   │    ← card: white/#171D26, hairline, soft shadow
   └──────────────────────────┘
```

### Directional convention

- **Forward flow** = solid connectors down the **centre** spine, each entering a
  numbered station then a card.
- **Feedback / loop** = a **dotted** edge in a **side gutter** (the dev-loop's
  return exits the last card's bottom, runs left below the boundary region,
  then up the **left** gutter, arrow back into the first card). Its label sits
  **horizontally** under the bottom segment — never rotate label text.
  Dotted + off-spine is what distinguishes "the loop closes" from "another
  pipeline step." Use it whenever a diagram is a cycle, not a chain.

### Boundary regions

When a subset of stages shares an environment (a cluster, a process, a trust
boundary), group them in a **recessed region** — a rounded rect drawn *behind*
the cards, filled with a neutral tint a shade off the panel, hairline border,
and a small uppercase label chip in the top-left corner (e.g. `LOCAL K3D
CLUSTER`). Stages outside the boundary stay outside it (in the dev-loop the
commit is on the host, so it sits above the cluster region; `git-auto-sync
pushes` is the edge that crosses in). Keep the region **neutral** — it's
structure, not a fourth accent role — and let the white cards float on top of
the tint. Nest freely: the cluster region lives inside the outer "your machine"
panel.

---

## Embedding in the README

```html
<picture>
  <source media="(prefers-color-scheme: dark)" srcset="docs/assets/<name>-dark.svg">
  <img alt="<one sentence describing the whole flow, in order>" src="docs/assets/<name>-light.svg" width="720">
</picture>
```

- `alt` text spells out the flow end-to-end (it's the accessible + indexed
  version of the picture). Don't write "diagram".
- `width="720"` matches the natural width; the vector scales down on mobile.

---

## Previewing while you iterate

GitHub-Mermaid is theme-locked, so we render SVGs with real Chrome to match how
GitHub will rasterize. After generating:

```sh
# light on white, dark on GitHub's dark page bg
"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome" \
  --headless=new --force-device-scale-factor=2 --window-size=720,<H> \
  --screenshot=out-light.png \
  <(printf '<style>body{margin:0}img{width:720px}</style><img src="docs/assets/<name>-light.svg">')
```

---

## Adding a new diagram — checklist

1. Reuse the tokens above (clone `devloop-diagram.gen.js` as a starting point).
2. Three accent roles max; decode them in a footer legend.
3. Mono for identifiers, sans for narration. No emoji.
4. Generate `-light.svg` + `-dark.svg` into `docs/assets/`.
5. Render both (light bg `#fff`, dark bg `#0d1117`) and eyeball contrast +
   overflow before committing.
6. Embed via `<picture>` with descriptive `alt` + `width`.
7. Keep the `*.gen.js` next to its output so wording stays editable.
