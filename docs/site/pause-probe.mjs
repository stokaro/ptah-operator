import { createServer } from 'node:http';
import { existsSync, readFileSync, statSync } from 'node:fs';
import { join, extname } from 'node:path';
import { chromium } from 'playwright';
const mime = {'.html':'text/html','.css':'text/css','.js':'text/javascript','.mjs':'text/javascript','.json':'application/json','.svg':'image/svg+xml','.woff2':'font/woff2','.png':'image/png','.ico':'image/x-icon'};
const server = createServer((req, res) => {
  let url = decodeURIComponent((req.url || '/').split('?')[0]);
  if (url.startsWith('/edge')) url = url.slice(5) || '/';
  let f = join('dist', url);
  if (existsSync(f) && statSync(f).isDirectory()) f = join(f, 'index.html');
  if (!existsSync(f)) { res.writeHead(404); res.end('nf'); return; }
  res.writeHead(200, {'content-type': mime[extname(f)] ?? 'application/octet-stream'});
  res.end(readFileSync(f));
});
await new Promise((r) => server.listen(0, '127.0.0.1', r));
const origin = `http://127.0.0.1:${server.address().port}/edge/`;
const browser = await chromium.launch();
async function run({ pauseAfterMs, forMs }) {
  const page = await browser.newPage();
  await page.goto(origin + 'demo/drift/', { waitUntil: 'load' });
  await page.waitForSelector('[data-demo-controls]:not([hidden])');
  const typed = await page.evaluate(() => {
    const id = document.querySelector('[data-demo]').getAttribute('data-demo-scenario');
    return window.PTAH_RUNS.scenarios[id].script
      .filter(([k]) => k === 'cmd' || k === 'cont' || k === 'note').map(([, t]) => t);
  });
  await page.evaluate((wanted) => {
    window.__hit = new Set();
    const screen = document.querySelector('[data-demo-screen]');
    window.__poll = setInterval(() => {
      const text = screen.textContent;
      for (const one of wanted) if (text.includes(one)) window.__hit.add(one);
    }, 50);
  }, typed);
  const play = page.locator('[data-demo-toggle]').first();
  await play.click();
  if (pauseAfterMs) {
    await page.waitForTimeout(pauseAfterMs);
    await play.click();
    await page.waitForTimeout(400);
    await play.click();
  }
  await page.waitForTimeout(forMs);
  const hit = await page.evaluate(() => { clearInterval(window.__poll); return [...window.__hit]; });
  await page.close();
  return { typed, hit };
}
const clean = await run({ pauseAfterMs: 0, forMs: 95000 });
console.log(`clean:  ${clean.hit.length}/${clean.typed.length}`);
for (const ms of [1200, 2600, 5200]) {
  const paused = await run({ pauseAfterMs: ms, forMs: 95000 });
  const lost = paused.typed.filter((t) => clean.hit.includes(t) && !paused.hit.includes(t));
  console.log(`paused at ${ms}ms: ${paused.hit.length}/${paused.typed.length}, lost ${lost.length}`);
  for (const one of lost) console.log('   LOST:', JSON.stringify(one.slice(0, 70)));
}
await browser.close();
server.close();
