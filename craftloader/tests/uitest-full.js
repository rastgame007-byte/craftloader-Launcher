const { JSDOM } = require('jsdom');
const fs = require('fs');
const PORT = process.argv[2];
const BASE = 'http://127.0.0.1:' + PORT;
const html = fs.readFileSync('/home/user/craftloader/ui/index.html', 'utf8');
const errors = [];
process.on('unhandledRejection', e => errors.push('unhandledRejection: ' + (e && e.message || e)));
const dom = new JSDOM(html, {
  url: BASE + '/', runScripts: 'dangerously', pretendToBeVisual: true,
  beforeParse(window) {
    window.fetch = (input, opts) => fetch(new URL(String(input), BASE).toString(), opts);
    window.addEventListener('error', e => errors.push('window.onerror: ' + e.message));
  },
});
const w = dom.window, d = w.document;
const $ = id => d.getElementById(id);
const sleep = ms => new Promise(r => setTimeout(r, ms));
async function waitOverlay(text, timeout = 90000) {
  const t0 = Date.now();
  while (Date.now() - t0 < timeout) {
    const r = $('iResult');
    if (r && !r.classList.contains('hidden') && r.textContent.includes(text)) return true;
    if (r && !r.classList.contains('hidden') && r.textContent.includes('Ошибка')) return false;
    await sleep(500);
  }
  return false;
}
let fails = 0;
const check = (name, cond) => { console.log((cond ? '✓' : '✗ FAIL'), name); if (!cond) fails++; };

(async () => {
  console.log('=== 1. первый запуск (без папки mods) ===');
  await sleep(4500);
  check('плашка «папка не найдена» НЕ показана', $('setupBar').classList.contains('hidden'));
  check('подсказка с путём есть', $('modsHint').textContent.includes('Моды скачиваются в'));
  check('карточки модов загрузились', d.querySelectorAll('#results .card').length > 0);

  console.log('=== 2. скачать мод ===');
  await w.openMod('modrinth', 'sodium');
  await sleep(2500);
  const btn = d.querySelector('#mVersions .vbtn:not(.packadd)[data-url]');
  check('кнопка Скачать есть', !!btn);
  const fn = btn && btn.dataset.fn;
  btn.click();
  await sleep(6000);
  check('кнопка сказала «Скачано»', btn && btn.textContent.includes('Скачано'));
  check('файл на диске: ' + fn, fn && fs.existsSync('/home/user/.config/craftloader/mods/' + fn));
  $('mClose').click();

  console.log('=== 3. готовая сборка (Modrinth, архивом) ===');
  w.gotoPage('packs');
  await sleep(3500);
  const pcard = d.querySelector('#pResults .card');
  check('сборки нашлись', !!pcard);
  pcard.click();
  await sleep(3000);
  const pkb = d.querySelector('.pk-install');
  check('кнопка «Установить» у версии есть', !!pkb);
  $('pkDest').value = 'file';
  pkb.click();
  check('архив скачан', await waitOverlay('Архив скачан'));
  $('iClose').click();

  console.log('=== 4. мини-сборка «Минимальная оптимизация» ===');
  const curBtn = d.querySelector('#curatedRow .btn.primary');
  check('кнопка установки мини-сборки есть', !!curBtn);
  $('curDest').value = 'instance';
  curBtn.click();
  const ok4 = await waitOverlay('Установлено файлов');
  check('мини-сборка установлена', ok4);
  const instRoot = '/home/user/.config/craftloader/instances';
  const curDir = fs.readdirSync(instRoot).map(x => instRoot + '/' + x).find(p => p.includes('opt-') && fs.existsSync(p + '/mods'));
  check('папка сборки на диске', !!curDir);
  const modCount = curDir ? fs.readdirSync(curDir + '/mods').filter(f => f.endsWith('.jar')).length : 0;
  check('моды в папке сборки: ' + modCount, modCount >= 6);
  $('iClose').click();

  console.log('=== 5. мои сборки: установить ===');
  w.gotoPage('mypacks');
  await sleep(1500);
  check('карточка сборки есть', d.querySelectorAll('#mpGrid .card').length === 1);
  d.querySelector('#mpGrid .card').click();
  await sleep(600);
  check('модалка сборки открылась', $('packModal').classList.contains('show'));
  $('pmInstall').click();
  const ok5 = await waitOverlay('Установлено файлов');
  check('моя сборка установлена', ok5);
  const myDir = '/home/user/.config/craftloader/instances/Моя сборка';
  const myMods = fs.existsSync(myDir + '/mods') ? fs.readdirSync(myDir + '/mods').filter(f => f.endsWith('.jar')).length : 0;
  check('мод в папке моей сборки: ' + myMods, myMods === 1);
  $('iClose').click();

  console.log('=== 6. страница «Игра» (после фикса launchHint) ===');
  w.gotoPage('game');
  await sleep(4500);
  const insts = d.querySelectorAll('#clInst .inst').length;
  check('карточек сборок CraftLoader: ' + insts, insts >= 2);
  check('кнопки ▶ у запускаемых сборок: ' + d.querySelectorAll('#page-game .playmini').length, d.querySelectorAll('#page-game .playmini').length >= 2);
  check('java-чип заполнен', $('lnJava').textContent.length > 3);
  check('версии игры в select', $('lnVer').options.length > 10);

  console.log('');
  console.log('=== НЕПЕРЕХВАЧЕННЫЕ ОШИБКИ ===');
  console.log(errors.length ? errors.join('\n') : 'нет ✓');
  console.log('');
  console.log(fails === 0 && errors.length === 0 ? 'ВСЁ ПРОШЛО 🎉' : 'ЕСТЬ ПРОБЛЕМЫ: ' + fails);
  process.exit(fails === 0 && errors.length === 0 ? 0 : 1);
})().catch(e => { console.error('TEST CRASH:', e && e.stack || e); process.exit(2); });
