// Source-of-truth generator for the README dev-loop diagram, and the reference
// implementation for the shared diagram style — see ./STYLEGUIDE.md.
// Regenerate after editing card text / palette:
//   node docs/assets/devloop-diagram.gen.js docs/assets
// Emits devloop-light.svg + devloop-dark.svg (embedded in README via <picture>).
//
// Generator for the Flywheel dev-loop schematic (light + dark SVGs).
// Aesthetic: refined engineered timeline — vertical spine, numbered stations,
// role-coded cards, mono for system identifiers, sans for human narration.
const fs = require('fs');

const esc = (s) => String(s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');

const SANS = "-apple-system, BlinkMacSystemFont, 'Segoe UI', system-ui, sans-serif";
const MONO = "ui-monospace, 'SF Mono', 'JetBrains Mono', Menlo, Consolas, monospace";

// role keys: you | comp | flux
// Card titles are the LITERAL workload names as seen in `kubectl get pods`
// (git-server / image-builder-controller in flywheel-system,
// image-automation-controller in flux-system) — mono promises literal.
const cards = [
  { n: '01', role: 'you',  font: 'sans', title: 'you save + git commit',            sub: ['in your app’s worktree'] },
  { n: '02', role: 'comp', font: 'mono', title: 'git-server',                       sub: ['a bare in-cluster mirror of your worktree'] },
  { n: '03', role: 'comp', font: 'mono', title: 'image-builder-controller',         sub: ['runs a build-<app> Job per commit (warm BuildKit)', 'and pushes the image to the local registry'] },
  { n: '04', role: 'flux', font: 'mono', title: 'image-automation-controller',      sub: ['commits the image bump to your gitops repo;', 'Flux rolls out the Deployment from git'] },
  { n: '05', role: 'you',  font: 'sans', title: 'your pod is running the new code',  sub: ['typically a few seconds · ~30s worst case'] },
];
// transition pill keyed by the index of the card it leads INTO
const pills = { 1: 'git-auto-sync pushes' };

const roleLabel = { you: 'YOU', comp: 'FLYWHEEL', flux: 'FLUX' };

const palettes = {
  light: {
    pageBg: 'transparent',
    panel: '#F4F6F9', panelStroke: '#E5E9EF',
    card: '#FFFFFF', cardStroke: '#E7EBF1',
    ink: '#10141A', muted: '#586172', faint: '#9AA3B0',
    eyebrow: '#7B8493',
    connector: '#CDD4DE', arrow: '#A9B2BE',
    pillBg: '#FFFFFF', pillStroke: '#DCE2EA', pillText: '#475569',
    nodeFill: '#FFFFFF',
    you:  { accent: '#0BA37F', chipText: '#07795E', chipBg: '#E1F6EF' },
    comp: { accent: '#3D63F5', chipText: '#2B45B8', chipBg: '#E8EDFE' },
    flux: { accent: '#7C5CFF', chipText: '#5A3FCF', chipBg: '#EEEAFE' },
    cluster: { fill: '#E9EEF7', stroke: '#CFDAE8', chipBg: '#DBE3F0', chipText: '#5A6473' },
    cardShadow: 'flood-color="#0B1220" flood-opacity="0.07"', cardDy: 3, cardBlur: 8,
    panelShadow: true,
  },
  dark: {
    pageBg: 'transparent',
    panel: '#10151C', panelStroke: '#232B35',
    card: '#171D26', cardStroke: '#2A323D',
    ink: '#E8EEF5', muted: '#98A2AF', faint: '#69727E',
    eyebrow: '#7E8896',
    connector: '#313A46', arrow: '#4C5663',
    pillBg: '#171D26', pillStroke: '#2A323D', pillText: '#9AA4B1',
    nodeFill: '#171D26',
    you:  { accent: '#2DD4A6', chipText: '#74ECC9', chipBg: '#0E2A24' },
    comp: { accent: '#6A92FF', chipText: '#AAC1FF', chipBg: '#17223A' },
    flux: { accent: '#A78BFA', chipText: '#CBB8FF', chipBg: '#211A3A' },
    // The boundary must survive dark mode: the fill delta vs the panel is
    // inevitably subtle at these luminances, so the stroke carries the edge.
    cluster: { fill: '#151C27', stroke: '#3D4A5C', chipBg: '#242F40', chipText: '#9FABBC' },
    cardShadow: 'flood-color="#000000" flood-opacity="0.40"', cardDy: 2, cardBlur: 6,
    panelShadow: false,
  },
};

// ---- layout ----
const W = 720;
const PANEL_X = 12, PANEL_W = W - 24;          // 12..708
const CARD_X = 92, CARD_W = 536;               // 92..628, center 360
const CX = CARD_X + CARD_W / 2;                // spine center = 360
const PAD_L = 34;                              // text left padding inside card
const GAP = 52;                                // vertical gap between cards
const GAP_PILL = 78;                           // wider gap where a transition pill sits
const NODE_R = 15;
const PANEL_TOP = 12;
const HEADER_H = 84;                            // clearance so station 01 clears the header
const cardH = (c) => (c.sub.length === 2 ? 112 : 92);

let y = PANEL_TOP + HEADER_H;                   // first card top
const layout = cards.map((c, i) => {
  const top = y;
  const h = cardH(c);
  y = top + h + (pills[i + 1] ? GAP_PILL : GAP);
  return { ...c, top, h };
});
const lastBottom = layout[layout.length - 1].top + layout[layout.length - 1].h;
const BUS_Y = lastBottom + 40;                 // loop-return bottom segment, below the cluster edge
const LEGEND_Y = BUS_Y + 56;                   // clears the return wire + its label row
const PANEL_H = (LEGEND_Y + 26) - PANEL_TOP;
const TOTAL_H = PANEL_TOP + PANEL_H + 14;

function chip(p, role, yTop) {
  const r = p[role];
  const label = roleLabel[role];
  const w = label.length * 6.6 + 22;
  const cx = CARD_X + CARD_W - 22 - w;          // right aligned, 22 inset
  const cy = yTop + 18;
  return `<g>
    <rect x="${cx.toFixed(1)}" y="${cy.toFixed(1)}" width="${w.toFixed(1)}" height="20" rx="10" fill="${r.chipBg}"/>
    <text x="${(cx + w / 2).toFixed(1)}" y="${(cy + 14).toFixed(1)}" font-family="${MONO}" font-size="10.5" font-weight="600" letter-spacing="0.8" fill="${r.chipText}" text-anchor="middle">${label}</text>
  </g>`;
}

function card(p, c) {
  const r = p[c.role];
  const x = CARD_X, w = CARD_W, top = c.top, h = c.h;
  const titleFont = c.font === 'mono' ? MONO : SANS;
  const titleY = top + (c.sub.length === 2 ? 42 : 41);
  let subY = titleY + 24;
  const subs = c.sub.map((s, i) => {
    const t = `<text x="${x + PAD_L}" y="${(subY + i * 19).toFixed(1)}" font-family="${SANS}" font-size="13.5" fill="${p.muted}">${esc(s)}</text>`;
    return t;
  }).join('\n    ');
  return `<g>
    <rect x="${x}" y="${top}" width="${w}" height="${h}" rx="16" fill="${p.card}" stroke="${p.cardStroke}" filter="url(#cardShadow)"/>
    <rect x="${x + 0.5}" y="${top + 16}" width="4" height="${h - 32}" rx="2" fill="${r.accent}"/>
    ${chip(p, c.role, top)}
    <text x="${x + PAD_L}" y="${titleY.toFixed(1)}" font-family="${titleFont}" font-size="${c.font === 'mono' ? 16.5 : 17.5}" font-weight="650" fill="${p.ink}" letter-spacing="${c.font === 'mono' ? -0.2 : -0.1}">${esc(c.title)}</text>
    ${subs}
  </g>`;
}

function station(p, c) {
  const r = p[c.role];
  const cy = c.top - NODE_R - 8;                 // node centered in the gap above the card
  return { cy, svg: `<g>
    <circle cx="${CX}" cy="${cy.toFixed(1)}" r="${NODE_R}" fill="${p.nodeFill}" stroke="${r.accent}" stroke-width="2"/>
    <text x="${CX}" y="${(cy + 4.5).toFixed(1)}" font-family="${MONO}" font-size="12.5" font-weight="700" fill="${r.chipText}" text-anchor="middle">${c.n}</text>
  </g>` };
}

function connectors(p) {
  let out = '';
  for (let i = 0; i < layout.length; i++) {
    const c = layout[i];
    const st = station(p, c);
    // line from node bottom into the card top, with arrowhead
    out += `<line x1="${CX}" y1="${(st.cy + NODE_R).toFixed(1)}" x2="${CX}" y2="${(c.top - 5).toFixed(1)}" stroke="${p.connector}" stroke-width="2"/>`;
    out += `<path d="M ${CX - 5} ${(c.top - 8).toFixed(1)} L ${CX} ${(c.top - 1).toFixed(1)} L ${CX + 5} ${(c.top - 8).toFixed(1)}" fill="none" stroke="${p.arrow}" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"/>`;
    if (i > 0) {
      const prevBottom = layout[i - 1].top + layout[i - 1].h;
      out += `<line x1="${CX}" y1="${prevBottom}" x2="${CX}" y2="${(st.cy - NODE_R).toFixed(1)}" stroke="${p.connector}" stroke-width="2"/>`;
      if (pills[i]) {
        const label = pills[i];
        const pw = label.length * 7.0 + 26;
        const py = prevBottom + ((st.cy - NODE_R) - prevBottom) / 2;
        out += `<g>
          <rect x="${(CX - pw / 2).toFixed(1)}" y="${(py - 12).toFixed(1)}" width="${pw.toFixed(1)}" height="24" rx="12" fill="${p.pillBg}" stroke="${p.pillStroke}"/>
          <text x="${CX}" y="${(py + 4).toFixed(1)}" font-family="${MONO}" font-size="12" fill="${p.comp.chipText}" text-anchor="middle">${esc(label)}</text>
        </g>`;
      }
    }
    out += st.svg;
  }
  return out;
}

// Boundary region around the stages that run inside the local k3d cluster
// (git-server → builder → Flux → pod). The commit (stage 01) sits outside it
// on the host; git-auto-sync is the push that crosses into the cluster. Drawn
// behind the cards as a recessed, neutrally-tinted zone with a corner label.
function clusterRegion(p) {
  const r = p.cluster;
  const inFirst = layout[1];                     // first in-cluster stage
  const inLast = layout[layout.length - 1];
  const top = inFirst.top - 2 * NODE_R - 16;     // clear above station 02
  const bottom = inLast.top + inLast.h + 16;
  const x = CARD_X - 12;
  const w = CARD_W + 24;
  const label = 'LOCAL K3D CLUSTER';
  const lw = label.length * 7.2 + 22;
  const cx = x + 16, cy = top + 11;
  return `<g>
    <rect x="${x}" y="${top.toFixed(1)}" width="${w}" height="${(bottom - top).toFixed(1)}" rx="18" fill="${r.fill}" stroke="${r.stroke}" stroke-width="1"/>
    <rect x="${cx}" y="${cy.toFixed(1)}" width="${lw.toFixed(1)}" height="22" rx="8" fill="${r.chipBg}"/>
    <text x="${(cx + lw / 2).toFixed(1)}" y="${(cy + 15).toFixed(1)}" font-family="${MONO}" font-size="10" font-weight="600" letter-spacing="1.5" fill="${r.chipText}" text-anchor="middle">${label}</text>
  </g>`;
}

// The feedback return that makes it a loop, not a chain: the wire exits the
// last card's bottom (crossing OUT of the cluster region, as the commit
// crossed in), runs left below the cluster, then up the left gutter and back
// into the first card — a dotted "you" edge (the developer closes the loop by
// observing + editing again). The label sits horizontally under the bottom
// segment, where it's actually readable.
function loopEdge(p) {
  const r = p.you;
  const first = layout[0];
  const last = layout[layout.length - 1];
  const y1 = first.top + first.h / 2;
  const exitY = last.top + last.h;                // bottom edge of the last card
  const xCard = CARD_X;                           // left edge of the cards
  const xBus = xCard - 40;                         // vertical return lane (left gutter)
  const R = 18;
  const label = 'you observe the result → make the next change';
  const d = [
    `M ${CX} ${exitY}`,
    `V ${(BUS_Y - R).toFixed(1)}`,
    `Q ${CX} ${BUS_Y} ${CX - R} ${BUS_Y}`,
    `H ${xBus + R}`,
    `Q ${xBus} ${BUS_Y} ${xBus} ${(BUS_Y - R).toFixed(1)}`,
    `V ${(y1 + R).toFixed(1)}`,
    `Q ${xBus} ${y1.toFixed(1)} ${xBus + R} ${y1.toFixed(1)}`,
    `H ${xCard - 8}`,
  ].join(' ');
  return `<g>
    <path d="${d}" fill="none" stroke="${r.accent}" stroke-width="2" stroke-dasharray="6 7" stroke-linecap="round"/>
    <path d="M ${xCard - 9} ${(y1 - 5).toFixed(1)} L ${xCard - 1} ${y1.toFixed(1)} L ${xCard - 9} ${(y1 + 5).toFixed(1)}" fill="none" stroke="${r.accent}" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"/>
    <text x="${((xBus + CX) / 2).toFixed(1)}" y="${(BUS_Y + 22).toFixed(1)}" font-family="${MONO}" font-size="11.5" font-weight="600" fill="${r.chipText}" text-anchor="middle">${esc(label)}</text>
  </g>`;
}

function header(p) {
  const cy = PANEL_TOP + 34;
  const text = 'EVERYTHING RUNS ON YOUR MACHINE';
  const tx = CX;
  return `<g>
    <line x1="${PANEL_X + 28}" y1="${cy - 4}" x2="${CX - 160}" y2="${cy - 4}" stroke="${p.panelStroke}" stroke-width="1"/>
    <line x1="${CX + 160}" y1="${cy - 4}" x2="${PANEL_X + PANEL_W - 28}" y2="${cy - 4}" stroke="${p.panelStroke}" stroke-width="1"/>
    <text x="${tx}" y="${cy}" font-family="${MONO}" font-size="11" font-weight="600" letter-spacing="2.4" fill="${p.eyebrow}" text-anchor="middle">${text}</text>
  </g>`;
}

function legend(p) {
  const items = [
    { role: 'you', label: 'you' },
    { role: 'comp', label: 'flywheel' },
    { role: 'flux', label: 'flux' },
  ];
  // compute total width to center
  const gap = 26, dot = 9;
  const parts = items.map((it) => ({ ...it, w: dot + 8 + it.label.length * 7 }));
  const total = parts.reduce((a, b) => a + b.w, 0) + gap * (parts.length - 1);
  let x = CX - total / 2;
  const y = LEGEND_Y;
  let out = `<line x1="${PANEL_X + 28}" y1="${y - 22}" x2="${PANEL_X + PANEL_W - 28}" y2="${y - 22}" stroke="${p.panelStroke}" stroke-width="1"/>`;
  for (const it of parts) {
    const r = p[it.role];
    out += `<circle cx="${(x + dot / 2).toFixed(1)}" cy="${(y - 4).toFixed(1)}" r="${dot / 2}" fill="${r.accent}"/>`;
    out += `<text x="${(x + dot + 8).toFixed(1)}" y="${(y).toFixed(1)}" font-family="${MONO}" font-size="12.5" fill="${p.muted}">${it.label}</text>`;
    x += it.w + gap;
  }
  return out;
}

function build(name) {
  const p = palettes[name];
  const panelShadow = p.panelShadow
    ? `<rect x="${PANEL_X}" y="${PANEL_TOP}" width="${PANEL_W}" height="${PANEL_H}" rx="22" fill="${p.panel}" filter="url(#panelShadow)"/>`
    : '';
  const svg = `<svg xmlns="http://www.w3.org/2000/svg" width="${W}" height="${TOTAL_H}" viewBox="0 0 ${W} ${TOTAL_H}" font-family="${SANS}">
  <defs>
    <filter id="cardShadow" x="-20%" y="-20%" width="140%" height="160%">
      <feDropShadow dx="0" dy="${p.cardDy}" stdDeviation="${p.cardBlur}" ${p.cardShadow}/>
    </filter>
    <filter id="panelShadow" x="-20%" y="-20%" width="140%" height="140%">
      <feDropShadow dx="0" dy="6" stdDeviation="20" flood-color="#0B1220" flood-opacity="0.06"/>
    </filter>
  </defs>
  ${panelShadow}
  <rect x="${PANEL_X}" y="${PANEL_TOP}" width="${PANEL_W}" height="${PANEL_H}" rx="22" fill="${p.panel}" stroke="${p.panelStroke}" stroke-width="1"/>
  ${clusterRegion(p)}
  ${header(p)}
  ${connectors(p)}
  ${layout.map((c) => card(p, c)).join('\n  ')}
  ${loopEdge(p)}
  ${legend(p)}
</svg>`;
  return svg;
}

const dir = process.argv[2] || '.';
fs.writeFileSync(`${dir}/devloop-light.svg`, build('light'));
fs.writeFileSync(`${dir}/devloop-dark.svg`, build('dark'));
console.log(`W=${W} H=${TOTAL_H} panelH=${PANEL_H} cards=${layout.length}`);
console.log('wrote devloop-light.svg, devloop-dark.svg');
