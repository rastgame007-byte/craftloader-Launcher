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
async function waitOverlay(text, timeout = 120000) {
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
  await sleep(4500);
  console.log('=== 1. страница «Игра»: секция выбора сборки ===');
  w.gotoPage('game');
  await sleep(5000);
  const lcards = d.querySelectorAll('#lpMy .lpack');
  check('карточек «Мои сборки» на Игре: ' + lcards.length, lcards.length >= 1);
  check('у карточки есть кнопка «Запустить»', !!d.querySelector('#lpMy .lpackgo'));
  const meta = d.querySelector('#lpMy .lpmeta');
  check('мета сборки: ' + (meta ? meta.textContent : '—'), !!meta && meta.textContent.includes('MC'));
  check('кнопки «Запустить» у установленных сборок: ' + d.querySelectorAll('#clInst .playmini').length,
        d.querySelectorAll('#clInst .playmini').length >= 1);
  check('версий в списке (все): ' + $('lnVer').options.length, $('lnVer').options.length > 90);

  console.log('=== 2. клик «Запустить» по сборке ===');
  $('lnNick').value = 'TestChooser';
  d.querySelector('#lpMy .lpackgo').click();
  check('установка+запуск прошли', await waitOverlay('Minecraft запущен'));
  const log = '/home/user/.config/craftloader/instances/Моя сборка/logs/latest.log';
  await sleep(9000);
  let gameOk = false;
  try { gameOk = fs.readFileSync(log, 'utf8').includes('TestChooser') || fs.readFileSync(log.replace('logs/latest.log','craftloader-launch.log'),'utf8').includes('TestChooser'); } catch(e) {}
  check('игра реально стартовала с ником TestChooser', gameOk);

  console.log('=== 3. «Мои сборки»: ▶ на карточке ===');
  $('iClose').click();
  w.gotoPage('mypacks');
  await sleep(1500);
  check('на карточке сборки есть ▶', !!d.querySelector('#mpGrid .card .lpackgo'));
  console.log('ошибки:', errors.length ? errors.join('; ') : 'нет');
  console.log(fails === 0 ? 'ВСЁ ПРОШЛО 🎉' : 'ЕСТЬ ПРОБЛЕМЫ: ' + fails);
  process.exit(fails === 0 ? 1 - 1 : 1);
})().catch(e => { console.error('TEST CRASH:', e && e.stack || e); process.exit(2); });
