// One-off generator: Lucide __iconNode → SVG (template black + regular light) →
// rsvg-convert PNG @44px → cmd/unarr-desktop/menuicons/<name>_{template,regular}.png
// Run: node gen-menu-icons.mjs
import fs from "node:fs";
import path from "node:path";
import os from "node:os";
import { execFileSync } from "node:child_process";

const distGlob = process.env.LUCIDE_DIST;
if (!distGlob) throw new Error("set LUCIDE_DIST to the lucide dist/esm/icons dir");
const OUT = process.env.OUT_DIR;
if (!OUT) throw new Error("set OUT_DIR");
fs.mkdirSync(OUT, { recursive: true });

// menu-item name -> lucide icon file
const MAP = {
  status: "activity",
  account: "circle-user-round",
  version: "tag",
  upgrade: "sparkles",
  pause: "pause",
  resume: "play",
  restart: "rotate-cw",
  enable: "hard-drive-download",
  open: "globe",
  library: "library-big",
  downloads: "folder-open",
  configure: "sliders-horizontal",
  edit: "file-pen",
  player: "monitor-play",
  logs: "scroll-text",
  sendlogs: "life-buoy",
  docs: "book-open",
  update: "circle-arrow-up",
  quit: "power",
};

function iconNodeOf(icon) {
  const src = fs.readFileSync(path.join(distGlob, `${icon}.js`), "utf8");
  const m = src.match(/const __iconNode = (\[[\s\S]*?\]);/);
  if (!m) throw new Error(`no __iconNode in ${icon}`);
  // The array literal is plain data (tag + attrs objects) — eval it in isolation.
  return Function(`"use strict";return (${m[1]})`)();
}

function svg(nodes, color) {
  const inner = nodes
    .map(([tag, attrs]) => {
      const a = Object.entries(attrs)
        .filter(([k]) => k !== "key")
        .map(([k, v]) => `${k}="${v}"`)
        .join(" ");
      return `<${tag} ${a} />`;
    })
    .join("");
  return `<svg xmlns="http://www.w3.org/2000/svg" width="24" height="24" viewBox="0 0 24 24" fill="none" stroke="${color}" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">${inner}</svg>`;
}

const tmp = fs.mkdtempSync(path.join(os.tmpdir(), "menuicons-"));
let n = 0;
for (const [name, icon] of Object.entries(MAP)) {
  const nodes = iconNodeOf(icon);
  for (const [variant, color] of [
    ["template", "#000000"],
    ["regular", "#d0d0d0"],
  ]) {
    const svgPath = path.join(tmp, `${name}_${variant}.svg`);
    fs.writeFileSync(svgPath, svg(nodes, color));
    execFileSync("rsvg-convert", [
      "-w", "44", "-h", "44",
      "-o", path.join(OUT, `${name}_${variant}.png`),
      svgPath,
    ]);
    n++;
  }
}
console.log(`wrote ${n} PNGs to ${OUT}`);
