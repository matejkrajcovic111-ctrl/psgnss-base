/* Drops one panel on another and then pulls each of the eight handles in turn,
   reporting what the layout did. Reads are async: React commits after the
   event, so a synchronous read reports the state before the gesture. */
const log = [];
const tiles = () => [...document.querySelectorAll('.tile')];
const heads = () => [...document.querySelectorAll('.tile h2')].map(h => h.textContent.split(' ')[0]);
const at = el => { const r = el.getBoundingClientRect(); return { x: r.left + r.width/2, y: r.top + r.height/2 }; };
const wait = ms => new Promise(r => setTimeout(r, ms));
const geom = i => { const t = tiles()[i], s = getComputedStyle(t); const r = t.getBoundingClientRect();
  return `span=${s.getPropertyValue('--span').trim()} h=${s.getPropertyValue('--tile-h').trim()} box=${Math.round(r.width)}x${Math.round(r.height)}`; };
const pe = (t,x,y) => new PointerEvent(t,{bubbles:true,cancelable:true,composed:true,clientX:x,clientY:y,
  pointerId:1,pointerType:'mouse',button:t==='pointerdown'?0:-1,buttons:t==='pointerup'?0:1});

const pull = async (i, dir, dx, dy) => {
  const before = geom(i);
  const h = tiles()[i].querySelector('.tile-' + dir);
  if (!h) { log.push(`${dir.padEnd(2)} MISSING HANDLE`); return; }
  const p = at(h);
  h.dispatchEvent(pe('pointerdown', p.x, p.y));
  await wait(40);
  const lit = getComputedStyle(tiles()[i].querySelector('.tile-' + dir)).opacity;
  window.dispatchEvent(pe('pointermove', p.x + dx, p.y + dy));
  await wait(40);
  window.dispatchEvent(pe('pointerup', p.x + dx, p.y + dy));
  await wait(80);
  log.push(`${dir.padEnd(2)} drag(${dx>=0?'+':''}${dx},${dy>=0?'+':''}${dy})  ${before}  ->  ${geom(i)}   handle lit=${lit}`);
};

(async () => {
  const st = document.createElement('style');           // transitions do not tick under a virtual clock
  st.textContent = '.tile-grip,.tile-handle{transition:none !important}';
  document.head.appendChild(st);
  await wait(7000);

  log.push('order          ' + heads().join(' | '));
  log.push('sizes          ' + tiles().map((_,i)=>geom(i)).join('  //  '));

  // --- swap: drop panel 0 on panel 1 ---
  const dt = new DataTransfer();
  const grip = tiles()[0].querySelector('.tile-grip');
  const de = (t,el,p) => el.dispatchEvent(new DragEvent(t,{bubbles:true,cancelable:true,dataTransfer:dt,clientX:p.x,clientY:p.y}));
  de('dragstart', grip, at(grip));
  await wait(50);
  de('dragover', tiles()[1], at(tiles()[1]));
  await wait(50);
  log.push('drop target    ' + tiles().map(t=>t.classList.contains('swap-target')?'HIGHLIT':'-').join(' '));
  log.push('passed-over unchanged: ' + (heads().join('|') === 'Satellite|Base|Multi-band'.split('|').map((_,i)=>heads()[i]).join('|')));
  de('drop', tiles()[1], at(tiles()[1]));
  de('dragend', grip, at(grip));
  await wait(120);
  log.push('after swap     ' + heads().join(' | '));
  log.push('sizes          ' + tiles().map((_,i)=>geom(i)).join('  //  '));

  // --- every handle, on whichever panel is now first ---
  await pull(0, 'e',  180,    0);
  await pull(0, 'w', -180,    0);
  await pull(0, 'n',    0, -100);
  await pull(0, 's',    0,  100);
  await pull(0, 'ne', 120,  -60);
  await pull(0, 'nw',-120,  -60);
  await pull(0, 'sw',-120,   60);
  await pull(0, 'se', 120,   60);

  try { log.push('persisted      ' + localStorage.getItem('psgnss.tiles.v1')); } catch {}
  const pre = document.createElement('pre'); pre.id='probe'; pre.textContent = log.join('\n');
  document.body.appendChild(pre);
})();
