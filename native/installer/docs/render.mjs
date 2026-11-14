// Render the installer's snapshot pages to 1920x1080 PNGs with headless
// Chromium. The wizard writes NAME.html (with NAME.ansi and NAME.txt) each
// time you press s, into --snapshot-dir; docs/splash-gif.sh writes one per
// splash frame.
//
//   node native/installer/docs/render.mjs SNAPSHOT_DIR OUT_DIR [NAME=OUT ...]
//
// With no NAME=OUT pairs it renders every NAME.html to OUT_DIR/NAME.png.
// Uses Playwright from the talk-video-record skill (TALK_VIDEO_LIB, default
// ~/.kiro/crew/skills/talk-video-record/scripts/lib.mjs). Opens local files
// only; listens on nothing.
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';

const lib = process.env.TALK_VIDEO_LIB
  || path.join(os.homedir(), '.kiro/crew/skills/talk-video-record/scripts/lib.mjs');
const R = await import(lib);

const [src, out, ...pairs] = process.argv.slice(2);
if (!src || !out) {
  console.error('usage: node native/installer/docs/render.mjs SNAPSHOT_DIR OUT_DIR [NAME=OUT ...]');
  process.exit(2);
}
let jobs = pairs.map((p) => p.split('='));
if (jobs.length === 0) {
  jobs = fs.readdirSync(src).filter((f) => f.endsWith('.html')).map((f) => [f.slice(0, -5), f.slice(0, -5)]);
}
fs.mkdirSync(out, { recursive: true });
const ctx = await R.newContext({ viewport: { width: 1920, height: 1080 } });
const page = await ctx.newPage();
for (const [name, dest] of jobs) {
  const file = path.resolve(src, `${name}.html`);
  if (!fs.existsSync(file)) throw new Error(`no ${file}`);
  await page.goto(R.fileUrl(file));
  await page.evaluate(() => document.fonts.ready);
  const png = path.join(out, `${dest}.png`);
  await page.screenshot({ path: png });
  console.log(png);
}
await R.closeBrowser();
