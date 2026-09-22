/* Clicks the tab named in the URL fragment, then settles. Used to screenshot
   a page that is only reachable by clicking, which is all of them: the UI
   keeps the current tab in React state rather than in the URL. */
setTimeout(() => {
  const want = decodeURIComponent(location.hash.slice(1) || '').toLowerCase();
  if (!want) return;
  const el = [...document.querySelectorAll('button,a')]
    .find(e => e.textContent.trim().toLowerCase() === want ||
               (e.getAttribute('title') || '').toLowerCase().includes(want) ||
               (e.getAttribute('aria-label') || '').toLowerCase().includes(want));
  if (el) el.click();
  else {
    const p = document.createElement('pre');
    p.id = 'probe';
    p.textContent = 'no control matching ' + want + '\n' +
      [...document.querySelectorAll('button,a')].map(e => e.textContent.trim()).filter(Boolean).join(' | ');
    document.body.appendChild(p);
  }
}, 5000);
