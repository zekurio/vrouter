#!/usr/bin/env node
// Regenerates the icon set in web/public from the mark below. Needs rsvg-convert (librsvg).
import { execFileSync } from "node:child_process";
import { writeFileSync } from "node:fs";
import { fileURLToPath } from "node:url";

const out = fileURLToPath(new URL("../web/public/", import.meta.url));
const bg = "#191a19";
const text = "#e7e7e5";
const accent = "#79bce9";

// The mark lives on a 32-unit grid; RouterLogo in web/src/main.tsx draws the same paths.
const mark = `<g fill="none" stroke-width="3.75" stroke-linecap="round"><path d="M7.25 8.5 16 23.5" stroke="${text}"/><path d="M24.75 8.5 19.94 16.75" stroke="${accent}"/></g>`;
const icon = ({ radius = 7, scale = 1 } = {}) => {
  const body =
    scale === 1
      ? mark
      : `<g transform="translate(16 16) scale(${scale}) translate(-16 -16)">${mark}</g>`;
  return `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 32 32"><rect width="32" height="32" rx="${radius}" fill="${bg}"/>${body}</svg>\n`;
};
const png = (svg, size) =>
  execFileSync("rsvg-convert", ["-w", size, "-h", size], {
    input: svg,
    maxBuffer: 1 << 24,
  });
const ico = (sizes) => {
  const images = sizes.map((size) => png(icon(), size));
  const head = Buffer.alloc(6 + 16 * images.length);
  head.writeUInt16LE(1, 2);
  head.writeUInt16LE(images.length, 4);
  let offset = head.length;
  images.forEach((image, i) => {
    const entry = 6 + 16 * i;
    head.writeUInt8(sizes[i], entry);
    head.writeUInt8(sizes[i], entry + 1);
    head.writeUInt16LE(1, entry + 4);
    head.writeUInt16LE(32, entry + 6);
    head.writeUInt32LE(image.length, entry + 8);
    head.writeUInt32LE(offset, entry + 12);
    offset += image.length;
  });
  return Buffer.concat([head, ...images]);
};

writeFileSync(out + "icon.svg", icon());
writeFileSync(out + "favicon.ico", ico([16, 32, 48]));
writeFileSync(out + "icon-192.png", png(icon(), 192));
writeFileSync(out + "icon-512.png", png(icon(), 512));
// iOS and maskable launchers crop the tile themselves, so these are full bleed
// with the mark held inside the safe zone.
writeFileSync(
  out + "apple-touch-icon.png",
  png(icon({ radius: 0, scale: 0.9 }), 180),
);
writeFileSync(
  out + "icon-maskable-512.png",
  png(icon({ radius: 0, scale: 0.8 }), 512),
);
