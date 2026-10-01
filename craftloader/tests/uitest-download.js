const { JSDOM } = require('jsdom');
const fs = require('fs');
const PORT = process.argv[2];
const BASE = 'http://127.0.0.1:' + PORT;
const html = fs.readFileSync('/home/user/craftloader/ui/index.html', 'utf8');
const errors = [];
const dom = new JSDOM(html, {
  url: BASE + '/', runScripts: 'dangerously', pretendToBeVisual: true,
  beforeParse(window) {
    window.fetch = (input, opts) => fetch(new URL(String(input), BASE).toString(), opts);
    window.addEventListener('error', e => errors.push(e.message));
  },
});
const w = dom.window, d = w.document;
const sleep = ms => new Promise(r => setTimeout(r, ms));
(async () => {
  await sleep(3500);
  await w.openMod('modrinth', 'sodium');
  await sleep(2500);
  const btn = d.querySelector('#mVersions .vbtn:not(.packadd)[data-url]');
  const fn = btn.dataset.fn;
  btn.click();
  await sleep(6000);
  console.log('кнопка после клика:', btn.textContent.trim());
  console.log('файл ожидается:', fn);
  console.log('ошибок:', errors.length ? errors.join('; ') : 'нет');
  process.exit(errors.length ? 1 : 0);
})().catch(e => { console.error('CRASH', e); process.exit(2); });
