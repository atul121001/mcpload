#!/usr/bin/env node
// Generates docs/images/demo.svg: a looping, self-contained animated terminal
// demo built from the real output captured in transcript.txt.
//
//   node docs/images/demo/make-demo.mjs
//
// To refresh the content, re-run the commands shown in transcript.txt against
// the demo servers and paste the verdict block of each run into the matching
// scene. No external fonts or scripts: CSS keyframes inside the SVG only.

import { readFileSync, writeFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

const here = dirname(fileURLToPath(import.meta.url));
const SRC = join(here, 'transcript.txt');
const OUT = join(here, '..', 'demo.svg');

// ---- layout -------------------------------------------------------------
const COLS = 104;          // max characters per line (longer lines get "…")
const CW = 7.5;            // char cell width; textLength pins every line to it
const FONT = 12.5;
const LH = 19;             // line height
const PADX = 20;
const BAR = 30;            // title bar height
const PADTOP = 16;
const PADBOT = 16;
const WIDTH = PADX * 2 + COLS * CW; // 820

// ---- timing (seconds) ---------------------------------------------------
const CYCLE = 30;
const SCENE_STARTS = [0, 14];       // scene i runs [start_i, start_{i+1})
const TYPE_DELAY = 0.8;             // after scene start, before typing
const TYPE_CPS = 32;                // typing speed, chars per second
const TYPE_MAX = 3.0;               // cap on typing duration
const ENTER_PAUSE = 0.7;            // pause after typing before "enter"
const FIRST_OUT = 0.25;             // enter -> first output line
const STEP = 0.3;                   // gap between output lines
const RESULT_GAP = 0.6;             // extra pause before the result line

// ---- colours (own dark background; independent of GitHub theme) --------
const C = {
  bg: '#0d1117', bar: '#161b22', border: '#30363d',
  fg: '#e6edf3', dim: '#8b949e', msg: '#b1bac4', prompt: '#58a6ff',
  pass: '#3fb950', fail: '#f85149', skip: '#8b949e', warn: '#d29922',
};
const STATUS_COLOR = { PASS: C.pass, FAIL: C.fail, SKIPPED: C.skip, WARN: C.warn };

// ---- parse transcript ---------------------------------------------------
const scenes = [];
for (const raw of readFileSync(SRC, 'utf8').split(/\r?\n/)) {
  if (raw.startsWith('#')) continue;
  if (raw.startsWith('=== scene')) { scenes.push({ cmd: '', out: [] }); continue; }
  const s = scenes.at(-1);
  if (!s) continue;
  if (raw.startsWith('$ ')) s.cmd = raw.slice(2);
  else s.out.push(raw);
}
for (const s of scenes) while (s.out.length && s.out.at(-1).trim() === '') s.out.pop();
if (scenes.length !== SCENE_STARTS.length) throw new Error(`expected ${SCENE_STARTS.length} scenes, got ${scenes.length}`);

// ---- helpers ------------------------------------------------------------
const esc = (t) => t.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
const len = (t) => [...t].length;
const trunc = (t, n = COLS) => (len(t) > n ? [...t].slice(0, n - 1).join('') + '…' : t);
const pct = (t) => `${+((t / CYCLE) * 100).toFixed(3)}%`;
const fx = (n) => +n.toFixed(2);

const styles = [];
let kid = 0;
// Element visible from time a (seconds into the cycle) until time b.
function showBetween(a, b = CYCLE, cls = '') {
  const name = `k${kid++}`;
  const frames = [];
  if (a > 0) frames.push(`0%{opacity:0}`, `${pct(a)}{opacity:1}`);
  else frames.push(`0%{opacity:1}`);
  if (b < CYCLE) frames.push(`${pct(b)}{opacity:0}`, `100%{opacity:0}`);
  styles.push(`@keyframes ${name}{${frames.join('')}}`);
  return `class="an${cls ? ' ' + cls : ''}" style="animation-name:${name}"`;
}
// Typing cover slides right in `n` steps between times a and b.
function slide(a, b, n, dx) {
  const name = `k${kid++}`;
  styles.push(
    `@keyframes ${name}{0%{transform:translateX(0)}${pct(a)}{transform:translateX(0);animation-timing-function:steps(${n},end)}` +
      `${pct(b)}{transform:translateX(${fx(dx)}px)}100%{transform:translateX(${fx(dx)}px)}}`,
  );
  return `class="an" style="animation-name:${name}"`;
}

const rowY = (i) => BAR + PADTOP + i * LH + FONT; // text baseline of row i
const textEl = (x, i, content, chars, extra = '') =>
  `<text x="${fx(x)}" y="${fx(rowY(i))}" textLength="${fx(chars * CW)}" lengthAdjust="spacingAndGlyphs"${extra}>${content}</text>`;

function outputLine(line) {
  const t = trunc(line);
  const m = t.match(/^(\s+)(PASS|FAIL|SKIPPED|WARN)(\s+)(\S+)(\s+)(.*)$/);
  if (m) {
    const [, s0, st, s1, id, s2, msg] = m;
    return {
      chars: len(t),
      html:
        `${esc(s0)}<tspan fill="${STATUS_COLOR[st]}" font-weight="bold">${st}</tspan>${esc(s1)}` +
        `<tspan fill="${C.fg}">${esc(id)}</tspan>${esc(s2)}<tspan fill="${C.msg}">${esc(msg)}</tspan>`,
    };
  }
  const r = t.match(/^(mcpload result: )(PASS|FAIL)(.*)$/);
  if (r) {
    const col = r[2] === 'PASS' ? C.pass : C.fail;
    return {
      chars: len(t),
      html: `<tspan fill="${C.fg}" font-weight="bold">${esc(r[1])}</tspan><tspan fill="${col}" font-weight="bold">${r[2]}</tspan>${esc(r[3])}`,
    };
  }
  if (t.startsWith('mcpload verdicts')) return { chars: len(t), html: `<tspan fill="${C.fg}">${esc(t)}</tspan>` };
  return { chars: len(t), html: esc(t) }; // dim (inherits)
}

// ---- build scenes -------------------------------------------------------
const rows = Math.max(...scenes.map((s) => 1 + s.out.length));
const HEIGHT = BAR + PADTOP + rows * LH + PADBOT;
const body = [];
const timeline = [];

scenes.forEach((s, si) => {
  const start = SCENE_STARTS[si];
  const end = SCENE_STARTS[si + 1] ?? CYCLE;
  const cmd = trunc(s.cmd, COLS - 2);
  const n = len(cmd);
  const typeA = start + TYPE_DELAY;
  const typeB = typeA + Math.min(TYPE_MAX, n / TYPE_CPS);
  const enter = typeB + ENTER_PAUSE;
  const g = [];

  g.push(`<g ${showBetween(start, end, si === 0 ? 's0' : '')}>`);
  // prompt + full command (revealed by the sliding cover)
  g.push(textEl(PADX, 0, '$', 1, ` fill="${C.prompt}" font-weight="bold"`));
  const cmdX = PADX + 2 * CW;
  g.push(textEl(cmdX, 0, esc(cmd), n, ` fill="${C.fg}"`));
  const top = BAR + PADTOP + 2;
  g.push(
    `<g class="cv"><g ${slide(typeA, typeB, n, n * CW)}>` +
      `<rect x="${fx(cmdX)}" y="${fx(top - 2)}" width="${fx(n * CW + CW * 2)}" height="${LH}" fill="${C.bg}"/>` +
      `<g ${showBetween(start, enter)}><rect class="blink" x="${fx(cmdX)}" y="${fx(top)}" width="${CW}" height="${LH - 4}" fill="${C.fg}"/></g>` +
      `</g></g>`,
  );
  timeline.push({ scene: si + 1, typing: [typeA, typeB], enter });

  let t = enter + FIRST_OUT;
  let row = 1;
  for (const line of s.out) {
    const isResult = line.startsWith('mcpload result:');
    if (isResult) t += RESULT_GAP;
    if (line.trim() !== '') {
      const o = outputLine(line);
      g.push(`<g ${showBetween(t, end)}>${textEl(PADX, row, o.html, o.chars)}</g>`);
    }
    if (isResult) timeline.push({ scene: si + 1, result: +t.toFixed(2), holdSeconds: +(end - t).toFixed(2) });
    row += 1;
    t += line.trim() === '' ? STEP / 2 : STEP;
  }
  g.push('</g>');
  body.push(g.join('\n'));
});

// ---- assemble -----------------------------------------------------------
const font = `ui-monospace,SFMono-Regular,'SF Mono',Menlo,Consolas,'DejaVu Sans Mono','Liberation Mono',monospace`;
const css = [
  `.an{animation-duration:${CYCLE}s;animation-iteration-count:infinite;animation-timing-function:step-end;animation-fill-mode:both}`,
  `text{white-space:pre}`,
  `.blink{animation:blink 1s step-end infinite}`,
  `@keyframes blink{0%{opacity:1}50%{opacity:0}}`,
  ...styles,
  // Reduced motion: show the final frame of the last scene, no animation.
  `@media (prefers-reduced-motion:reduce){.an,.blink{animation:none!important}.s0,.cv{display:none}}`,
].join('\n');

const dots = [C.fail, C.warn, C.pass]
  .map((c, i) => `<circle cx="${20 + i * 18}" cy="${BAR / 2}" r="5.5" fill="${c}" opacity=".85"/>`)
  .join('');

const svg = `<svg xmlns="http://www.w3.org/2000/svg" width="${WIDTH}" height="${HEIGHT}" viewBox="0 0 ${WIDTH} ${HEIGHT}" role="img" aria-label="mcpload demo: a healthy MCP server run ends in PASS; a run behind a load balancer without sticky sessions ends in FAIL session_not_found" xml:space="preserve">
<title>mcpload demo</title>
<style>
${css}
</style>
<rect x=".5" y=".5" width="${WIDTH - 1}" height="${HEIGHT - 1}" rx="9" fill="${C.bg}" stroke="${C.border}"/>
<path d="M.5 ${BAR}V9.5a9 9 0 0 1 9-9h${WIDTH - 19}a9 9 0 0 1 9 9V${BAR}z" fill="${C.bar}"/>
<line x1=".5" y1="${BAR}" x2="${WIDTH - 0.5}" y2="${BAR}" stroke="${C.border}"/>
${dots}
<text x="${WIDTH / 2}" y="${BAR / 2 + 4}" text-anchor="middle" font-family="${font}" font-size="12" fill="${C.dim}">mcpload</text>
<g font-family="${font}" font-size="${FONT}" fill="${C.dim}">
${body.join('\n')}
</g>
</svg>
`;

writeFileSync(OUT, svg);
console.log(`wrote ${OUT}: ${WIDTH}x${HEIGHT}, ${Buffer.byteLength(svg)} bytes, cycle ${CYCLE}s`);
console.log(JSON.stringify(timeline));
