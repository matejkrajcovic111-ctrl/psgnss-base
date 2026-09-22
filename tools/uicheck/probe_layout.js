setTimeout(() => {
  const out = [];
  const name = el => el ? (el.tagName.toLowerCase() + '.' + (el.className.baseVal !== undefined ? el.className.baseVal : el.className).toString().split(' ').filter(Boolean).slice(0,3).join('.')) : 'none';
  const mh = document.querySelector('.map-host');
  if (mh) {
    const r = mh.getBoundingClientRect(), cs = getComputedStyle(mh);
    out.push(`map-host rect ${Math.round(r.width)}x${Math.round(r.height)} height=${cs.height} flex=${cs.flexGrow}/${cs.flexShrink}/${cs.flexBasis} tile-h=${getComputedStyle(mh.closest('.tile')||mh).getPropertyValue('--tile-h')}`);
  } else out.push('map-host MISSING');
  document.querySelectorAll('.tile').forEach(t => {
    const id = t.querySelector('h2') ? t.querySelector('h2').textContent.slice(0,22) : '?';
    const g = t.querySelector('.tile-grip'), rz = t.querySelector('.tile-resize');
    const hit = el => { if (!el) return 'none'; const r = el.getBoundingClientRect();
      return name(document.elementFromPoint(r.left + r.width/2, r.top + r.height/2)); };
    const tr = t.getBoundingClientRect();
    out.push(`tile "${id}" ${Math.round(tr.width)}x${Math.round(tr.height)} grip-hit=${hit(g)} resize-hit=${hit(rz)}`);
  });
  const pre = document.createElement('pre');
  pre.id = 'probe'; pre.textContent = out.join('\n');
  document.body.appendChild(pre);
}, 7000);
