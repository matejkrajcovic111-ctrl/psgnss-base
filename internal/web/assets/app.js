/* PSGNSS dashboard. React via UMD + htm, so there is no build step and the
   page works on a Pi with no internet access. */
const { useState, useEffect, useCallback, useRef } = React;
const html = htm.bind(React.createElement);

/* uPlot draws every time-series chart: 50 kB of canvas where Plotly was 3.6 MB,
   which was most of what a cold visit had to download before anything appeared.
   The live instruments -- skyplot and SNR bars -- are hand-drawn SVG further
   down, because a polar scatter with a label inside every marker was never
   something a general charting library did well here. Vendored and loaded
   lazily, like Leaflet: only map tiles ever come from the network. */
let uplotPromise;
function loadUplot() {
  if (window.uPlot) return Promise.resolve(window.uPlot);
  if (!uplotPromise) uplotPromise = new Promise((resolve,reject) => {
    const css=document.createElement('link');
    css.rel='stylesheet'; css.href='uplot-1.6.32.min.css';
    document.head.appendChild(css);
    const script=document.createElement('script');
    script.src='uplot-1.6.32.min.js'; script.async=true;
    script.onload=()=>resolve(window.uPlot);
    script.onerror=()=>{uplotPromise=null;reject(new Error('Could not load chart engine'))};
    document.head.appendChild(script);
  });
  return uplotPromise;
}
/* Leaflet loads the same way, and only when a tile source is configured: with
   no map there is no reason to fetch 145 kB, and the station is the only thing
   in this UI that ever talks to another origin. Vendored, not from a CDN ---
   only the tiles come from the network. */
let leafletPromise;
function loadLeaflet() {
  if (window.L) return Promise.resolve(window.L);
  if (!leafletPromise) leafletPromise = new Promise((resolve,reject) => {
    const css=document.createElement('link');
    css.rel='stylesheet'; css.href='leaflet-1.9.4.css';
    document.head.appendChild(css);
    const script=document.createElement('script');
    script.src='leaflet-1.9.4.js'; script.async=true;
    script.onload=()=>resolve(window.L);
    script.onerror=()=>{leafletPromise=null;reject(new Error('Could not load the map engine'))};
    document.head.appendChild(script);
  });
  return leafletPromise;
}

/* StationMap plots the base position on raster tiles. The marker is a circle
   drawn by Leaflet rather than its default icon, so no image assets are needed
   and the page keeps working when the tile host is unreachable: the map stays
   blank-grey, the marker and the readout still show. */
function StationMap({ position, map, coverage }) {
  const host=useRef(null), instance=useRef(null), marker=useRef(null), ring=useRef(null), fitted=useRef(0);
  const [err,setErr]=useState('');
  const tiles=map&&map.tiles, attribution=(map&&map.attribution)||'';
  const lat=position&&position.latitude, lon=position&&position.longitude;
  const rangeKM=(coverage&&coverage.range_km)||0;
  /* One source of truth for the circle's colour: the --range token, so the
     light theme gets its darker orange instead of a hard-coded one. */
  const rangeColour=()=>(getComputedStyle(document.documentElement).getPropertyValue('--range')||'').trim()||'#f0883e';
  useEffect(()=>{
    if(!tiles||lat==null||lon==null||!host.current)return;
    let active=true;
    loadLeaflet().then(L=>{
      if(!active||!host.current)return;
      if(!instance.current){
        instance.current=L.map(host.current,{attributionControl:true,zoomControl:true,
          scrollWheelZoom:false,keyboard:true}).setView([lat,lon],16);
        L.tileLayer(tiles,{maxZoom:19,attribution}).addTo(instance.current);
        marker.current=L.circleMarker([lat,lon],{radius:7,weight:2,color:'#52b7e8',
          fillColor:'#52b7e8',fillOpacity:.65}).addTo(instance.current);
      }else{
        marker.current.setLatLng([lat,lon]);
      }
      marker.current.bindTooltip(t('Base antenna'),{permanent:false});
      /* The working range is why this map is zoomed out at all: an antenna on
         its own needs no map. The view is fitted to the circle once per radius
         so that a two-second telemetry poll does not undo the operator's own
         panning, and the circle is non-interactive so it never swallows a
         click meant for the map. */
      if(rangeKM>0){
        if(!ring.current){
          const colour=rangeColour();
          ring.current=L.circle([lat,lon],{radius:rangeKM*1000,color:colour,weight:2,
            dashArray:'5 4',fillColor:colour,fillOpacity:.14,interactive:false}).addTo(instance.current);
        }else{ ring.current.setLatLng([lat,lon]); ring.current.setRadius(rangeKM*1000); }
        if(fitted.current!==rangeKM){ fitted.current=rangeKM; instance.current.fitBounds(ring.current.getBounds(),{padding:[16,16]}); }
      }else{
        if(ring.current){ ring.current.remove(); ring.current=null; fitted.current=0; }
        instance.current.setView([lat,lon],instance.current.getZoom());
      }
    }).catch(e=>{if(active)setErr(e.message||t('Could not load the map engine'))});
    return ()=>{active=false};
  },[tiles,attribution,lat,lon,rangeKM,LANG]);
  useEffect(()=>()=>{if(instance.current){instance.current.remove();instance.current=null}},[]);
  /* The panel can be resized under the map, and Leaflet only measures its
     container when it is told to. */
  useEffect(()=>{
    const el=host.current;
    if(!el||typeof ResizeObserver!=='function')return;
    const ro=new ResizeObserver(()=>{if(instance.current)instance.current.invalidateSize()});
    ro.observe(el);
    return ()=>ro.disconnect();
  },[]);
  if(lat==null||lon==null)return null;
  return html`<div class="card plot-card map-card">
    <h2>${t("Base Position and Working Range")}</h2>
    ${!tiles?html`<p class="note map-note">${t("No tile source configured, so no map is shown. Settings → Station and service can set one.")}</p>`
      :err?html`<p class="err map-note">${err}</p>`
      :html`<div class="map-host" ref=${host}></div>`}
    ${rangeKM>0?html`<div class="map-range" title=${coverage.note}>
      <b class="mono">${t('Usable range ≈ {n} km',{n:rangeKM})}</b>
      <span class="muted">${coverage.limit==='accuracy'?t('limited by the 50 mm accuracy budget'):t('limited by ambiguity resolution')} · ${t('{n} constellations',{n:coverage.constellations})} · ${t('{n} bands',{n:coverage.bands})}</span>
    </div>`:null}
    <div class="map-readout mono muted">${lat.toFixed(7)}, ${lon.toFixed(7)} · ${position.height.toFixed(3)} ${t('m ellipsoidal')}</div>
  </div>`;
}

/* Theme. Tabler switches on data-bs-theme, this stylesheet's tokens switch on
   data-theme, and "auto" pins neither: the operating system decides and the
   canvases repaint through useThemeEpoch. The choice is remembered per
   browser, and a browser that refuses storage simply follows the system. */
const THEME_KEY='psgnss.theme';
const THEME_NEXT={auto:'light',light:'dark',dark:'auto'};
const THEME_LABEL={auto:'Theme: following the system',light:'Theme: light',dark:'Theme: dark'};
const readTheme=()=>{try{return THEME_NEXT[localStorage.getItem(THEME_KEY)]?localStorage.getItem(THEME_KEY):'auto'}catch{return 'auto'}};
function applyTheme(mode){
  const root=document.documentElement;
  if(mode==='auto')root.removeAttribute('data-theme'); else root.setAttribute('data-theme',mode);
  const dark=mode==='dark'||(mode==='auto'&&window.matchMedia('(prefers-color-scheme: dark)').matches);
  root.setAttribute('data-bs-theme',dark?'dark':'light');
}
function useThemeMode(){
  const [mode,setMode]=useState(readTheme);
  useEffect(()=>{
    applyTheme(mode);
    try{localStorage.setItem(THEME_KEY,mode)}catch{/* private window: the choice just does not persist */}
    if(mode!=='auto')return;
    const mq=window.matchMedia('(prefers-color-scheme: dark)');
    const sync=()=>applyTheme('auto');
    mq.addEventListener('change',sync);
    return ()=>mq.removeEventListener('change',sync);
  },[mode]);
  return [mode,setMode];
}
function ThemeButton(){
  const [mode,setMode]=useThemeMode();
  const icon={
    light:html`<svg viewBox="0 0 24 24" aria-hidden="true"><circle cx="12" cy="12" r="4"></circle><path d="M12 2v2M12 20v2M4.9 4.9l1.4 1.4M17.7 17.7l1.4 1.4M2 12h2M20 12h2M4.9 19.1l1.4-1.4M17.7 6.3l1.4-1.4"></path></svg>`,
    dark:html`<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M21 12.8A9 9 0 1 1 11.2 3a7 7 0 0 0 9.8 9.8Z"></path></svg>`,
    auto:html`<svg viewBox="0 0 24 24" aria-hidden="true"><circle cx="12" cy="12" r="9"></circle><path d="M12 3a9 9 0 0 1 0 18Z"></path></svg>`,
  };
  return html`<button class="icon-button theme-button" title=${t(THEME_LABEL[mode])} aria-label=${t(THEME_LABEL[mode])}
    onClick=${()=>setMode(THEME_NEXT[mode])}>${icon[mode]}</button>`;
}

/* The language switch shows the language it switches to. setLang changes the
   table t() reads; the state change on App re-renders everything under it. */
function LangButton({ onChange }) {
  const next = LANG === 'sk' ? 'en' : 'sk';
  const label = next === 'sk' ? 'Prepnúť do slovenčiny' : 'Switch to English';
  return html`<button class="icon-button lang-button" title=${label} aria-label=${label}
    onClick=${() => { setLang(next); onChange(next); }}>${next.toUpperCase()}</button>`;
}

function useUplot() {
  const [ready,setReady]=useState(!!window.uPlot);
  useEffect(()=>{let active=true;loadUplot().then(()=>{if(active)setReady(true)}).catch(e=>console.error(e));return()=>{active=false}},[]);
  return ready;
}

/* A canvas cannot read a CSS custom property, so chart colours are resolved
   from the token block at draw time rather than written out again here. One
   palette for the whole UI, and the charts follow the light/dark switch. */
const token = name => getComputedStyle(document.documentElement).getPropertyValue(name).trim() || '#888';
const rgba = (colour,a) => {
  const h=colour.replace('#','');
  if(!/^[0-9a-fA-F]{3,8}$/.test(h)) return colour;
  const full=h.length===3?h.split('').map(c=>c+c).join(''):h.slice(0,6);
  const n=parseInt(full,16);
  return `rgba(${(n>>16)&255},${(n>>8)&255},${n&255},${a})`;
};

/* uPlot and the hand-drawn instruments need explicit pixel dimensions; the
   cards they sit in are fluid. One observer per host element reports the size
   and nothing redraws while it is unchanged. */
function useSize(ref) {
  const [size,setSize]=useState({w:0,h:0});
  useEffect(()=>{
    const el=ref.current;
    if(!el||typeof ResizeObserver!=='function')return;
    const ro=new ResizeObserver(entries=>{
      const r=entries[0].contentRect;
      setSize(s=>(Math.abs(s.w-r.width)<1&&Math.abs(s.h-r.height)<1)?s:{w:Math.round(r.width),h:Math.round(r.height)});
    });
    ro.observe(el);
    return ()=>ro.disconnect();
  },[ref]);
  return size;
}

/* Everything drawn to a canvas has to be repainted when the theme changes:
   the token values it read are now the other palette. Counting the changes is
   enough -- it is only ever used to force a redraw. */
function useThemeEpoch() {
  const [epoch,setEpoch]=useState(0);
  useEffect(()=>{
    const bump=()=>setEpoch(e=>e+1);
    const mq=window.matchMedia('(prefers-color-scheme: dark)');
    mq.addEventListener('change',bump);
    const mo=new MutationObserver(bump);
    mo.observe(document.documentElement,{attributes:true,attributeFilter:['data-theme']});
    return ()=>{mq.removeEventListener('change',bump);mo.disconnect()};
  },[]);
  return epoch;
}

/* Shared chart furniture. uPlot reads colours and axis options once, at
   construction, which is why the chart is rebuilt on a theme change rather
   than restyled. */
const chartAxis = extra => ({
  stroke:token('--dim'),
  font:token('--fs-1')+' '+token('--sans'),
  grid:{stroke:rgba(token('--line'),.7),width:1},
  ticks:{stroke:rgba(token('--line'),.7),width:1,size:4},
  ...extra,
});
const chartBase = () => ({
  cursor:{drag:{x:false,y:false},y:false,points:{size:7}},
  legend:{live:true},
  padding:[12,12,0,0],
});

/* One uPlot instance per host element: created once a width is known, handed
   new data in place afterwards, and rebuilt only when the theme, the size or
   the series shape changes. */
function UPlotChart({ data, opts, height=220, className='' }) {
  const host=useRef(null), plot=useRef(null);
  const ready=useUplot();
  const { w }=useSize(host);
  const theme=useThemeEpoch();
  const shape=opts.shape||'';
  useEffect(()=>{
    if(!ready||!w||!host.current)return;
    const built={...opts.build(),width:w,height};
    plot.current=new window.uPlot(built,data,host.current);
    return ()=>{if(plot.current){plot.current.destroy();plot.current=null;}};
  },[ready,w,height,theme,shape,LANG]);
  useEffect(()=>{if(plot.current)plot.current.setData(data)},[data]);
  /* The host is not given the chart's height: uPlot appends a live legend
     below the canvas, and a fixed box would clip it. */
  return html`<div class=${'uplot-host '+className} ref=${host}></div>`;
}

async function api(path, opts) {
  const r = await fetch(path, { credentials: 'same-origin', ...opts });
  if (r.status === 401) { throw { unauth: true }; }
  const body = await r.json().catch(() => ({}));
  if (!r.ok) { const e = new Error(body.error || `HTTP ${r.status}`); e.status = r.status; e.body = body; throw e; }
  return body;
}

const fmtBytes = n => {
  if (!n) return '0 B';
  const u = ['B', 'KB', 'MB', 'GB', 'TB'];
  const i = Math.min(Math.floor(Math.log(n) / Math.log(1024)), u.length - 1);
  return (n / Math.pow(1024, i)).toFixed(i ? 1 : 0) + ' ' + u[i];
};
/* Small zips round to "0 MB" with no decimals, which looks like a failure. */
const fmtMB = v => {
  const n = v || 0;
  if (n < 1) return (n * 1024).toFixed(0) + ' KB';
  if (n < 10) return n.toFixed(1) + ' MB';
  return n.toFixed(0) + ' MB';
};
const numberFormatter = new Intl.NumberFormat('en-GB');
const fmtNum = n => numberFormatter.format(Number(n) || 0);
/* Always 24-hour. toLocaleTimeString follows the browser locale and will show
   AM/PM on some machines, which is wrong for survey timestamps. */
const D2 = new Intl.DateTimeFormat('en-GB', {
  year: 'numeric', month: '2-digit', day: '2-digit',
  hour: '2-digit', minute: '2-digit', second: '2-digit', hour12: false });
const CEST_T2 = new Intl.DateTimeFormat('en-GB', {
  hour: '2-digit', minute: '2-digit', second: '2-digit', hour12: false, timeZone: 'Europe/Bratislava' });
const UTC_T2 = new Intl.DateTimeFormat('en-GB', {
  hour: '2-digit', minute: '2-digit', second: '2-digit', hour12: false, timeZone: 'UTC' });
const fmtDateTime = d => D2.format(new Date(d)).replace(',', '');
const fmtCESTTime = d => CEST_T2.format(new Date(d));
const fmtUTCTime = d => UTC_T2.format(new Date(d));
const fmtDur = s => {
  if (s < 60) return s + 's';
  if (s < 3600) return Math.floor(s / 60) + 'm ' + (s % 60) + 's';
  const h = Math.floor(s / 3600);
  return h + 'h ' + Math.floor((s % 3600) / 60) + 'm';
};
const displayVersion = value => {
  const match = String(value || '').match(/v?(\d+\.\d+\.\d+)/);
  return match ? 'v ' + match[1] : String(value || '');
};

/* ---------------------------------------------------------------- skyplot */

const PLOT_SYSTEMS = {
  GPS: { color:'#3fb950', short:'G' }, Galileo: { color:'#58a6ff', short:'E' },
  BeiDou: { color:'#f85149', short:'B' }, SBAS: { color:'#f2cc60', short:'S' },
  GLONASS: { color:'#ae8bea', short:'R' }, QZSS: { color:'#db61a2', short:'J' },
  NavIC: { color:'#f0883e', short:'I' },
};
const plotKey = s => s.system + ':' + s.sv;
/* NAV-SAT can name a constellation the palette does not, such as IMES or a bare
   GNSS<id>. Indexing PLOT_SYSTEMS directly with one of those unmounts the page. */
const systemStyle = sys => PLOT_SYSTEMS[sys] || { color:'var(--dim)', short:'' };
const bestCNO = s => Math.max(s.cno || 0, ...Object.values(s.signals || {}));
const sizeForCNO = n => n <= 0 ? 20 : Math.round(20 + Math.max(0, Math.min(35, n - 15)) * 14 / 35);
const trackedSatelliteCount = live => ((live && live.satellites) || []).filter(s => s.cno > 0).length;

/* The instrument panels are arranged by the person looking at them: drag a
   panel by its grip onto another and the two change places, pull an edge or a
   corner to set its width in twelfths and its height in pixels. The
   arrangement is remembered per browser, keyed by the page, and a browser that
   refuses storage simply starts from the default every time. Nothing here is
   sent to the station: this is a view preference, not configuration. */
const TILE_KEY='psgnss.tiles.v1';
const TILE_DEFAULTS=[{id:'skyplot',span:6,h:380},{id:'side',span:6,h:380},{id:'snr',span:12,h:320}];
const TILE_MIN_SPAN=3, TILE_MIN_H=180, TILE_MAX_H=900;
/* Eight handles. `x` and `y` are which way the pointer has to travel for the
   panel to grow: the right edge grows with a pointer moving right, the left
   edge with one moving left. A panel flows into the grid and has no stored
   top-left of its own, so pulling the top or the left edge resizes it from
   where the flow put it rather than moving that edge -- the size follows the
   pointer, the corner it grew from does not. */
const TILE_HANDLES=[
  {d:'n',x:0,y:-1,label:'top edge'},   {d:'s',x:0,y:1,label:'bottom edge'},
  {d:'e',x:1,y:0,label:'right edge'},  {d:'w',x:-1,y:0,label:'left edge'},
  {d:'nw',x:-1,y:-1,label:'top-left corner'},  {d:'ne',x:1,y:-1,label:'top-right corner'},
  {d:'sw',x:-1,y:1,label:'bottom-left corner'},{d:'se',x:1,y:1,label:'bottom-right corner'},
];
function loadTiles(scope){
  const base=TILE_DEFAULTS.map(t=>({...t}));
  let saved=null;
  try{ saved=JSON.parse(localStorage.getItem(TILE_KEY)||'{}')[scope]; }catch{ /* no storage */ }
  if(!Array.isArray(saved))return base;
  // Saved order first, then anything the release has added since.
  const out=[];
  saved.forEach(s=>{
    const b=base.find(x=>x.id===(s&&s.id));
    if(!b||out.some(o=>o.id===b.id))return;
    out.push({id:b.id,
      span:Math.max(TILE_MIN_SPAN,Math.min(12,Number(s.span)||b.span)),
      h:Math.max(TILE_MIN_H,Math.min(TILE_MAX_H,Number(s.h)||b.h))});
  });
  base.forEach(b=>{if(!out.some(o=>o.id===b.id))out.push(b)});
  return out;
}
function saveTiles(scope,list){
  try{
    const all=JSON.parse(localStorage.getItem(TILE_KEY)||'{}');
    all[scope]=list;
    localStorage.setItem(TILE_KEY,JSON.stringify(all));
  }catch{ /* private window: the arrangement just does not persist */ }
}

function TileGrid({ scope, tiles, reset=0 }) {
  const [layout,setLayout]=useState(()=>loadTiles(scope));
  useEffect(()=>{if(reset)setLayout(TILE_DEFAULTS.map(t=>({...t})))},[reset]);
  const [dragging,setDragging]=useState(null);
  const [over,setOver]=useState(null);
  const [resizing,setResizing]=useState(null);
  const grid=useRef(null), from=useRef(null);
  /* A pull writes a new layout on every pointer move; storage is written once,
     when the pointer comes back up. */
  useEffect(()=>{if(!resizing)saveTiles(scope,layout)},[scope,layout,resizing]);
  const shown=layout.map(l=>({...l,tile:tiles.find(t=>t.id===l.id)})).filter(l=>l.tile);
  /* Two panels change places and each keeps the size it was given, so the
     entries are exchanged whole. By id, not by index: a panel the page does
     not render (History has no map) is in the layout but not on screen. */
  const swap=(a,b)=>setLayout(l=>{
    const ia=l.findIndex(t=>t.id===a), ib=l.findIndex(t=>t.id===b);
    if(ia<0||ib<0||ia===ib)return l;
    const n=l.slice(); n[ia]=l[ib]; n[ib]=l[ia]; return n;
  });

  const startResize=(e,id,dir)=>{
    if(e.button)return;
    e.preventDefault(); e.stopPropagation();
    const handle=e.currentTarget;
    const box=handle.parentElement.getBoundingClientRect();
    const gridBox=grid.current.getBoundingClientRect();
    /* Twelve columns with a gap between them: a span is not the width divided
       by a twelfth of the grid, it is the width plus one gap divided by a
       column plus its gap. */
    const gap=parseFloat(getComputedStyle(grid.current).columnGap)||0;
    const column=(gridBox.width-gap*11)/12;
    const start=layout.find(t=>t.id===id);
    if(!start||column<=0)return;
    const sx=e.clientX, sy=e.clientY;
    /* Capture the pointer: without it the gesture is lost the moment it
       crosses a map tile or any other element that wants the pointer. */
    try{ handle.setPointerCapture(e.pointerId); }catch{ /* older browser */ }
    setResizing({id,dir:dir.d});
    const onMove=ev=>{
      /* An edge moves one dimension and leaves the other alone; a corner
         moves both. */
      const span=dir.x?Math.max(TILE_MIN_SPAN,Math.min(12,
        Math.round((box.width+dir.x*(ev.clientX-sx)+gap)/(column+gap)))):null;
      const h=dir.y?Math.max(TILE_MIN_H,Math.min(TILE_MAX_H,
        Math.round(start.h+dir.y*(ev.clientY-sy)))):null;
      setLayout(l=>l.map(t=>t.id!==id?t:{...t,span:span===null?t.span:span,h:h===null?t.h:h}));
    };
    const stop=()=>{
      setResizing(null);
      try{ handle.releasePointerCapture(e.pointerId); }catch{ /* already gone */ }
      window.removeEventListener('pointermove',onMove);
      window.removeEventListener('pointerup',stop);
      window.removeEventListener('pointercancel',stop);
    };
    window.addEventListener('pointermove',onMove);
    window.addEventListener('pointerup',stop);
    window.addEventListener('pointercancel',stop);
  };

  /* The exchange happens on the drop, not while passing over. Swapping as the
     pointer crossed each panel would trade places with every panel on the way
     to the one that was meant. */
  return html`<div class=${'tile-grid'+(resizing?' resizing':'')} ref=${grid}>
    ${shown.map(l => html`<div key=${l.id}
        class=${'tile'+(dragging===l.id?' dragging':'')+(resizing&&resizing.id===l.id?' resizing':'')+(over===l.id?' swap-target':'')}
        style=${{'--span':l.span,'--tile-h':l.h+'px'}}
        onDragOver=${e=>{if(!from.current||from.current===l.id)return;e.preventDefault();setOver(l.id)}}
        onDragLeave=${()=>setOver(o=>o===l.id?null:o)}
        onDrop=${e=>{e.preventDefault();
          if(from.current&&from.current!==l.id)swap(from.current,l.id);
          from.current=null;setOver(null);setDragging(null)}}>
      ${l.tile.node}
      <button class="tile-grip" draggable="true" title=${t("Drag onto another panel to change places")}
        aria-label=${t('Move {id}',{id:l.id})}
        onDragStart=${e=>{from.current=l.id;setDragging(l.id);e.dataTransfer.effectAllowed='move';
          try{e.dataTransfer.setData('text/plain',l.id)}catch{/* Safari */}}}
        onDragEnd=${()=>{from.current=null;setDragging(null);setOver(null)}}>
        <svg viewBox="0 0 16 16" aria-hidden="true"><circle cx="6" cy="4" r="1.3"></circle><circle cx="10" cy="4" r="1.3"></circle><circle cx="6" cy="8" r="1.3"></circle><circle cx="10" cy="8" r="1.3"></circle><circle cx="6" cy="12" r="1.3"></circle><circle cx="10" cy="12" r="1.3"></circle></svg>
      </button>
      ${TILE_HANDLES.map(hd => html`<span key=${hd.d}
        class=${'tile-handle tile-'+hd.d+(resizing&&resizing.id===l.id&&resizing.dir===hd.d?' active':'')}
        title=${t('Drag the {edge} to resize',{edge:t(hd.label)})}
        onPointerDown=${e=>startResize(e,l.id,hd)}></span>`)}
    </div>`)}
  </div>`;
}

/* The skyplot is drawn here rather than by a charting library. It is a fixed
   0-100 viewBox scaled to whatever box the card gives it, so nothing has to be
   measured and the labels keep their proportion to the dish at every size.
   Screen position: elevation is the radius (90 deg at the centre), azimuth is
   clockwise from north. */
const SKY_R = 41, SKY_C = 50, SKY_MASK = 15;
const skyRadius = elev => SKY_R * (90 - Math.max(0, Math.min(90, elev))) / 90;
const skyPoint = (elev, azim) => {
  const r = skyRadius(elev), a = (azim || 0) * Math.PI / 180;
  return { x: SKY_C + r * Math.sin(a), y: SKY_C - r * Math.cos(a) };
};
/* Marker radius carries C/N0 the way it always did: nothing below 15 dB-Hz
   grows, and the scale saturates at 50 so one strong satellite cannot swamp
   the plot. */
const skyMarker = cno => cno <= 0 ? 2.1 : 2.1 + Math.max(0, Math.min(35, cno - 15)) * 2.1 / 35;

function Skyplot({ sats, hovered, onHover }) {
  const rings = [0, 30, 60].map(e => ({ elev:e, r:skyRadius(e) }));
  const cardinals = [['N',0],['E',90],['S',180],['W',270]];
  const placed = sats.filter(s => s.elev > 0 || s.azim > 0)
    .sort((a,b) => bestCNO(a) - bestCNO(b));
  return html`<svg class="skyplot-svg" viewBox="0 0 100 100" preserveAspectRatio="xMidYMid meet"
      role="img" aria-label=${t("Satellite skyplot")}>
    <circle cx=${SKY_C} cy=${SKY_C} r=${SKY_R} class="sky-face"/>
    ${rings.map(ring => html`<circle key=${ring.elev} cx=${SKY_C} cy=${SKY_C} r=${ring.r} class="sky-ring"/>`)}
    ${[0,45,90,135].map(a => {
      const p1=skyPoint(0,a), p2=skyPoint(0,a+180);
      return html`<line key=${a} x1=${p1.x} y1=${p1.y} x2=${p2.x} y2=${p2.y} class="sky-ring"/>`;
    })}
    <circle cx=${SKY_C} cy=${SKY_C} r=${skyRadius(SKY_MASK)} class="sky-mask"/>
    ${rings.filter(r => r.elev).map(ring => html`<text key=${ring.elev} x=${SKY_C + 1} y=${SKY_C - ring.r + 3}
      class="sky-tick" fontSize="2.4">${ring.elev}°</text>`)}
    ${cardinals.map(([label,azim]) => {
      const p = skyPoint(-8, azim);
      return html`<text key=${label} x=${p.x} y=${p.y} class="sky-cardinal" fontSize="3.4"
        textAnchor="middle" dominantBaseline="central">${t(label)}</text>`;
    })}
    ${placed.map(s => {
      const p = skyPoint(s.elev, s.azim), key = plotKey(s), cfg = systemStyle(s.system);
      const on = hovered === key, low = s.elev < SKY_MASK;
      const r = skyMarker(bestCNO(s)) + (on ? 1.4 : 0);
      return html`<g key=${key} class=${'sky-sat'+(low?' low':'')+(hovered&&!on?' dim':'')}
          onMouseEnter=${()=>onHover(key)} onMouseLeave=${()=>onHover(null)}>
        <circle cx=${p.x} cy=${p.y} r=${r} fill=${cfg.color} class="sky-dot"/>
        <text x=${p.x} y=${p.y} textAnchor="middle" dominantBaseline="central"
          class="sky-label" style=${{fontSize:(on?2.9:2.4)+'px'}}>${cfg.short}${s.sv}</text>
      </g>`;
    })}
  </svg>`;
}

/* Signal-to-noise bars. Unlike the skyplot this one has to be measured: the
   satellite labels are real type and must not stretch with the box. Bars are
   grouped by satellite, one bar per band, tallest band first within a group. */
function SNRBars({ sats, hovered, onHover }) {
  const host=useRef(null);
  const { w, h }=useSize(host);
  const groups = sats.map(s => ({
    key: plotKey(s), sat: s, cfg: systemStyle(s.system),
    bands: Object.entries(s.signals||{}).sort((a,b) => a[0].localeCompare(b[0])),
  })).filter(g => g.bands.length);
  const bars = groups.reduce((n,g) => n + g.bands.length, 0);
  if (!w || !h || !bars) return html`<div class="snr-host" ref=${host}></div>`;
  const padL=30, padR=8, padT=10, padB=26, gap=7;
  const plotW = Math.max(10, w - padL - padR), plotH = Math.max(10, h - padT - padB);
  const slot = (plotW - gap * Math.max(0, groups.length - 1)) / bars;
  const barW = Math.max(2, Math.min(slot, 22));
  const yFor = v => padT + plotH * (1 - Math.max(0, Math.min(60, v)) / 60);
  let x = padL;
  const drawn = [];
  groups.forEach(g => {
    g.bands.forEach(([band, cno]) => {
      drawn.push({ ...g, band, cno, x, y:yFor(cno) });
      x += slot;
    });
    g.labelX = x - slot * g.bands.length / 2 - (slot - barW) / 2;
    x += gap;
  });
  const ticks = [0,15,30,45,60];
  return html`<div class="snr-host" ref=${host}>
    <svg width=${w} height=${h} role="img" aria-label=${t("Signal to noise ratio by band")}>
      ${ticks.map(t => html`<g key=${t}>
        <line x1=${padL - 4} y1=${yFor(t)} x2=${w - padR} y2=${yFor(t)} class="snr-grid"/>
        <text x=${padL - 7} y=${yFor(t)} textAnchor="end" dominantBaseline="central" class="snr-tick">${t}</text>
      </g>`)}
      ${drawn.map(d => {
        const on = hovered === d.key, height = Math.max(1, padT + plotH - d.y);
        return html`<g key=${d.key+d.band} class=${'snr-bar'+(hovered&&!on?' dim':'')+(d.sat.elev<SKY_MASK?' low':'')}
            onMouseEnter=${()=>onHover(d.key)} onMouseLeave=${()=>onHover(null)}>
          <rect x=${d.x} y=${d.y} width=${barW} height=${height} fill=${d.cfg.color} rx="2"/>
          ${height > 16 && barW >= 12 ? html`<text x=${d.x + barW/2} y=${d.y + 11} textAnchor="middle"
            class="snr-value">${Math.round(d.cno)}</text>` : null}
        </g>`;
      })}
      ${groups.map(g => barW >= 9 ? html`<text key=${g.key} x=${g.labelX + barW/2} y=${h - padB + 14}
        textAnchor="middle" class=${'snr-name'+(hovered&&hovered!==g.key?' dim':'')}
        >${g.cfg.short}${g.sat.sv}</text>` : null)}
      <text x="4" y=${padT + plotH/2} textAnchor="middle" class="snr-axis"
        transform=${`rotate(-90 4 ${padT + plotH/2})`}>dB-Hz</text>
    </svg>
  </div>`;
}

function GNSSPlots({ sats, signals, hiddenSystems=[], side=null, scope='dashboard' }) {
  const [visible, setVisible] = useState(() => Object.fromEntries(Object.keys(PLOT_SYSTEMS).map(k => [k, !hiddenSystems.includes(k)])));
  const [hovered, setHovered] = useState(null);
  const [reset, setReset] = useState(0);
  // NAV-SAT also reports known satellites the receiver has no carrier lock on.
  // A positive aggregate C/N0 means it is actually tracking at least one
  // signal; only those belong on a live instrument.
  const byKey = {};
  sats.forEach(s => { if (s.cno > 0 && PLOT_SYSTEMS[s.system] && visible[s.system]) byKey[plotKey(s)] = {...s, signals:{}}; });
  signals.forEach(s => { const sat = byKey[plotKey(s)]; if (sat && s.cno > 0) sat.signals[s.band] = s.cno; });
  const all = Object.values(byKey).sort((a,b) =>
    Object.keys(PLOT_SYSTEMS).indexOf(a.system) - Object.keys(PLOT_SYSTEMS).indexOf(b.system) || a.sv - b.sv);
  const hoveredSat = all.find(s => plotKey(s) === hovered);
  const panels=[
    {id:'skyplot',node:html`<div class="card plot-card"><h2>${t("Satellite skyplot")}</h2>
      <div class="plot-host skyplot-chart">
        <${Skyplot} sats=${all} hovered=${hovered} onHover=${setHovered}/>
      </div></div>`},
    side?{id:'side',node:side}:null,
    {id:'snr',node:html`<div class="card plot-card"><h2>${t("Multi-band signal-to-noise ratio")}</h2>
      <div class="plot-host snr-chart">
        <${SNRBars} sats=${all} hovered=${hovered} onHover=${setHovered}/>
      </div></div>`},
  ].filter(Boolean);
  return html`<div class="gnss-instruments">
    <div class="constellation-controls" role="group" aria-label=${t("Visible constellations")}>
      <span class="control-label">${t("Display")}</span>
      ${Object.entries(PLOT_SYSTEMS).map(([system,cfg]) => {
        const count=sats.filter(s=>s.system===system && s.cno>0).length, on=visible[system];
        return html`<button key=${system} class=${'constellation-toggle '+(on?'on':'off')}
          style=${{'--sys-color':cfg.color}} aria-pressed=${on}
          onClick=${()=>setVisible(v=>({...v,[system]:!v[system]}))}>
          <i></i><span>${system}</span><b>${count}</b></button>`;
      })}
      ${hoveredSat ? html`<span class="instrument-hover mono">
        ${systemStyle(hoveredSat.system).short}${hoveredSat.sv} · ${hoveredSat.system} ·
        ${hoveredSat.elev}° ${t('elev')} · ${hoveredSat.azim}° ${t('az')} · ${bestCNO(hoveredSat)} dB-Hz</span>` : null}
      <button class="btn act tile-reset" onClick=${()=>setReset(n=>n+1)}
        title=${t("Put the panels back where they started")}>${t("Reset layout")}</button>
    </div>
    <${TileGrid} scope=${scope} tiles=${panels} reset=${reset}/>
  </div>`;
}

/* ------------------------------------------------------------- dashboard */

function Dashboard({ live }) {
  if (!live) return html`<p class="muted">${t("Loading…")}</p>`;
  const h = live.host || {};
  const sats = live.satellites || [];
  const signals = live.signals || [];
  const trackedSats = sats.filter(s => s.cno > 0);
  const used = trackedSats.filter(s => s.used).length;
  const cn = signals.filter(s => s.cno > 0).map(s => s.cno);
  const avgCNO = cn.length ? Math.round(cn.reduce((a,b) => a+b, 0) / cn.length) : 0;
  const masked = trackedSats.filter(s => s.elev > 0 && s.elev < 15).length;
  const gps = live.gps_time || {};
  const gpsClock = gps.valid ? `GPS W${gps.week} · TOW ${Math.floor(gps.tow)} s` : t('GPS acquiring');
  return html`
    <div class="dashboard-shell">
      <div class="card dashboard-kpis">
        <div class="dashboard-titlebar">
          <span class="instrument-title">${t("Live GNSS observatory")}</span>
          <span class=${'instrument-state '+(live.online?'ok':'bad')}><i></i>${live.online?t('Online'):t('TELEMETRY STALE')}</span>
          <span class="instrument-clock mono"><span>CEST ${fmtCESTTime(live.time)}</span><i></i><span>UTC ${fmtUTCTime(live.time)}</span><i></i><span>${gpsClock}</span></span>
        </div>
        <div class="kpis">
          ${kpi(t('Tracked satellites'), trackedSats.length, t('{n} used by receiver',{n:used}))}
          ${kpi(t('Average SNR'), avgCNO ? avgCNO + ' dB-Hz' : '—', t('{n} tracked signals',{n:cn.length}))}
          ${kpi(t('Below 15° mask'), masked, t('shown grey'))}
          ${kpi('CPU', (h.cpu_percent || 0).toFixed(0) + '%', (h.temp_c || 0).toFixed(1) + ' °C')}
          ${kpi(t('Disk free'), fmtBytes((h.disk_free_mb || 0) * 1048576), t('archive volume'))}
          ${kpi(t('Memory available'), fmtBytes((h.mem_free_mb || 0) * 1048576), t('of {total} · load {load}',{total:fmtBytes((h.mem_total_mb || 0) * 1048576),load:(h.load1 || 0).toFixed(2)}))}
        </div>
      </div>

      <${GNSSPlots} sats=${sats} signals=${signals}
        hiddenSystems=${live.display_hidden_constellations||[]}
        side=${html`<${StationMap} position=${live.station_position} map=${live.map} coverage=${live.coverage}/>`}/>
    </div>`;
}

const kpi = (l, v, s) => html`<div class="kpi" key=${l}>
  <div class="l">${l}</div><div class="v">${v}</div><div class="s">${s}</div></div>`;

/* ----------------------------------------------------------------- users */

const expiryDay = value => {
  if (!value) return '';
  const d = new Date(value);
  // Calendar-date expiry is stored as 00:00 UTC on the following day so the
  // selected date remains valid through 23:59:59.
  d.setUTCDate(d.getUTCDate() - 1);
  return d.toISOString().slice(0, 10);
};

const userState = u => {
  if (!u.enabled) return [t('disabled'), 'bad'];
  if (u.expires_at && new Date(u.expires_at) <= new Date()) return [t('expired'), 'warn'];
  return [t('enabled'), 'ok'];
};

const detailEditor = d => ({
  limit: d.user.limit, enabled: d.user.enabled, password: '',
  email: d.user.email || '', note: d.user.note || '',
  expiry: expiryDay(d.user.expires_at),
  ipRules: (d.access.ip_rules || []).join('\n'),
  allMounts: !!d.access.all_mountpoints,
  mountpoints: d.access.mountpoints || [],
});

function Users() {
  const [users, setUsers] = useState([]);
  const [mounts, setMounts] = useState([]);
  const [show, setShow] = useState(false);
  const [createOpen, setCreateOpen] = useState(false);
  const [err, setErr] = useState('');
  const [form, setForm] = useState({ username: '', password: '', limit: 5, note: '' });
  const [selected, setSelected] = useState('');
  const [detail, setDetail] = useState(null);
  const [edit, setEdit] = useState(null);
  const [offset, setOffset] = useState(0);
  const [busy, setBusy] = useState(false);
  const pageSize = 25;

  const load = useCallback(async () => {
    try {
      const [u, m] = await Promise.all([
        api('/api/users' + (show ? '?passwords=1' : '')),
        api('/api/mountpoints')]);
      setUsers(u); setMounts(m.map(x => x.name)); setErr('');
    }
    catch (e) { setErr(e.message || t('failed')); }
  }, [show]);
  useEffect(() => { load(); }, [load]);

  const openUser = async (name, nextOffset = 0) => {
    setSelected(name); setOffset(nextOffset); setErr(''); setBusy(true);
    try {
      const d = await api(`/api/users/${encodeURIComponent(name)}?limit=${pageSize}&offset=${nextOffset}`);
      setDetail(d); setEdit(detailEditor(d));
    } catch (e) { setErr(e.message || t('failed')); }
    finally { setBusy(false); }
  };

  const create = async e => {
    e.preventDefault(); setErr('');
    try {
      const made = await api('/api/users', { method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ ...form, limit: Number(form.limit) || 5 }) });
      setForm({ username: '', password: '', limit: 5, note: '' });
      setCreateOpen(false);
      await load(); await openUser(made.username, 0);
    } catch (e) { setErr(e.message || t('failed')); }
  };
  const toggle = async (u) => {
    try {
      await api(`/api/users/${encodeURIComponent(u.username)}/enabled`, {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ enabled: !u.enabled }) });
      await load();
      if (selected === u.username) await openUser(u.username, offset);
    } catch (e) { setErr(e.message || t('failed')); }
  };
  const del = async (u) => {
    if (!confirm(t('Delete NTRIP user "{name}"? Past connection records are kept.',{name:u.username}))) return;
    try {
      await api(`/api/users/${encodeURIComponent(u.username)}`, { method: 'DELETE' });
      if (selected === u.username) { setSelected(''); setDetail(null); setEdit(null); }
      await load();
    } catch (e) { setErr(e.message || t('failed')); }
  };

  const save = async e => {
    e.preventDefault(); setErr(''); setBusy(true);
    const rules = edit.ipRules.split(/[\n,]+/).map(x => x.trim()).filter(Boolean);
    const body = {
      limit: Number(edit.limit), enabled: edit.enabled, note: edit.note,
      email: edit.email, expires_at: edit.expiry || null,
      ip_rules: rules, mountpoints: edit.allMounts ? [] : edit.mountpoints,
    };
    if (edit.password) body.password = edit.password;
    try {
      await api(`/api/users/${encodeURIComponent(selected)}`, {
        method: 'PUT', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body) });
      await load(); await openUser(selected, offset);
    } catch (e) { setErr(e.message || t('failed')); }
    finally { setBusy(false); }
  };

  const chooseMount = (name, checked) => setEdit({ ...edit,
    mountpoints: checked
      ? [...new Set([...edit.mountpoints, name])]
      : edit.mountpoints.filter(x => x !== name) });

  return html`<div class="auto-grid">
    <div class="card" style=${{ gridColumn: '1 / -1' }}>
      <div class="card-title"><h2>${t("NTRIP users")}</h2>
        <div><button class="btn act" onClick=${()=>setCreateOpen(true)}>${t("+ Add user")}</button>${' '}
          <button class="btn act" onClick=${load}>${t("Refresh")}</button></div></div>
      <div class="tbl-scroll"><table>
        <thead><tr><th>${t("Username")}</th><th>${t("Status")}</th><th>${t("Active")}</th><th>${t("Limit")}</th>
          <th>${t("Last connection")}</th><th>${t("Sent")}</th><th>${t("Expires")}</th>
          ${show ? html`<th>${t("Password")}</th>` : null}<th></th></tr></thead>
        <tbody>${users.map(u => { const state = userState(u); return html`
        <tr key=${u.username} class=${selected === u.username ? 'selected-row' : ''}>
          <td><button class="user-link mono" onClick=${() => openUser(u.username, 0)}>${u.username}</button></td>
          <td><span class=${'pill ' + state[1]}>${state[0]}</span></td>
          <td>${u.active}</td><td>${u.limit}</td>
          <td class="mono">${u.last_connection ? fmtDateTime(u.last_connection) : t('Never')}</td>
          <td>${fmtBytes(u.total_bytes_sent)}</td>
          <td>${u.expires_at ? expiryDay(u.expires_at) : t('Never')}</td>
          ${show ? html`<td class="mono">${u.password || '—'}</td>` : null}
          <td style=${{ textAlign: 'right', whiteSpace: 'nowrap' }}>
            <button class="btn act" onClick=${() => toggle(u)}>${u.enabled ? t('Disable') : t('Enable')}</button>
            ${' '}<button class="btn act danger" onClick=${() => del(u)}>${t("Delete")}</button></td>
        </tr>`; })}</tbody></table></div>
      <p style=${{ marginBottom: 0 }}>
        <button class="btn act" onClick=${() => setShow(!show)}>
          ${show ? t('Hide passwords') : t('Show passwords')}</button>
        <span class="muted" style=${{ marginLeft: '10px', fontSize: '12px' }}>
          ${t("NTRIP passwords are stored encrypted, not hashed, so they can be read back to configure field equipment.")}</span></p>
    </div>

    ${createOpen ? html`<div class="modal-backdrop" onMouseDown=${e=>{if(e.target===e.currentTarget)setCreateOpen(false)}}>
      <div class="card modal-card" role="dialog" aria-modal="true" aria-labelledby="add-user-title">
      <div class="card-title"><h2 id="add-user-title">${t("Add NTRIP user")}</h2><button class="btn act" type="button" onClick=${()=>setCreateOpen(false)}>${t("Close")}</button></div>
      <form onSubmit=${create}>
        <div class="control-row">
          <div><label>${t("Username")}</label><input class="form-control" value=${form.username} required
            onChange=${e => setForm({ ...form, username: e.target.value })}/></div>
          <div><label>${t("Password")}</label><input class="form-control" value=${form.password} required
            type="password" autoComplete="new-password"
            onChange=${e => setForm({ ...form, password: e.target.value })}/></div>
          <div style=${{ maxWidth: '110px' }}><label>${t("Conn. limit")}</label>
            <input class="form-control" type="number" min="1" value=${form.limit}
            onChange=${e => setForm({ ...form, limit: e.target.value })}/></div>
        </div>
        <p><label>${t("Notes")}</label><input class="form-control" value=${form.note}
          onChange=${e => setForm({ ...form, note: e.target.value })}/></p>
        <div class="form-actions"><button class="btn act" type="submit">${t("Create user")}</button>
          <button class="btn act" type="button" onClick=${()=>setCreateOpen(false)}>${t("Cancel")}</button></div>
      </form>
    </div></div>` : null}

    ${err ? html`<div class="card error-card"><div class="err">${err}</div></div>` : null}

    ${selected && edit && detail ? html`
    <div class="users-layout">
      <div class="card">
        <div class="card-title"><h2>${t('Edit {name}',{name:selected})}</h2>
          <span class=${'pill ' + userState(detail.user)[1]}>${userState(detail.user)[0]}</span></div>
        <form class="user-form" onSubmit=${save}>
          <div class="field-row two">
            <div class="field"><label>${t("Connection limit")}</label><input class="form-control" type="number" min="1" required
              value=${edit.limit} onChange=${e => setEdit({ ...edit, limit: e.target.value })}/></div>
            <div class="field check-field"><label>${t("Account")}</label><label class="check">
              <input class="form-check-input" type="checkbox" checked=${edit.enabled}
                onChange=${e => setEdit({ ...edit, enabled: e.target.checked })}/> ${t("Enabled")}</label></div>
          </div>
          <div class="field"><label>${t("New password")}</label><input class="form-control" type="password" autoComplete="new-password"
            placeholder=${t("Leave blank to keep current password")} value=${edit.password}
            onChange=${e => setEdit({ ...edit, password: e.target.value })}/></div>
          <div class="field"><label>${t("Email")}</label><input class="form-control" type="email" value=${edit.email}
            onChange=${e => setEdit({ ...edit, email: e.target.value })}/></div>
          <div class="field"><label>${t("Expiry date (UTC)")}</label><input class="form-control" type="date" value=${edit.expiry}
            onChange=${e => setEdit({ ...edit, expiry: e.target.value })}/>
            <div class="field-help">${t("Blank means the account never expires. The selected date remains valid all day.")}</div></div>
          <div class="field"><label>${t("Notes")}</label><textarea rows="3" value=${edit.note}
            onChange=${e => setEdit({ ...edit, note: e.target.value })}></textarea></div>
          <div class="field"><label>${t("Allowed client IPs / CIDRs")}</label><textarea rows="4"
            placeholder=${t("Blank allows every address\n192.0.2.14\n2001:db8::/48")} value=${edit.ipRules}
            onChange=${e => setEdit({ ...edit, ipRules: e.target.value })}></textarea>
            <div class="field-help">${t("One address or CIDR per line. Rules use the client address visible to the caster.")}</div></div>
          <div class="field"><label>${t("Mountpoint access")}</label><label class="check">
            <input class="form-check-input" type="checkbox" checked=${edit.allMounts}
              onChange=${e => setEdit({ ...edit, allMounts: e.target.checked })}/> ${t("All mountpoints")}</label>
            <div class="mount-checks">${mounts.map(name => html`<label class="check" key=${name}>
              <input class="form-check-input" type="checkbox" disabled=${edit.allMounts}
                checked=${edit.allMounts || edit.mountpoints.includes(name)}
                onChange=${e => chooseMount(name, e.target.checked)}/>${name}</label>`)}</div>
            ${!edit.allMounts && edit.mountpoints.length === 0
              ? html`<div class="field-help bad-text">${t("Select at least one mountpoint, or choose All mountpoints.")}</div>` : null}
          </div>
          <div class="form-actions"><button class="primary go" type="submit"
            disabled=${busy || (!edit.allMounts && edit.mountpoints.length === 0)}>
            ${busy ? t('Saving…') : t('Save changes')}</button>
            <button class="btn act" type="button" onClick=${() => openUser(selected, offset)}>${t("Discard")}</button></div>
        </form>
      </div>

      <div class="user-detail">
        <div class="card">
          <h2>${t("Account summary")}</h2>
          <div class="kpis user-kpis">
            ${kpi(t('Connections'), detail.stats.connection_count, t('{n} active',{n:detail.stats.active}))}
            ${kpi(t('Data sent'), fmtBytes(detail.stats.total_bytes_sent), t('{v} received',{v:fmtBytes(detail.stats.total_bytes_received)}))}
            ${kpi(t('Connected time'), fmtDur(detail.stats.total_duration_s), t('across all sessions'))}
            ${kpi(t('Last connection'), detail.stats.last_connection ? fmtDateTime(detail.stats.last_connection) : t('Never'),
              detail.stats.first_connection ? t('first {d}',{d:fmtDateTime(detail.stats.first_connection)}) : t('no history'))}
          </div>
          <dl class="kv user-meta">
            <dt>${t("Email")}</dt><dd>${detail.user.email || '—'}</dd>
            <dt>${t("Notes")}</dt><dd>${detail.user.note || '—'}</dd>
            <dt>${t("IPs seen")}</dt><dd class="mono">${(detail.stats.ips_seen || []).join(', ') || t('None')}</dd>
            <dt>${t("IP access")}</dt><dd class="mono">${detail.access.all_ips ? t('All addresses') : detail.access.ip_rules.join(', ')}</dd>
            <dt>${t("Mountpoints")}</dt><dd class="mono">${detail.access.all_mountpoints ? t('All mountpoints') : detail.access.mountpoints.join(', ')}</dd>
          </dl>
        </div>
        <div class="card">
          <div class="card-title"><h2>${t("Connection history")}</h2>
            <span class="muted">${t('{n} total',{n:detail.page.total})}</span></div>
          ${detail.history.length === 0 ? html`<p class="muted">${t("No connections recorded for this account.")}</p>` : html`
          <div class="tbl-scroll"><table>
            <thead><tr><th>${t("Started")}</th><th>${t("Mountpoint")}</th><th>${t("Client IP")}</th>
              <th>${t("Duration")}</th><th>${t("Sent")}</th><th>${t("Agent")}</th></tr></thead>
            <tbody>${detail.history.map(c => html`<tr key=${c.id} class=${c.active ? 'active-row' : ''}>
              <td class="mono">${fmtDateTime(c.started)}</td><td class="mono">${c.mountpoint}</td>
              <td class="mono">${c.client_ip}</td><td>${fmtDur(c.duration_s)}
                ${c.active ? html` <span class="pill ok">${t("live")}</span>` : null}</td>
              <td>${fmtBytes(c.bytes_sent)}</td><td class="muted">${c.agent || '—'}</td>
            </tr>`)}</tbody></table></div>`}
          <div class="history-pager">
            <button class="btn act" disabled=${offset === 0 || busy}
              onClick=${() => openUser(selected, Math.max(0, offset - pageSize))}>${t("Previous")}</button>
            <span class="muted">${detail.page.total ? `${offset + 1}–${Math.min(offset + pageSize, detail.page.total)}` : '0'} ${t('of')} ${detail.page.total}</span>
            <button class="btn act" disabled=${offset + pageSize >= detail.page.total || busy}
              onClick=${() => openUser(selected, offset + pageSize)}>${t("Next")}</button>
          </div>
        </div>
      </div>
    </div>` : selected && busy ? html`<div class="card"><p class="muted">${t("Loading user…")}</p></div>` : null}
  </div>`;
}

/* ----------------------------------------------------------- connections */

function Connections() {
  const [rows, setRows] = useState([]);
  useEffect(() => {
    const go = () => api('/api/connections?limit=200').then(setRows).catch(() => {});
    go(); const t = setInterval(go, 10000); return () => clearInterval(t);
  }, []);
  return html`<div class="card">
    <h2>${t("Connection history")}</h2>
    ${rows.length === 0 ? html`<p class="muted">${t("No connections recorded yet.")}</p>` : html`
    <div class="tbl-scroll"><table>
      <thead><tr><th>${t("User")}</th><th>${t("Mountpoint")}</th><th>${t("Client IP")}</th><th>${t("Via proxy")}</th>
        <th>${t("Started")}</th><th>${t("Duration")}</th><th>${t("Sent")}</th><th>${t("Agent")}</th></tr></thead>
      <tbody>${rows.map((c, i) => html`<tr key=${i}>
        <td class="mono">${c.username}</td><td class="mono">${c.mountpoint}</td>
        <td class="mono">${c.client_ip}</td>
        <td>${c.via_proxy ? html`<span class="pill ok">${t("yes")}</span>` : html`<span class="muted">—</span>`}</td>
        <td class="mono">${fmtDateTime(c.started)}</td>
        <td>${fmtDur(c.duration_s)}${c.active ? html` <span class="pill ok">${t("live")}</span>` : null}</td>
        <td>${fmtBytes(c.bytes)}</td><td class="muted">${c.agent || '—'}</td></tr>`)}
      </tbody></table></div>`}
  </div>`;
}

/* Connection history is a record of these same accounts using the caster, so
   it is a sub-page of Users rather than a top-level tab. The sub-nav is the
   one Settings already uses. */
function UsersPage() {
  const [view, setView] = useState('accounts');
  return html`<div class="users-page">
    <div class="subnav">
      <button class=${view === 'accounts' ? 'on' : ''} onClick=${() => setView('accounts')}>${t("Accounts")}</button>
      <button class=${view === 'connections' ? 'on' : ''} onClick=${() => setView('connections')}>${t("Connection history")}</button>
    </div>
    ${view === 'accounts' ? html`<${Users}/>` : html`<${Connections}/>`}
  </div>`;
}

/* --------------------------------------------------------------- archive */

/* ---------------------------------------------------- File download */

const DOW = ['Mon','Tue','Wed','Thu','Fri','Sat','Sun'];
const MONTHS = ['January','February','March','April','May','June','July',
                'August','September','October','November','December'];
const LOCAL_TZ = 'Europe/Bratislava';

const zoneTime = (d, tz) => new Intl.DateTimeFormat('en-GB', {
  timeZone: tz, hour: '2-digit', minute: '2-digit', second: '2-digit', hour12: false,
}).format(d);

/* Offset of the configured local zone from UTC in minutes, for a given date,
   so CET and CEST are both handled without a lookup table. */
function localOffsetMin(dateStr) {
  const probe = new Date((dateStr || new Date().toISOString().slice(0, 10)) + 'T12:00:00Z');
  const asLocal = new Date(probe.toLocaleString('en-US', { timeZone: LOCAL_TZ }));
  const asUTC = new Date(probe.toLocaleString('en-US', { timeZone: 'UTC' }));
  return Math.round((asLocal - asUTC) / 60000);
}
const tzAbbr = d => new Intl.DateTimeFormat('en-GB', {
  timeZone: LOCAL_TZ, timeZoneName: 'short' }).formatToParts(d)
  .find(p => p.type === 'timeZoneName')?.value || 'CET';

const pad2 = n => String(n).padStart(2, '0');
const hhmm = min => {
  const m = ((min % 1440) + 1440) % 1440;
  return `${pad2(Math.floor(m / 60))}:${pad2(m % 60)}`;
};
const toMin = v => { const [h, m] = (v || '00:00').split(':').map(Number); return h * 60 + (m || 0); };

/* A strictly 24-hour time field.
   <input class="form-control" type="time"> renders AM/PM wherever the browser locale says so, and
   no attribute overrides that, so the control is built from two selects. */
function TimeField({ value, onChange }) {
  const [h, mi] = (value || '00:00').split(':');
  const set = (hh, mm) => onChange(`${hh}:${mm}`);
  const opts = (n, step) => Array.from({ length: Math.ceil(n / step) },
    (_, i) => pad2(i * step));
  return html`<div class="tfield">
    <select class="form-select" value=${h} onChange=${e => set(e.target.value, mi)}>
      ${opts(24, 1).map(v => html`<option key=${v} value=${v}>${v}</option>`)}
    </select><span>:</span><select class="form-select" value=${mi} onChange=${e => set(h, e.target.value)}>
      ${opts(60, 5).map(v => html`<option key=${v} value=${v}>${v}</option>`)}
    </select></div>`;
}

/* Status strip: the clocks, laid out as aligned readouts rather than cards. */
function TimeStrip() {
  const [, tick] = useState(0);
  useEffect(() => { const t = setInterval(() => tick(n => n + 1), 1000); return () => clearInterval(t); }, []);
  const now = new Date();
  const cell = (label, value, mono = true) => html`<div class="rd" key=${label}>
    <div class="rd-l">${label}</div><div class=${'rd-v' + (mono ? ' mono' : '')}>${value}</div></div>`;
  return html`<div class="strip">
    ${cell(tzAbbr(now), zoneTime(now, LOCAL_TZ))}
    ${cell('UTC', zoneTime(now, 'UTC'))}
  </div>`;
}

/* Live capture readouts, one row per archive. */
function LiveCapture({ live }) {
  const arcs = (live && live.archives) || [];
  const prev = useRef({});
  const [rates, setRates] = useState({});
  useEffect(() => {
    const now = Date.now(); const next = {};
    arcs.forEach(a => {
      const p = prev.current[a.name];
      if (p && now > p.t) next[a.name] = (a.bytes - p.b) / ((now - p.t) / 1000);
      prev.current[a.name] = { b: a.bytes, t: now };
    });
    if (Object.keys(next).length) setRates(r => ({ ...r, ...next }));
  }, [live && live.time]);
  if (!arcs.length) return null;
  const now = Date.now() / 1000;
  return html`<section class="panel">
    <header class="panel-h"><h2>${t("Capture in progress")}</h2></header>
    <div class="panel-b">
      <table class="table table-vcenter grid-t"><tbody>
        ${arcs.map(a => {
          const span = (a.swap_at || 0) - (a.start || 0);
          const through = span > 0 ? Math.min(1, Math.max(0, (now - a.start) / span)) : 0;
          const rate = rates[a.name];
          return html`<tr key=${a.name}>
            <th>${a.name.toUpperCase()}</th>
            <td class="mono num">${fmtBytes(a.bytes)}</td>
            <td class="mono num dim">${rate != null && rate >= 0 ? fmtBytes(rate) + '/s' : '—'}</td>
            <td class="bar-cell"><div class="bar"><i style=${{ width: (through * 100).toFixed(1) + '%' }}></i></div></td>
            <td class="mono num dim">${(through * 100).toFixed(0)}%</td>
          </tr>`;
        })}
      </tbody></table>
    </div>
  </section>`;
}

function FileDownload({ live }) {
  const [days, setDays] = useState([]);
  const [month, setMonth] = useState(null);
  const [sel, setSel] = useState(null);
  const [endSel, setEndSel] = useState(null);
  const [job, setJob] = useState(null);
  const [err, setErr] = useState('');
  const [zone, setZone] = useState('local');
  const [from, setFrom] = useState('09:00');
  const [to, setTo] = useState('11:00');
  const [outputs, setOutputs] = useState('both');
  const [preset, setPreset] = useState('station'), [presets, setPresets] = useState([]);

  useEffect(() => {
    api('/api/downloader/available').then(r => {
      setDays(r.days || []);
      setPresets(r.presets || []);
      if (r.error) setErr(r.error);
      const withData = (r.days || []).filter(d => !d.capturing);
      const last = withData.length ? withData[withData.length - 1].date : null;
      const base = last ? new Date(last + 'T00:00:00') : new Date();
      setMonth(new Date(base.getFullYear(), base.getMonth(), 1));
      if (last) { setSel(last); setEndSel(last); }
    }).catch(e => setErr(e.message || t('Could not load available data')));
  }, []);

  useEffect(() => {
    api('/api/downloader/jobs').then(js => {
      const l = (js || []).find(j => j.state === 'running')
             || (js || []).find(j => j.state === 'done' && j.token);
      if (l) { setJob(l); setSel(l.date); setEndSel(l.date); }
    }).catch(() => {});
  }, []);

  useEffect(() => {
    if (!job || job.state !== 'running') return;
    const t = setInterval(() => {
      api('/api/downloader/job/' + job.id).then(setJob).catch(() => clearInterval(t));
    }, 1000);
    return () => clearInterval(t);
  }, [job && job.id, job && job.state]);

  const byDate = {}; days.forEach(d => { byDate[d.date] = d; });
  const firstMonth = days.filter(d => !d.capturing).map(d => d.date.slice(0, 7))[0];
  const leap = live && live.gps_time && live.gps_time.valid ? live.gps_time.leap_secs : 18;
  const gpsOffMin = Math.round(leap / 60);

  const utcInstant = (date, clock) => {
    let minutes = Date.parse(date + 'T00:00:00Z') / 60000 + toMin(clock);
    if (zone === 'local') minutes -= localOffsetMin(date);
    else if (zone === 'gps') minutes -= gpsOffMin;
    return new Date(minutes * 60000);
  };
  const startAt = sel ? utcInstant(sel, from) : null;
  const endAt = endSel ? utcInstant(endSel, to) : null;
  const durMin = startAt && endAt ? Math.round((endAt - startAt) / 60000) : 0;
  const rangeBad = !startAt || !endAt || durMin <= 0 || durMin > 31 * 1440;
  const selectedDates = days.filter(d => sel && endSel && d.date >= sel && d.date <= endSel);
  const missingDays = sel && endSel ? Math.max(0,
    Math.round((Date.parse(endSel+'T00:00:00Z')-Date.parse(sel+'T00:00:00Z'))/86400000)+1-selectedDates.length) : 0;

  const run = async () => {
    if (!sel || rangeBad) return;
    setErr('');
    try {
      setJob(await api('/api/downloader/process', {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ outputs, preset, start: startAt.toISOString(), end: endAt.toISOString() }) }));
    } catch (e) { setErr(e.message || t('Conversion failed')); }
  };
  const cancel = async () => {
    if (!job) return;
    try { await api('/api/downloader/job/' + job.id + '/cancel', { method: 'POST' }); }
    catch (e) { setErr(e.message || t('Could not cancel')); }
  };

  if (!month) return html`<section class="panel"><header class="panel-h"><h2>${t("File download")}</h2></header>
    <div class="panel-b"><p class="note">${t("Loading…")}</p></div></section>`;

  const y = month.getFullYear(), m = month.getMonth();
  const lead = (new Date(y, m, 1).getDay() + 6) % 7;
  const nDays = new Date(y, m + 1, 0).getDate();
  const cells = [];
  for (let i = 0; i < lead; i++) cells.push(null);
  for (let d = 1; d <= nDays; d++) cells.push(d);
  const key = d => `${y}-${pad2(m + 1)}-${pad2(d)}`;
  const ym = `${y}-${pad2(m + 1)}`;
  const today = new Date();
  const prevOff = firstMonth && ym <= firstMonth;
  const nextOff = y > today.getFullYear() || (y === today.getFullYear() && m >= today.getMonth());

  const running = job && job.state === 'running';
  const pct = job && job.percent != null ? Math.round(job.percent * 100) : 0;
  const noNav = selectedDates.some(d => !d.has_nav);
  const outOpts = [['obs', 'Observations'], ['nav', 'Navigation'], ['both', 'Both']];
  if (noNav && outputs !== 'obs') setTimeout(() => setOutputs('obs'), 0);

  return html`<div class="fd">
    <${TimeStrip}/>
    <${LiveCapture} live=${live}/>

    <div class="fd-cols">
      <section class="panel">
        <header class="panel-h">
          <h2>${t("Date")}</h2>
          <div class="pager">
            <button disabled=${!!prevOff} onClick=${() => setMonth(new Date(y, m - 1, 1))}>‹</button>
            <span>${t(MONTHS[m])} ${y}</span>
            <button disabled=${!!nextOff} onClick=${() => setMonth(new Date(y, m + 1, 1))}>›</button>
          </div>
        </header>
        <div class="panel-b">
          <div class="cal">
            ${DOW.map(d => html`<div class="cal-h" key=${d}>${t(d).slice(0, 2)}</div>`)}
            ${cells.map((d, i) => {
              if (d === null) return html`<div class="cal-d empty-state" key=${'e' + i}></div>`;
              const k = key(d), info = byDate[k];
              let cls = 'cal-d';
              if (info && info.capturing) cls += ' cap';
              else if (info && !info.has_nav) cls += ' avail obs';
              else if (info) cls += ' avail';
              if (sel && endSel && k > sel && k < endSel) cls += ' range';
              if (sel === k || endSel === k) cls += ' sel';
              return html`<button class=${cls} key=${k} disabled=${!info}
                onClick=${() => { if (running) return; if (sel && endSel === sel && k >= sel) setEndSel(k);
                  else { setSel(k); setEndSel(k); } setJob(null); }}>${d}</button>`;
            })}
          </div>
          <dl class="legend-swatch">
            <dt class="k-av"></dt><dd>${t("Available")}</dd>
            <dt class="k-obs"></dt><dd>${t("No ephemeris")}</dd>
            <dt class="k-cap"></dt><dd>${t("Recording now")}</dd>
            <dt class="k-no"></dt><dd>${t("No data")}</dd>
          </dl>
          <p class="note">${t("Select a start day, then an end day. A range may span up to 31 days.")}</p>
        </div>
      </section>

      <section class="panel">
        <header class="panel-h"><h2>${t("Extract")}</h2></header>
        <div class="panel-b">
          <dl class="kv">
            <dt>${t("Range")}</dt><dd class="mono">${sel || '—'} → ${endSel || '—'}</dd>
            <dt>${t("Duration")}</dt><dd class="mono">${durMin > 0 ? Math.floor(durMin/1440)+'d '+hhmm(durMin) : '—'}</dd>
          </dl>

            <div class="field-row two">
              <div class="field"><label>${t("Start date")}</label><input class="form-control" type="date" value=${sel||''}
                min=${days.length?days[0].date:''} max=${endSel||''} onChange=${e=>setSel(e.target.value)}/></div>
              <div class="field"><label>${t("End date")}</label><input class="form-control" type="date" value=${endSel||''}
                min=${sel||''} max=${days.length?days[days.length-1].date:''} onChange=${e=>setEndSel(e.target.value)}/></div>
            </div>

            <div class="field-row">
              <div class="field"><label>${t("Time base")}</label>
                <select class="form-select" value=${zone} onChange=${e => setZone(e.target.value)}>
                  <option value="local">${tzAbbr(today)}</option>
                  <option value="utc">UTC</option>
                  <option value="gps">GPS</option>
                </select></div>
              <div class="field"><label>${t("From")}</label>
                <${TimeField} value=${from} onChange=${setFrom}/></div>
              <div class="field"><label>${t("To")}</label>
                <${TimeField} value=${to} onChange=${setTo}/></div>
            </div>
            <div class="quick">
              <span>${t("Set duration")}</span>
              ${[['30 min', 30], ['1 h', 60], ['2 h', 120], ['4 h', 240], ['6 h', 360], ['12 h', 720]]
                .map(([lab, mins]) => html`<button key=${lab}
                  onClick=${() => { const e = toMin(from) + mins;
                    setTo(hhmm(e)); if(sel) setEndSel(e>=1440?new Date(Date.parse(sel+'T00:00:00Z')+86400000).toISOString().slice(0,10):sel); }}>${lab}</button>`)}
            </div>
            ${rangeBad ? html`<p class="alert-text">
                ${t("End must be after start and the range may not exceed 31 days.")}</p>` : html`
              <div class="tz-row">
                ${[[tzAbbr(today), zoneTime(startAt,LOCAL_TZ).slice(0,5), zoneTime(endAt,LOCAL_TZ).slice(0,5)], ['UTC', zoneTime(startAt,'UTC').slice(0,5), zoneTime(endAt,'UTC').slice(0,5)],
                   ['GPS', hhmm(startAt.getUTCHours()*60+startAt.getUTCMinutes()+gpsOffMin),hhmm(endAt.getUTCHours()*60+endAt.getUTCMinutes()+gpsOffMin)]]
                  .map(([lab, a, b]) => html`<div key=${lab}>
                    <span class="tz-l">${lab}</span>
                    <span class="tz-v mono">${a}–${b}</span></div>`)}
              </div>`}

          ${missingDays ? html`<p class="alert-text">${t('{n} day(s) in this range have no archive data.',{n:missingDays})}</p>` : null}

          <div class="field">
            <label>${t("Output")}</label>
            <div class="seg">
              ${outOpts.map(([k, lab]) => html`<button key=${k}
                class=${outputs === k ? 'on' : ''}
                disabled=${k === 'nav' && noNav}
                title=${k === 'nav' && noNav ? t('No ephemeris recorded for this day') : ''}
                onClick=${() => setOutputs(k)}>${t(lab)}</button>`)}
            </div>
          </div>
          <div class="field">
            <label>${t("RINEX preset")}</label>
            <select class="form-select" value=${preset} onChange=${e=>setPreset(e.target.value)}>
              ${presets.map(p=>html`<option key=${p.id} value=${p.id}>${t(p.name)}</option>`)}
            </select>
            ${presets.filter(p=>p.id===preset).map(p=>html`<p class="note" key=${p.id}>${t(p.note)}</p>`)}
          </div>
          ${noNav ? html`<p class="note">${t("At least one selected day holds RTCM only; navigation data is unavailable for the complete range.")}</p>` : null}
          ${endSel && byDate[endSel] && byDate[endSel].capturing ? html`<p class="note">
            ${t("The final day is still recording. Anything after the current time is not available yet and the range is trimmed to now.")}</p>` : null}

          <div class="submit">
            ${job && job.state === 'done' ? html`
              <a class="primary go" href=${'/api/downloader/download/' + job.token}>
                ${t("↓ Download")}</a>
              <div class="submit-info">
                <div class="submit-line"><span class="mono">${job.zip_name}</span>
                  <span class="dim mono">${fmtMB(job.size_mb)}</span></div>
                <div class="bar done"><i style=${{ width: '100%' }}></i></div>
                <div class="submit-foot"><span>${job.date} · ${job.window}</span>
                  <span class="mono">${job.elapsed}</span>
                  <button class="link-btn" onClick=${() => setJob(null)}>${t("Clear")}</button></div>
              </div>`
            : html`
              <button class="primary go" disabled=${!sel || running || rangeBad || !!missingDays} onClick=${run}>
                ${running ? t('Converting') : t('Convert')}</button>
              ${running ? html`<div class="submit-info">
                  <div class="bar lg"><i style=${{ width: pct + '%' }}></i></div>
                  <div class="submit-foot"><span class="mono">${pct}%</span>
                    <span>${job.stage}</span>
                    <span class="mono">${job.elapsed}</span>
                    <button class="link-btn" onClick=${cancel}>${t("Cancel")}</button></div>
                </div>`
                : html`<span class="est">${sel && !rangeBad ? t('Final ZIP size is shown after conversion') : ''}</span>`}`}
          </div>

          ${job && job.state === 'cancelled' ? html`<div class="result cancelled">
            <div class="result-h"><span>${t("Cancelled")}</span>
              <button class="link-btn" onClick=${() => setJob(null)}>${t("Clear")}</button></div>
          </div>` : null}
          ${job && job.state === 'error' ? html`<div class="result error">
            <div class="result-h"><span>${t("Conversion failed")}</span>
              <button class="link-btn" onClick=${() => setJob(null)}>${t("Clear")}</button></div>
            <p class="alert-text" style=${{ margin: 0 }}>${job.error}</p>
          </div>` : null}
          ${err ? html`<p class="alert-text">${err}</p>` : null}
        </div>
      </section>
    </div>
  </div>`;
}

/* --------------------------------------------------------------- history */

/* Visibility over the window: one row per satellite, one column per stored
   epoch, colour is aggregate C/N0. Hundreds of rows times thousands of columns
   is a lot of rectangles for SVG or the DOM, so this one is a canvas, drawn
   once per data change and hit-tested arithmetically on hover. */
const SNR_FLOOR = 15, SNR_CEILING = 55;
const mixColour = (from, to, t) => {
  const parse = c => {
    const h=c.replace('#','');
    const full=h.length===3?h.split('').map(x=>x+x).join(''):h.slice(0,6);
    const n=parseInt(full,16);
    return [(n>>16)&255,(n>>8)&255,n&255];
  };
  const a=parse(from), b=parse(to);
  return `rgb(${a.map((v,i)=>Math.round(v+(b[i]-v)*t)).join(',')})`;
};
const snrColour = (cno, cold, mid, hot) => {
  const t=Math.max(0,Math.min(1,(cno-SNR_FLOOR)/(SNR_CEILING-SNR_FLOOR)));
  return t<.6 ? mixColour(cold,mid,t/.6) : mixColour(mid,hot,(t-.6)/.4);
};

function VisibilityHeatmap({ epochs, keys }) {
  const host=useRef(null), canvas=useRef(null);
  const { w, h }=useSize(host);
  const theme=useThemeEpoch();
  const [tip,setTip]=useState(null);
  const padL=42, padB=20, padT=4;
  const cellW=keys.length&&epochs.length?Math.max(1,(w-padL)/epochs.length):0;
  const cellH=keys.length?Math.max(1,(h-padB-padT)/keys.length):0;
  useEffect(()=>{
    const el=canvas.current;
    if(!el||!w||!h||!keys.length||!epochs.length)return;
    const dpr=window.devicePixelRatio||1;
    el.width=Math.round(w*dpr); el.height=Math.round(h*dpr);
    const g=el.getContext('2d');
    g.setTransform(dpr,0,0,dpr,0,0);
    g.clearRect(0,0,w,h);
    const cold=token('--panel2'), mid=token('--accent'), hot=token('--sbas');
    const index=new Map(keys.map((k,i)=>[k,i]));
    epochs.forEach((e,col) => {
      (e.satellites||[]).forEach(s => {
        if(!(s.cno>0))return;
        const row=index.get(plotKey(s));
        if(row===undefined)return;
        g.fillStyle=snrColour(s.cno,cold,mid,hot);
        g.fillRect(padL+col*cellW,padT+row*cellH,Math.max(1,cellW),Math.max(1,cellH));
      });
    });
    g.fillStyle=token('--dim');
    g.font=token('--fs-1')+' '+token('--mono');
    g.textBaseline='middle';
    const step=Math.max(1,Math.ceil(13/cellH));
    keys.forEach((k,row) => {
      if(row%step)return;
      const [sys,sv]=k.split(':');
      g.fillText(systemStyle(sys).short+sv,4,padT+row*cellH+cellH/2);
    });
    g.textBaseline='alphabetic';
    const first=epochs[0], last=epochs[epochs.length-1];
    if(first&&last){
      g.fillText(fmtDateTime(first.t*1000),padL,h-6);
      g.textAlign='right';
      g.fillText(fmtDateTime(last.t*1000),w-2,h-6);
      g.textAlign='left';
    }
  },[epochs,keys,w,h,theme,cellW,cellH]);
  const probe = e => {
    if(!cellW||!cellH)return;
    const box=canvas.current.getBoundingClientRect();
    const col=Math.floor((e.clientX-box.left-padL)/cellW), row=Math.floor((e.clientY-box.top-padT)/cellH);
    const epoch=epochs[col], key=keys[row];
    if(!epoch||!key){setTip(null);return;}
    const sat=(epoch.satellites||[]).find(s=>plotKey(s)===key);
    const [sys,sv]=key.split(':');
    setTip({x:e.clientX-box.left,y:e.clientY-box.top,
      label:systemStyle(sys).short+sv+' · '+sys,
      when:fmtDateTime(epoch.t*1000),
      value:sat&&sat.cno>0?Math.round(sat.cno)+' dB-Hz':t('not observed')});
  };
  return html`<div class="heatmap-host" ref=${host}>
    <canvas ref=${canvas} style=${{width:'100%',height:'100%'}}
      onMouseMove=${probe} onMouseLeave=${()=>setTip(null)}></canvas>
    ${tip?html`<div class="heatmap-tip mono" style=${{left:tip.x+'px',top:tip.y+'px'}}>
      <b>${tip.label}</b><span>${tip.when}</span><span>${tip.value}</span></div>`:null}
  </div>`;
}

/* The colour ramp has no meaning without its scale. */
function SNRLegend() {
  const theme=useThemeEpoch();
  const stops=[0,.25,.5,.75,1].map(t => snrColour(SNR_FLOOR+t*(SNR_CEILING-SNR_FLOOR),
    token('--panel2'),token('--accent'),token('--sbas')));
  return html`<div class="heatmap-legend mono" key=${theme}>
    <span>${SNR_FLOOR} dB-Hz</span>
    <i style=${{background:`linear-gradient(90deg,${stops.join(',')})`}}></i>
    <span>${SNR_CEILING}+</span></div>`;
}

function HistoryCharts({ epochs, health }) {
  const visibilityKeys=[...new Set(epochs.flatMap(e=>(e.satellites||[]).filter(s=>s.cno>0&&PLOT_SYSTEMS[s.system]).map(s=>plotKey(s))))]
    .sort((a,b)=>{const [as,av]=a.split(':'),[bs,bv]=b.split(':');return Object.keys(PLOT_SYSTEMS).indexOf(as)-Object.keys(PLOT_SYSTEMS).indexOf(bs)||Number(av)-Number(bv)});
  const times=epochs.map(e=>e.t);
  const tracked=epochs.map(e=>new Set((e.satellites||[]).filter(s=>s.cno>0&&PLOT_SYSTEMS[s.system]).map(plotKey)).size);
  const used=epochs.map(e=>new Set((e.satellites||[]).filter(s=>s.cno>0&&s.used&&PLOT_SYSTEMS[s.system]).map(plotKey)).size);
  const ht=health.map(h=>h.t);
  const countData=[times,tracked,used];
  const streamData=[ht,health.map(h=>(h.rtcm_bps||0)/1000),health.map(h=>(h.ubx_bps||0)/1000),health.map(h=>h.clients||0)];
  const countOpts={shape:'count',build:()=>({
    ...chartBase(),
    series:[{label:t('Time')},
      {label:t('Tracked'),stroke:token('--accent'),width:2,fill:rgba(token('--accent'),.12),points:{show:false}},
      {label:t('Used in navigation'),stroke:token('--ok'),width:1.5,dash:[5,4],points:{show:false}}],
    axes:[chartAxis({}),chartAxis({size:44})],
    scales:{y:{range:(u,min,max)=>[0,Math.max(5,Math.ceil((max||0)+2))]}},
  })};
  const streamOpts={shape:'stream',build:()=>({
    ...chartBase(),
    series:[{label:t('Time')},
      {label:t('RTCM kb/s'),stroke:token('--ok'),width:1.5,points:{show:false}},
      {label:t('UBX kb/s'),stroke:token('--accent'),width:1.5,points:{show:false}},
      {label:t('Clients'),stroke:token('--sbas'),width:1.5,scale:'c',points:{show:false}}],
    axes:[chartAxis({}),chartAxis({size:44}),chartAxis({scale:'c',side:1,size:38,grid:{show:false}})],
    scales:{y:{range:(u,min,max)=>[0,Math.max(1,(max||0)*1.15)]},c:{range:(u,min,max)=>[0,Math.max(2,(max||0)+1)]}},
  })};
  const hostSeries=[
    {label:t('CPU %'),key:'cpu',colour:'--accent',values:health.map(h=>h.cpu_pct||0)},
    {label:t('Temperature °C'),key:'temp',colour:'--bad',values:health.map(h=>h.temp_c||0)},
    {label:t('Memory available GB'),key:'mem',colour:'--ok',values:health.map(h=>(h.mem_free_mb||0)/1024)},
    {label:t('Disk free TB'),key:'disk',colour:'--sbas',values:health.map(h=>(h.disk_free_mb||0)/1048576)},
  ];
  return html`<div class="history-chart-grid">
    <div class="card history-chart wide history-count"><h2>${t("Tracked satellites over time")}</h2>
      <${UPlotChart} data=${countData} opts=${countOpts} height=${230}/></div>
    <div class="card history-chart wide"><h2>${t('Visibility and aggregate SNR · {n} satellites observed in window',{n:visibilityKeys.length})}</h2>
      <div class="visibility-history" style=${{height:Math.max(320,Math.min(760,visibilityKeys.length*17+105))+'px'}}>
        <${VisibilityHeatmap} epochs=${epochs} keys=${visibilityKeys}/>
      </div>
      <${SNRLegend}/></div>
    <div class="card history-chart"><h2>${t("Input throughput and clients")}</h2>
      <${UPlotChart} data=${streamData} opts=${streamOpts} height=${280}/></div>
    <div class="card history-chart host-history"><h2>${t("Host resources")}</h2>
      <div class="host-stack">
        ${hostSeries.map(s => html`<div class="host-track" key=${s.key}>
          <span class="host-track-label mono">${s.label}</span>
          <${UPlotChart} data=${[ht,s.values]} height=${96}
            opts=${{shape:s.key,build:()=>({
              ...chartBase(),
              legend:{show:false},
              series:[{},{label:s.label,stroke:token(s.colour),width:1.5,
                fill:rgba(token(s.colour),.1),points:{show:false}}],
              axes:[chartAxis({size:26}),chartAxis({size:44})],
            })}}/>
        </div>`)}
      </div></div>
  </div>`;
}

/* Data History arranges the same panels as the dashboard, and on the dashboard
   the one beside the skyplot is the station map. History has no map to show,
   so this panel reports what the replayed epoch actually contained -- otherwise
   that panel is simply blank. */
function EpochSummary({ epoch }) {
  if (!epoch) return null;
  const sats=(epoch.satellites||[]).filter(s=>s.cno>0);
  const signals=(epoch.signals||[]).filter(s=>s.cno>0);
  const used=sats.filter(s=>s.used).length;
  const masked=sats.filter(s=>s.elev>0&&s.elev<15).length;
  const cn=signals.map(s=>s.cno);
  const avg=cn.length?Math.round(cn.reduce((a,b)=>a+b,0)/cn.length):0;
  /* History satellites carry no per-signal map of their own: the epoch lists
     signals separately, so the best band per satellite is folded in here. */
  const bestBand={};
  signals.forEach(s=>{const k=plotKey(s);if(!(k in bestBand)||s.cno>bestBand[k])bestBand[k]=s.cno});
  const satBest=s=>Math.max(s.cno||0,bestBand[plotKey(s)]||0);
  const best=sats.reduce((b,s)=>!b||satBest(s)>satBest(b)?s:b,null);
  const bands={};
  signals.forEach(s=>{const b=bands[s.band]||(bands[s.band]={n:0,sum:0});b.n++;b.sum+=s.cno});
  const known=Object.keys(PLOT_SYSTEMS);
  const systems=[...known,...[...new Set(sats.map(s=>s.system))].filter(x=>!known.includes(x))];
  const rows=systems.map(system=>{
    const sig=signals.filter(s=>s.system===system).map(s=>s.cno);
    return {system,cfg:systemStyle(system),n:sats.filter(s=>s.system===system).length,
      avg:sig.length?Math.round(sig.reduce((a,b)=>a+b,0)/sig.length):0};
  });
  const cell=(label,value)=>html`<div class="rd" key=${label}><div class="rd-l">${label}</div><div class="rd-v mono">${value}</div></div>`;
  return html`<div class="card plot-card epoch-summary-card"><h2>${t("Epoch detail")}</h2>
    <div class="epoch-summary">
      <div class="epoch-readouts">
        ${cell(t('Tracked'),sats.length)}
        ${cell(t('Used in solution'),used)}
        ${cell(t('Average SNR'),avg?avg+' dB-Hz':'—')}
        ${cell(t('Below 15° mask'),masked)}
        ${cell(t('Signals'),signals.length)}
        ${cell(t('Strongest'),best?best.system+' '+systemStyle(best.system).short+best.sv+' · '+satBest(best)+' dB-Hz':'—')}
      </div>
      <table class="table table-vcenter epoch-table"><thead><tr><th>${t("Constellation")}</th><th>${t("Satellites")}</th><th>${t("Mean SNR")}</th></tr></thead>
        <tbody>${rows.map(r=>html`<tr key=${r.system} class=${r.n?'':'absent'}><td><i class="dot" style=${{background:r.cfg.color}}></i>${r.system}</td>
          <td class="mono">${r.n}</td><td class="mono">${r.avg?r.avg+' dB-Hz':'—'}</td></tr>`)}</tbody></table>
      <table class="table table-vcenter epoch-table"><thead><tr><th>${t("Band")}</th><th>${t("Signals")}</th><th>${t("Mean SNR")}</th></tr></thead>
        <tbody>${Object.keys(bands).sort().map(b=>html`<tr key=${b}><td class="mono">${b}</td><td class="mono">${bands[b].n}</td>
          <td class="mono">${Math.round(bands[b].sum/bands[b].n)} dB-Hz</td></tr>`)}</tbody></table>
    </div></div>`;
}

function History() {
  const [hours,setHours]=useState(6),[data,setData]=useState(null),[index,setIndex]=useState(0),[playing,setPlaying]=useState(false),[err,setErr]=useState(''),[loading,setLoading]=useState(false);
  const liveEdge=useRef(true);
  useEffect(()=>{
    let active=true,controller=null;
    const load=initial=>{if(document.hidden&&!initial)return;if(controller)controller.abort();controller=new AbortController();if(initial)setLoading(true);setErr('');api(`/api/history?hours=${hours}`,{signal:controller.signal}).then(d=>{if(!active)return;setData(d);setLoading(false);setIndex(i=>liveEdge.current?Math.max(0,(d.epochs||[]).length-1):Math.min(i,Math.max(0,(d.epochs||[]).length-1)))}).catch(e=>{if(active&&e.name!=='AbortError'){setLoading(false);setErr(e.message||t('Could not load history'))}})};
    liveEdge.current=true;load(true);const refresh=setInterval(()=>load(false),60000);return()=>{active=false;if(controller)controller.abort();clearInterval(refresh)};
  },[hours]);
  const pts=(data&&data.epochs)||[],health=(data&&data.health)||[];
  useEffect(()=>{if(!playing||pts.length<2)return;const t=setInterval(()=>setIndex(i=>i>=pts.length-1?0:i+1),350);return()=>clearInterval(t)},[playing,pts.length]);
  const epoch=pts[Math.min(index,Math.max(0,pts.length-1))];
  const fullHours=(data&&data.retention_days?data.retention_days:7)*24;
  const windows=[...new Set([1,6,24,72,168,fullHours].filter(h=>h<=fullHours))].sort((a,b)=>a-b);
  return html`<div class="history-page">
    <div class="card history-controls"><div class="card-title"><div><h2>${t("GNSS data history")}</h2></div>${epoch?html`<span class="pill ok">${fmtDateTime(epoch.t*1000)}</span>`:null}</div>
      <div class="history-control-grid"><div><label>${t("Retained window")}</label><select class="form-select" value=${hours} onChange=${e=>{setPlaying(false);setHours(Number(e.target.value))}}>${windows.map(h=>html`<option value=${h}>${h<24?t('{n} hours',{n:h}):t('{n} days',{n:h/24})}</option>`)}</select></div>
        <div class="history-play"><label>${t("Playback")}</label><button class="btn act" onClick=${()=>{if(!playing)liveEdge.current=false;setPlaying(!playing)}} disabled=${pts.length<2}>${playing?t('Pause'):t('Play')}</button></div></div>
      ${pts.length?html`<div class="scrubber"><input class="form-range" type="range" min="0" max=${Math.max(0,pts.length-1)} value=${Math.min(index,pts.length-1)} onInput=${e=>{liveEdge.current=false;setPlaying(false);setIndex(Number(e.target.value))}}/><div><span>${fmtDateTime(pts[0].t*1000)}</span><b>${t('{i} / {n} sampled · {e} stored epochs · auto-refresh 60 s',{i:index+1,n:pts.length,e:fmtNum((data&&data.epoch_count)||pts.length)})}${loading?' · '+t('refreshing'):''}</b><span>${fmtDateTime(pts[pts.length-1].t*1000)}</span></div></div>`:null}
      ${err?html`<p class="err">${err}</p>`:null}
    </div>
    ${!data?html`<div class="card"><p class="muted">${t("Loading telemetry history…")}</p></div>`:pts.length<2?html`<div class="card"><p class="muted">${t("Not enough telemetry in this window.")}</p></div>`:html`
      <div class="history-playback"><${GNSSPlots} scope="history" sats=${epoch.satellites||[]} signals=${epoch.signals||[]} hiddenSystems=${data.display_hidden_constellations||[]}
        side=${html`<${EpochSummary} epoch=${epoch}/>`}/></div>
      <${HistoryCharts} epochs=${pts} health=${health}/>`}
  </div>`;
}

/* ------------------------------------------------------------------- app */

/* Sign-in is a dialog, not a gate: the dashboard, data history and file
   download pages are public, so an anonymous visitor must never be sent to a
   form. Only the administrator-only pages need this. */
function Login({ onDone, onClose }) {
  const [u, setU] = useState(''), [p, setP] = useState(''), [err, setErr] = useState('');
  const [busy, setBusy] = useState(false);
  const submit = async e => {
    e.preventDefault(); setErr(''); setBusy(true);
    try {
      await api('/api/login', { method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ username: u, password: p }) });
      onDone();
    } catch (e) { setErr(e.unauth ? t('Invalid credentials') : (e.message || t('Sign-in failed'))); }
    finally { setBusy(false); }
  };
  return html`<div class="modal-backdrop" onMouseDown=${e=>{if(e.target===e.currentTarget&&onClose)onClose()}}>
    <div class="card modal-card login-card">
      <div class="card-title"><div><h2>${t("PSGNSSB sign in")}</h2>
        <p class="muted">${t("Administrator access. The public pages need no account.")}</p></div></div>
      <form onSubmit=${submit}>
        <p><label>${t("Username")}</label><input class="form-control" value=${u} autoFocus autoComplete="username"
          onChange=${e => setU(e.target.value)}/></p>
        <p><label>${t("Password")}</label><input class="form-control" type="password" value=${p} autoComplete="current-password"
          onChange=${e => setP(e.target.value)}/></p>
        <div class="submit">
          <button class="btn act" type="submit" disabled=${busy}>${busy?t('Signing in…'):t('Sign in')}</button>
          ${onClose?html`<button class="btn act" type="button" onClick=${onClose}>${t("Cancel")}</button>`:null}
        </div>
        ${err ? html`<div class="err">${err}</div>` : null}
      </form>
    </div></div>`;
}

/* --------------------------------------------------------------- receiver */

const mountMessagesText = m => (m.messages || []).map(x => x.type + ':' + x.interval).join('\n');

function MountpointSettings({ settings }) {
  const [rows, setRows] = useState([]), [err, setErr] = useState(''), [busy, setBusy] = useState(false);
  const [runtime,setRuntime]=useState({}), runtimePrev=useRef({});
  useEffect(() => setRows((settings.mountpoints || []).map(m => ({...m, messages_text:mountMessagesText(m)}))), [settings]);
  useEffect(()=>{const go=()=>api('/api/mountpoints').then(items=>{const now=Date.now(),next={};items.forEach(m=>{const p=runtimePrev.current[m.name];next[m.name]={...m,rate:p&&now>p.time&&m.bytes_out>=p.bytes?(m.bytes_out-p.bytes)/((now-p.time)/1000):null};runtimePrev.current[m.name]={bytes:m.bytes_out,time:now};});setRuntime(next);}).catch(()=>{});go();const t=setInterval(go,2000);return()=>clearInterval(t);},[]);
  const patch = (i, values) => setRows(rs => rs.map((r,n) => n === i ? {...r,...values} : r));
  const add = () => setRows(rs => [...rs, {name:'New_Mountpoint',enabled:true,source_id:rs.length+1,
    format:'RTCM 3.2',carrier:3,nav_system:'GPS+GAL+BDS',network:'',country:'',
    nmea:0,solution:0,generator:'PSGNSS',compress:'none',auth:'B',fee:'N',bitrate:0,msm_detail:'',
    messages_text:'1006:10\n1008:10\n1033:10\n1077:1\n1097:1\n1127:1'}]);
  const remove = i => { if (confirm(t('Delete mountpoint {name}? Connected rovers using it will be disconnected.',{name:rows[i].name}))) setRows(rs => rs.filter((_,n)=>n!==i)); };
  const parseMessages = text => text.split(/[\n,]+/).map(x=>x.trim()).filter(Boolean).map(x => {
    const m=x.match(/^(\d{1,4})(?:\s*[:(\/]\s*(\d{1,4})\)?)?$/);
    if(!m) throw new Error(t('Message entries must be TYPE:INTERVAL, for example 1077:1'));
    return {type:Number(m[1]),interval:Number(m[2]||1)};
  });
  const save = async () => {
    setErr(''); let mountpoints;
    try { mountpoints=rows.map(({messages_text,...m})=>({...m,messages:parseMessages(messages_text)})); }
    catch(e){setErr(e.message);return;}
    if(!confirm(t('Apply mountpoint configuration?\n\nPSGNSSB will restart briefly. Connected rovers will reconnect through the caster.'))) return;
    setBusy(true);
    try {
      await api('/api/settings/mountpoints',{method:'PUT',headers:{'Content-Type':'application/json'},body:JSON.stringify({mountpoints})});
      setTimeout(()=>window.location.reload(),3000);
    } catch(e){setErr(e.message||t('Could not save mountpoints'));setBusy(false);}
  };
  return html`<div class="card settings-section">
    <div class="card-title"><div><h2>${t("Mountpoint management")}</h2><p class="muted">${t("Each mountpoint is a lossless RTCM message filter over the receiver stream.")}</p></div>
      <button class="btn act" onClick=${add} disabled=${busy}>${t("Add mountpoint")}</button></div>
    ${err?html`<p class="err">${err}</p>`:null}
    <div class="mountpoint-editor">${rows.map((m,i)=>html`<div class=${'mount-editor '+(m.enabled?'':'disabled')} key=${i}>
      <div class="mount-editor-head"><label class="check"><input class="form-check-input" type="checkbox" checked=${m.enabled} onChange=${e=>patch(i,{enabled:e.target.checked})}/><strong>${m.name||t('Unnamed mountpoint')}</strong></label>
        ${runtime[m.name]?html`<span class="mount-live mono">${t(runtime[m.name].clients===1?'{n} client':'{n} clients',{n:runtime[m.name].clients})} · ${runtime[m.name].rate==null?t('measuring'):fmtBytes(runtime[m.name].rate)+'/s'} · ${runtime[m.name].last_data?t('last {d}',{d:fmtDateTime(runtime[m.name].last_data)}):t('no data')}</span>`:null}
        <span class=${'pill '+(m.enabled?'ok':'warn')}>${m.enabled?t('served'):t('disabled')}</span><button class="btn act danger" onClick=${()=>remove(i)}>${t("Delete")}</button></div>
      <div class="settings-fields">
        <div><label>${t("Name")}</label><input class="form-control mono" value=${m.name} onChange=${e=>patch(i,{name:e.target.value})}/></div>
        <div><label>${t("Source ID")}</label><input class="form-control" type="number" min="1" value=${m.source_id} onChange=${e=>patch(i,{source_id:Number(e.target.value)})}/></div>
        <div><label>${t("Format")}</label><input class="form-control" value=${m.format} onChange=${e=>patch(i,{format:e.target.value})}/></div>
        <div><label>${t("Carrier")}</label><select class="form-select" value=${m.carrier} onChange=${e=>patch(i,{carrier:Number(e.target.value)})}>${[0,1,2,3].map(v=>html`<option value=${v}>${v}</option>`)}</select></div>
        <div><label>${t("Navigation systems")}</label><input class="form-control mono" value=${m.nav_system} onChange=${e=>patch(i,{nav_system:e.target.value})}/></div>
        <div><label>${t("Network")}</label><input class="form-control" value=${m.network} onChange=${e=>patch(i,{network:e.target.value})}/></div>
        <div><label>${t("Country")}</label><input class="form-control" maxlength="3" value=${m.country} onChange=${e=>patch(i,{country:e.target.value.toUpperCase()})}/></div>
        <div><label>${t("Generator")}</label><input class="form-control" value=${m.generator} onChange=${e=>patch(i,{generator:e.target.value})}/></div>
        <div><label>${t("Compression")}</label><input class="form-control" value=${m.compress} onChange=${e=>patch(i,{compress:e.target.value})}/></div>
        <div><label>${t("Authentication")}</label><select class="form-select" value=${m.auth} onChange=${e=>patch(i,{auth:e.target.value})}><option value="B">${t("Basic (B)")}</option></select></div>
        <div><label>${t("Fee")}</label><select class="form-select" value=${m.fee} onChange=${e=>patch(i,{fee:e.target.value})}><option value="N">${t("No")}</option><option value="Y">${t("Yes")}</option></select></div>
        <div><label>${t("Bitrate (bit/s)")}</label><input class="form-control" type="number" min="0" value=${m.bitrate} onChange=${e=>patch(i,{bitrate:Number(e.target.value)})}/></div>
        <div><label>${t("NMEA requirement")}</label><input class="form-control" type="number" min="0" value=${m.nmea} onChange=${e=>patch(i,{nmea:Number(e.target.value)})}/></div>
        <div><label>${t("Solution")}</label><input class="form-control" type="number" min="0" value=${m.solution} onChange=${e=>patch(i,{solution:Number(e.target.value)})}/></div>
        <div class="wide"><label>${t("Format details")}</label><input class="form-control" value=${m.msm_detail||''} onChange=${e=>patch(i,{msm_detail:e.target.value})}/></div>
        <div class="wide"><label>${t("RTCM messages · TYPE:INTERVAL seconds")}</label><textarea class="mono" rows="4" value=${m.messages_text} onChange=${e=>patch(i,{messages_text:e.target.value})}></textarea></div>
      </div>
      <p class="field-help">${t("Sourcetable coordinates always follow the base position below; they cannot drift independently.")}</p>
      ${runtime[m.name]&&runtime[m.name].sourcetable?html`<details class="sourcetable-preview"><summary>${t("Live sourcetable record")}</summary><code>${runtime[m.name].sourcetable}</code></details>`:null}
    </div>`)}</div>
    <div class="form-actions"><button class="btn act" disabled=${busy} onClick=${save}>${busy?t('Saving and restarting…'):t('Save mountpoints and restart')}</button></div>
  </div>`;
}

function BasePosition({ position, locked, onApplied, onError }) {
  const [p,setP]=useState(position);
  useEffect(()=>setP(position),[position && position.latitude,position && position.longitude,position && position.height]);
  if(!p)return null;
  const submit=async e=>{e.preventDefault();
    const next={latitude:Number(p.latitude),longitude:Number(p.longitude),height:Number(p.height)};
    if(!confirm(t('Change the surveyed base position?\n\nThis updates receiver CFG-TMODE in RAM first. Corrections may move immediately. Verify the stream, then Keep to write both receiver flash and PSGNSSB configuration.')))return;
    try{onApplied(await api('/api/settings/position/apply',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(next)}));onError('');}
    catch(x){onError(x.message||t('Could not apply base position'));}
  };
  return html`<div class="card settings-section position-card"><h2>${t("Base position")}</h2>
    <p class="muted">${t("Enter surveyed geodetic latitude, longitude and ellipsoidal antenna-reference-point height. Use one consistent reference frame: an official ETRS89 realization is normally appropriate for European survey control; use WGS-84 only when your survey coordinates are explicitly WGS-84. The two are not interchangeable at centimetre precision. PSGNSSB converts the position for RTCM 1006 and keeps receiver CFG-TMODE aligned.")}</p>
    <form onSubmit=${submit}><div class="settings-fields position-fields">
      <div><label>${t("Latitude (degrees)")}</label><input class="form-control mono" type="number" step="0.000000001" min="-90" max="90" value=${p.latitude} onChange=${e=>setP({...p,latitude:e.target.value})}/></div>
      <div><label>${t("Longitude (degrees)")}</label><input class="form-control mono" type="number" step="0.000000001" min="-180" max="180" value=${p.longitude} onChange=${e=>setP({...p,longitude:e.target.value})}/></div>
      <div><label>${t("Ellipsoidal height (m)")}</label><input class="form-control mono" type="number" step="0.0001" value=${p.height} onChange=${e=>setP({...p,height:e.target.value})}/></div>
    </div><div class="form-actions"><button class="btn act danger" type="submit" disabled=${locked}>${t("Apply to RAM and verify")}</button></div></form>
  </div>`;
}

function StationID({ value, locked, onApplied, onError }) {
  const [id,setID]=useState(value);
  useEffect(()=>setID(value),[value]);
  const submit=async e=>{e.preventDefault(); const n=Number(id);
    if(!Number.isInteger(n)||n<0||n>4095){onError(t('Station ID must be between 0 and 4095'));return;}
    if(!confirm(t('Change the RTCM reference station ID to {n}?\n\nThis updates the receiver and generated RTCM together. Corrections may pause while PSGNSSB restarts after Keep.',{n})))return;
    try{onApplied(await api('/api/settings/station-id/apply',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({station_id:n})}));onError('');}
    catch(x){onError(x.message||t('Could not apply station ID'));}
  };
  return html`<div class="card settings-section station-id-card"><h2>${t("RTCM station ID")}</h2>
    <p class="muted">${t("The receiver's DF003 value and PSGNSSB-generated station messages must always match. This guarded transaction changes both.")}</p>
    <form onSubmit=${submit}><div class="settings-fields"><div><label>${t("Station ID")}</label><input class="form-control mono" type="number" min="0" max="4095" value=${id} onChange=${e=>setID(e.target.value)}/></div></div>
    <div class="form-actions"><button class="btn act danger" type="submit" disabled=${locked||Number(id)===Number(value)}>${t("Apply to RAM and verify")}</button></div></form>
  </div>`;
}

function GeneralSettings({ settings }) {
  const [g,setG]=useState(settings.general),[busy,setBusy]=useState(false),[err,setErr]=useState('');
  const set=(k,v)=>setG({...g,[k]:v});
  const save=async e=>{e.preventDefault();
    if(!confirm(t('Save general configuration and restart PSGNSSB?\n\nChanging listener addresses, archive paths or retention affects live operation.')))return;
    setBusy(true);setErr('');try{await api('/api/settings/general',{method:'PUT',headers:{'Content-Type':'application/json'},body:JSON.stringify(g)});setTimeout(()=>window.location.reload(),3500);}catch(x){setErr(x.message||t('Could not save settings'));setBusy(false);}
  };
  const listeners=g.hub_listeners||[];
  const hidden=g.display_hidden_constellations||[];
	const profiles=settings.board_profiles||[];
	const activeProfile=profiles.find(p=>p.id===g.receiver_profile);
  const toggleShown=system=>set('display_hidden_constellations',hidden.includes(system)?hidden.filter(x=>x!==system):[...hidden,system]);
  return html`<div class="card settings-section"><div class="card-title"><div><h2>${t("Station and service configuration")}</h2></div></div>
    ${err?html`<p class="err">${err}</p>`:null}<form onSubmit=${save}><div class="settings-fields">
      <div><label>${t("Station name")}</label><input class="form-control" value=${g.station_name} onChange=${e=>set('station_name',e.target.value)} required/></div>
      <div><label>${t("Antenna descriptor")}</label><input class="form-control mono" value=${g.antenna} onChange=${e=>set('antenna',e.target.value)} required/></div>
      <div><label>${t("Receiver descriptor")}</label><input class="form-control" value=${g.receiver_description} onChange=${e=>set('receiver_description',e.target.value)}/></div>
	  <div><label>${t("Receiver board profile")}</label><select class="form-select" value=${g.receiver_profile||''} onChange=${e=>set('receiver_profile',e.target.value)}>
		${profiles.map(p=>html`<option key=${p.id} value=${p.id} disabled=${!p.implemented}>${p.name} · ${p.receiver}${p.implemented?'':' ('+t('driver stub')+')'}</option>`)}</select></div>
	  ${activeProfile?html`<div class="wide field-help board-profile-note"><strong>${activeProfile.vendor}</strong> · ${activeProfile.protocol} · ${activeProfile.form_factors.join(', ')}<br/>${activeProfile.note}</div>`:null}
      <div><label>${t("NTRIP caster listen")}</label><input class="form-control mono" value=${g.caster_listen} onChange=${e=>set('caster_listen',e.target.value)}/></div>
      <div><label>${t("PROXY listener")}</label><input class="form-control mono" value=${g.proxy_listen} onChange=${e=>set('proxy_listen',e.target.value)} placeholder=${t("disabled when blank")}/></div>
      <div><label>${t("Web UI listen")}</label><input class="form-control mono" value=${g.web_listen} onChange=${e=>set('web_listen',e.target.value)}/></div>
      ${listeners.map((l,i)=>html`<div key=${l.name}><label>${t('{name} address',{name:l.name||t('Hub listener')})}</label><input class="form-control mono" value=${l.listen} onChange=${e=>{const a=listeners.map(x=>({...x}));a[i].listen=e.target.value;set('hub_listeners',a)}}/></div>`)}
      <div><label>${t("Archive mount path")}</label><input class="form-control mono" value=${g.archive_mount} onChange=${e=>set('archive_mount',e.target.value)} required/><div class="field-help">${t("The SMB source currently mounted here is shown in Diagnostics.")}</div></div>
      <div><label>${t("Local spool path")}</label><input class="form-control mono" value=${g.archive_spool} onChange=${e=>set('archive_spool',e.target.value)} required/></div>
      <div><label>${t("Archive retention (days)")}</label><input class="form-control" type="number" min="0" max="3650" value=${g.archive_retention_days} onChange=${e=>set('archive_retention_days',Number(e.target.value))}/></div>
      <div><label>${t("Share sync interval (seconds)")}</label><input class="form-control" type="number" min="5" max="3600" value=${g.archive_sync_seconds} onChange=${e=>set('archive_sync_seconds',Number(e.target.value))}/></div>
      <div><label>${t("Telemetry retention (days)")}</label><input class="form-control" type="number" min="1" max="365" value=${g.telemetry_retention_days} onChange=${e=>set('telemetry_retention_days',Number(e.target.value))}/></div>
      <div class="wide"><label>${t("Map tile source")}</label><input class="form-control mono" value=${g.map_tiles||''} onChange=${e=>set('map_tiles',e.target.value)} placeholder="https://tile.openstreetmap.org/{z}/{x}/{y}.png"/><div class="field-help">${t('Needs {z}, {x} and {y}, and https. PSGNSS fetches and caches tiles itself, sending a User-Agent that identifies this station, because a browser cannot and OpenStreetMap blocks anonymous requests. Their servers are donated: for anything more than an occasional look, use a provider you pay or one you host.')}</div></div>
      <div><label>${t("Map contact")}</label><input class="form-control" value=${g.map_contact||''} onChange=${e=>set('map_contact',e.target.value)} placeholder="you@example.org"/><div class="field-help">${t("Sent in the User-Agent so a tile provider can ask this station to stop. Falls back to the station name.")}</div></div>
      <div class="wide"><label>${t("Map attribution")}</label><input class="form-control" value=${g.map_attribution||''} onChange=${e=>set('map_attribution',e.target.value)} placeholder="© OpenStreetMap contributors"/><div class="field-help">${t("Required with a tile source. Tile providers ask for credit and OpenStreetMap's usage policy requires it.")}</div></div>
      <div><label>${t("Log level")}</label><select class="form-select" value=${g.logging_level} onChange=${e=>set('logging_level',e.target.value)}><option>debug</option><option>info</option><option>warn</option><option>error</option></select></div>
      <div class="wide settings-subsection"><h3>${t("Default constellation display")}</h3><p class="field-help">${t("Checked constellations are shown by default on Dashboard and Data History. Unchecked ones start hidden and remain available from each page's Display controls.")}</p>
        <div class="settings-checks">${Object.keys(PLOT_SYSTEMS).map(system=>html`<label class="check" key=${system}><input class="form-check-input" type="checkbox" checked=${!hidden.includes(system)} onChange=${()=>toggleShown(system)}/><span style=${{color:PLOT_SYSTEMS[system].color}}>${system}</span> ${t("shown by default")}</label>`)}</div></div>
      <div class="wide settings-subsection"><h3>${t("Automatic diagnosis")}</h3><div class="settings-checks"><label class="check"><input class="form-check-input" type="checkbox" checked=${!!g.diagnostics_auto} onChange=${e=>set('diagnostics_auto',e.target.checked)}/>${t("Run diagnosis automatically while the Diagnostics page is open")}</label></div>
        <div class="inline-field"><label>${t("Run every")}</label><input class="form-control" type="number" min="1" max="1440" value=${g.diagnostics_interval_minutes} onChange=${e=>set('diagnostics_interval_minutes',Number(e.target.value))} disabled=${!g.diagnostics_auto}/><span>${t("minutes")}</span></div></div>
    </div><div class="form-actions"><button class="btn act" disabled=${busy}>${busy?t('Saving and restarting…'):t('Save general settings')}</button></div></form>
  </div>`;
}

function AdminSettings() {
  const [rows,setRows]=useState([]),[err,setErr]=useState(''),[user,setUser]=useState(''),[pw,setPw]=useState('');
  const load=()=>api('/api/admins').then(setRows).catch(e=>setErr(e.message)); useEffect(()=>{load();},[]);
  const create=async e=>{e.preventDefault();try{await api('/api/admins',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({username:user,password:pw})});setUser('');setPw('');load();}catch(x){setErr(x.message)}};
  const change=async name=>{const p=prompt(t('New password for {name} (minimum 8 characters)',{name}));if(!p)return;try{await api('/api/admins/'+encodeURIComponent(name)+'/password',{method:'PUT',headers:{'Content-Type':'application/json'},body:JSON.stringify({password:p})});}catch(x){setErr(x.message)}};
  const remove=async name=>{if(!confirm(t('Delete web administrator {name}?',{name})))return;try{await api('/api/admins/'+encodeURIComponent(name),{method:'DELETE'});load();}catch(x){setErr(x.message)}};
  return html`<div class="card settings-section"><div class="card-title"><div><h2>${t("Web administrators")}</h2></div></div>
    ${err?html`<p class="err">${err}</p>`:null}<div class="tbl-scroll"><table class="table table-vcenter"><thead><tr><th>${t("Account")}</th><th>${t("Created")}</th><th>${t("Last login")}</th><th></th></tr></thead><tbody>${rows.map(a=>html`<tr key=${a.username}><td class="mono">${a.username}</td><td>${fmtDateTime(a.created_at)}</td><td>${a.last_login?fmtDateTime(a.last_login):t('Never')}</td><td><button class="user-link" onClick=${()=>change(a.username)}>${t("Change password")}</button> · <button class="user-link bad-text" onClick=${()=>remove(a.username)}>${t("Delete")}</button></td></tr>`)}</tbody></table></div>
    <form class="admin-create" onSubmit=${create}><div class="settings-fields"><div><label>${t("New administrator")}</label><input class="form-control" value=${user} onChange=${e=>setUser(e.target.value)} required pattern="[A-Za-z0-9_.-]+"/></div><div><label>${t("Password")}</label><input class="form-control" type="password" minlength="8" value=${pw} onChange=${e=>setPw(e.target.value)} required/></div></div><div class="form-actions"><button class="btn act">${t("Add administrator")}</button></div></form>
  </div>`;
}

/* Phases reported by the integrity monitor, in the order they occur. */
const INTEGRITY_PHASES={preparing:'PREPARING PRIVATE RTCM FEED',connecting:'CONNECTING · COLLECTING EPHEMERIS',
  observing:'OBSERVING AND SOLVING',comparing:'COMPARING WITH BROADCAST POSITION',
  cancelling:'STOPPING THE CHECK'};
const fmtElapsed=v=>{const s=Math.max(0,Math.round(v||0));return Math.floor(s/60)+':'+String(s%60).padStart(2,'0')};
const fmtMM=v=>v==null?'\u2014':Math.round(v)+' mm';

function IntegrityMonitor() {
  const [data,setData]=useState(null),[form,setForm]=useState(null),[err,setErr]=useState(''),[busy,setBusy]=useState(false);
  const load=useCallback(()=>api('/api/integrity').then(x=>{setData(x);setForm({...x.settings,password:''});setErr('')}).catch(e=>setErr(e.message||t('Could not load external integrity settings'))),[]);
  useEffect(()=>{load();},[load]);
  useEffect(()=>{if(!data||!data.running)return;const t=setInterval(load,2000);return()=>clearInterval(t)},[data&&data.running,load]);
  if(!form)return html`<div class="card settings-section"><h2>${t("External position integrity")}</h2><p class=${err?'err':'muted'}>${err||t('Loading…')}</p></div>`;
  const set=(k,v)=>setForm({...form,[k]:v});
  const save=async e=>{e.preventDefault();setBusy(true);setErr('');const {has_password,...payload}=form;try{const x=await api('/api/integrity/settings',{method:'PUT',headers:{'Content-Type':'application/json'},body:JSON.stringify(payload)});setData(x);setForm({...x.settings,password:''})}catch(x){setErr(x.message||t('Could not save integrity settings'))}finally{setBusy(false)}};
  const run=async()=>{setBusy(true);setErr('');try{await api('/api/integrity/run',{method:'POST'});setData({...data,running:true})}catch(x){setErr(x.message||t('Could not start comparison'))}finally{setBusy(false)}};
  /* Cancelling releases the reference-network login immediately: many
     subscriptions allow one connection at a time, so a running check can keep a
     rover out of the field. */
  const cancel=async()=>{setBusy(true);setErr('');try{setData(await api('/api/integrity/cancel',{method:'POST'}))}catch(x){setErr(x.message||t('Could not cancel the check'))}finally{setBusy(false)}};
  const last=data&&data.latest;
  const prog=data&&data.progress;
  const overrun=!!(prog&&prog.planned_s&&prog.elapsed_s>prog.planned_s);
  const elapsedPct=prog&&prog.planned_s?Math.min(100,Math.round(prog.elapsed_s/prog.planned_s*100)):0;
  return html`<div class="card settings-section integrity-card"><div class="card-title"><div><h2>${t("External position integrity")}</h2><p class="muted">${t("Independently solves this antenna from its live RTCM observations against an external reference stream, then compares that result with the broadcast ETRS89/ellipsoidal base position. Any NTRIP network that serves this area works. Scheduled in UTC.")}</p></div>
    <div class="integrity-actions">
      <button class="btn act" type="button" disabled=${busy||data.running||!form.has_password} onClick=${run}>${data.running?t('CHECK RUNNING…'):t('RUN CHECK NOW')}</button>
      ${data.running?html`<button class="btn act danger" type="button" disabled=${busy} onClick=${cancel}>${t("CANCEL CHECK")}</button>`:null}</div></div>
    ${err?html`<p class="err">${err}</p>`:null}
    ${prog&&prog.running?html`<div class="integrity-progress">
      <div class="diag-meter"><div><span>${t(INTEGRITY_PHASES[prog.phase]||'CHECK RUNNING')}</span>
        <span class="mono">${fmtElapsed(prog.elapsed_s)}${prog.planned_s?' / '+fmtElapsed(prog.planned_s):''}</span></div>
        <div class="diag-meter-track"><i style=${{width:elapsedPct+'%'}}></i></div>
        <small>${overrun?t('Observation window elapsed; RTKLIB is resolving the final solution.')
          :!prog.epochs?t('RTKLIB collects broadcast ephemeris from the receiver before it can solve anything; the first epochs usually appear about a minute in. Zero here is normal until then.')
          :t('The check continues on the base even if this page is closed. It holds one login on the reference network until it finishes or is cancelled.')}</small></div>
      <div class="integrity-grid">
        <div class="rd"><div class="rd-l">${t("Epochs solved")}</div><div class="rd-v mono">${fmtNum(prog.epochs||0)}</div></div>
        <div class="rd"><div class="rd-l">${t("Fixed")}</div><div class="rd-v mono">${fmtNum(prog.fixed||0)}</div></div>
        <div class="rd"><div class="rd-l">${t("Float")}</div><div class="rd-v mono">${fmtNum(prog.float||0)}</div></div>
        <div class="rd"><div class="rd-l">${t("Solution in use")}</div><div class="rd-v mono">${prog.solution&&prog.solution!=='none'?prog.solution:t('not yet reached')}</div><small class="field-help">${t("needs 3 fixed or 10 float")}</small></div>
        <div class="rd"><div class="rd-l">${t("Horizontal offset")}</div><div class="rd-v mono">${fmtMM(prog.horizontal_mm)}</div></div>
        <div class="rd"><div class="rd-l">${t("Vertical offset")}</div><div class="rd-v mono">${fmtMM(prog.vertical_mm)}</div></div>
      </div>
      <p class="note">${t("Offsets are provisional: they are the median of the solutions accepted so far and move until the window closes.")}</p>
    </div>`:null}
    ${last?html`<div class="integrity-last"><span class=${'pill '+(last.status==='pass'?'ok':last.status==='fail'?'bad':'warn')}>${t(last.status)}</span><strong>${last.detail}</strong><span class="muted mono">${fmtDateTime(last.finished_at*1000)}</span></div>`:html`<p class="note">${t("No comparison has completed yet.")}</p>`}
    <form onSubmit=${save}><div class="settings-fields">
      <div class="wide settings-checks"><label class="check"><input class="form-check-input" type="checkbox" checked=${form.enabled} onChange=${e=>set('enabled',e.target.checked)}/>${t("Run automatically once per UTC day")}</label></div>
      <div><label>${t("UTC schedule")}</label><input class="form-control" type="time" step="60" value=${form.schedule} onChange=${e=>set('schedule',e.target.value)} required/></div>
      <div><label>${t("Observation duration (min)")}</label><input class="form-control" type="number" min="5" max="120" value=${form.duration_minutes} onChange=${e=>set('duration_minutes',Number(e.target.value))}/></div>
      <div><label>${t("Horizontal tolerance (mm)")}</label><input class="form-control" type="number" min="1" max="10000" value=${form.tolerance_horizontal_mm} onChange=${e=>set('tolerance_horizontal_mm',Number(e.target.value))}/></div>
      <div><label>${t("Vertical tolerance (mm)")}</label><input class="form-control" type="number" min="1" max="10000" value=${form.tolerance_vertical_mm} onChange=${e=>set('tolerance_vertical_mm',Number(e.target.value))}/></div>
      <div><label>${t("Reference caster")}</label><input class="form-control mono" value=${form.host} onChange=${e=>set('host',e.target.value)} required/></div>
      <div><label>${t("Reference mountpoint")}</label><input class="form-control mono" value=${form.mountpoint} onChange=${e=>set('mountpoint',e.target.value)} required/></div>
      <div><label>${t("Reference username")}</label><input class="form-control" autoComplete="off" value=${form.username} onChange=${e=>set('username',e.target.value)} required=${form.enabled}/></div>
      <div><label>${t("Reference password")}</label><input class="form-control" type="password" autoComplete="new-password" value=${form.password} placeholder=${form.has_password?t('Stored securely · blank keeps it'):t('Required before enabling')} onChange=${e=>set('password',e.target.value)}/></div>
    </div><div class="form-actions"><button class="btn act" disabled=${busy}>${busy?t('Saving…'):t('Save integrity settings')}</button></div></form>
  </div>`;
}

function Receiver({ position, stationID }) {
  const [data, setData] = useState(null);
  const [err, setErr] = useState('');
  const [busy, setBusy] = useState('');
  const [pending, setPending] = useState(null);
  const load = useCallback(async () => {
    setBusy(t('Reading receiver configuration…'));
    try { setData(await api('/api/receiver')); setPending(null); setErr(''); }
    catch (e) {
      if (e.body && e.body.pending) { setPending(e.body.pending); setErr(''); }
      else setErr(e.message || t('Could not read receiver configuration'));
    }
    setBusy('');
  }, []);
  useEffect(() => { load(); }, [load]);
  /* Every control funnels through here: one verified RAM transaction. */
  const apply = async (label, changes) => {
    if (!changes.length) return;
    setBusy(t('Applying to RAM and reading back…'));
    try {
      const r = await api('/api/receiver/apply', {method:'POST', headers:{'Content-Type':'application/json'},
        body:JSON.stringify({label, changes})});
      setPending(r); setErr('');
    } catch (x) { setErr(x.message || t('Receiver change failed')); }
    setBusy('');
  };
  const settle = async commit => {
    setBusy(commit ? t('Writing BBR + Flash…') : t('Reverting RAM…'));
    try { await api('/api/receiver/' + (commit ? 'commit' : 'revert'), {method:'POST'}); const positionCommit=commit&&pending&&pending.label==='Base position'; setPending(null); await load(); if(positionCommit)setTimeout(()=>window.location.reload(),3000); }
    catch (x) { setErr(x.message || t('Could not settle receiver change')); setBusy(''); }
  };
  if (!data && !err && !pending) return html`<p class="muted">${t("Reading receiver configuration…")}</p>`;
  const sys = data && data.system;
  const locked = !!pending || !!busy;
  return html`<div class="receiver">
    ${err ? html`<div class="card error-card"><p class="err">${err}</p></div>` : null}
    ${data && !pending ? html`<${BasePosition} position=${position} locked=${locked} onApplied=${setPending} onError=${setErr}/>` : null}
    ${data ? html`<div class="strip receiver-id">
	  ${readout(t('Board profile'), data.board_profile ? data.board_profile.name : '—')}
      ${readout(t('Model'), data.model || '—')}${readout(t('Firmware'), data.firmware || '—')}
      ${readout(t('Hardware'), data.hardware || '—')}${readout(t('Protocol'), data.protocol || '—')}
      ${readout(t('Receiver uptime'), sys ? fmtUptime(sys.run_time_s) : t('unavailable'))}
      ${readout(t('Last restart'), sys ? sys.boot_reason || t('type {n}',{n:sys.boot_type}) : '—')}
      ${readout(t('Constellations'), (data.constellations || []).join(', ') || '—')}
    </div>` : null}
    ${data && data.system_error ? html`<p class="muted">${t('Uptime could not be read:')} ${data.system_error}</p>` : null}
    ${busy ? html`<p class="muted">${busy}</p>` : null}
    ${pending ? html`<${PendingChange} pending=${pending} onSettle=${settle} onExpired=${load}/>` : null}
    ${data && !pending ? html`
      <${StationID} value=${stationID} locked=${locked} onApplied=${setPending} onError=${setErr}/>
      <${MessageRates} data=${data} locked=${locked} onApply=${apply} onRefresh=${load}/>
      <${SignalToggles} data=${data} locked=${locked} onApply=${apply}/>
      <${RawKeys} data=${data} locked=${locked} onApply=${apply}/>
    ` : null}
  </div>`;
}

/* Integrity lives on Diagnostics, not here: it is a verification that runs and
   reports, like the rest of that page, rather than configuration. */
const SETTINGS_SECTIONS = { station:'Station and service', receiver:'Receiver',
                            mountpoints:'Mountpoints', pushout:'Push-out', admins:'Administrators',
                            backup:'Backup and restore' };

/* Settings backup and restore.

   The file holds live credentials -- NTRIP passwords are recoverable by design
   -- so it is encrypted with a passphrase the operator chooses and the station's
   master key never leaves the machine. That trade-off is stated on screen,
   because the passphrase is the only thing protecting the file. A restore is
   confirmed against what the file actually contains, not against its name. */
const BACKUP_MIN_PASSPHRASE = 12;

function BackupSettings() {
  const [pass,setPass]=useState(''),[confirmPass,setConfirmPass]=useState('');
  const [restorePass,setRestorePass]=useState(''),[file,setFile]=useState(null);
  const [summary,setSummary]=useState(null),[typed,setTyped]=useState('');
  const [busy,setBusy]=useState(false),[err,setErr]=useState(''),[note,setNote]=useState('');
  const short=pass.length<BACKUP_MIN_PASSPHRASE;

  const download=async()=>{
    setErr('');setNote('');
    if(short){setErr(t('The passphrase must be at least {n} characters.',{n:BACKUP_MIN_PASSPHRASE}));return}
    if(pass!==confirmPass){setErr(t('The two passphrases do not match. A backup you cannot open is worse than none.'));return}
    setBusy(true);
    try{
      const r=await fetch('/api/backup',{method:'POST',credentials:'same-origin',
        headers:{'Content-Type':'application/json'},body:JSON.stringify({passphrase:pass})});
      if(!r.ok){const b=await r.json().catch(()=>({}));throw new Error(b.error||('HTTP '+r.status))}
      const blob=await r.blob();
      const name=r.headers.get('X-Backup-Filename')||'psgnssb-backup.psbk';
      const url=URL.createObjectURL(blob);
      const a=document.createElement('a');a.href=url;a.download=name;document.body.appendChild(a);a.click();
      a.remove();URL.revokeObjectURL(url);
      setNote(t('Saved {name}. Keep it and its passphrase apart from this machine.',{name}));
    }catch(e){setErr(e.message||t('Backup failed'))}
    finally{setBusy(false)}
  };

  /* Chunked rather than one spread into String.fromCharCode: a file the
     operator picked by mistake can be any size, and spreading it would
     overflow the call stack instead of failing cleanly. */
  const readFile=f=>new Promise((resolve,reject)=>{
    const fr=new FileReader();
    fr.onload=()=>{
      const bytes=new Uint8Array(fr.result);
      let out='';
      for(let i=0;i<bytes.length;i+=0x8000)out+=String.fromCharCode.apply(null,bytes.subarray(i,i+0x8000));
      resolve(btoa(out));
    };
    fr.onerror=()=>reject(new Error(t('Could not read that file')));
    fr.readAsArrayBuffer(f);
  });

  const inspect=async()=>{
    setErr('');setNote('');setSummary(null);setTyped('');
    if(!file){setErr(t('Choose a .psbk file first.'));return}
    setBusy(true);
    try{
      const data=await readFile(file);
      setSummary({...await api('/api/backup/inspect',{method:'POST',
        headers:{'Content-Type':'application/json'},
        body:JSON.stringify({passphrase:restorePass,data})}),data});
    }catch(e){setErr(e.message||t('Could not read that backup'))}
    finally{setBusy(false)}
  };

  const restore=async()=>{
    setErr('');setNote('');setBusy(true);
    try{
      const r=await api('/api/backup/restore',{method:'POST',headers:{'Content-Type':'application/json'},
        body:JSON.stringify({passphrase:restorePass,data:summary.data})});
      setNote(t('Restored. The daemon is restarting and you have been signed out; sign in with the credentials from the backup. Rollback snapshot: {file}',{file:r.rollback_snapshot}));
      setSummary(null);setTyped('');
      setTimeout(()=>window.location.reload(),4000);
    }catch(e){setErr(e.message||t('Restore failed'))}
    finally{setBusy(false)}
  };

  return html`<div class="auto-grid">
    <div class="card settings-section backup-card">
      <div class="card-title"><div><h2>${t("Download a backup")}</h2>
        <p class="muted">${t("Configuration, administrators, NTRIP accounts and their access rules, the integrity monitor and the push-out targets. Not telemetry, the connection log or the receiver audit trail: those stay with this machine.")}</p></div></div>
      <p class="note warn-note">${t("The file contains every NTRIP password in usable form, because this station stores them recoverably. It is encrypted with the passphrase below and nothing else — the station's master key is not in it. Whoever has both the file and the passphrase has the credentials. There is no way to recover a forgotten passphrase.")}</p>
      <div class="field-row two">
        <p><label>${t("Passphrase")}</label><input class="form-control" type="password" value=${pass} autoComplete="new-password"
          onChange=${e=>setPass(e.target.value)}/>
          <small class="field-help">${short?t('At least {n} characters.',{n:BACKUP_MIN_PASSPHRASE}):t('Long enough.')}</small></p>
        <p><label>${t("Repeat passphrase")}</label><input class="form-control" type="password" value=${confirmPass} autoComplete="new-password"
          onChange=${e=>setConfirmPass(e.target.value)}/></p>
      </div>
      <div class="submit"><button class="btn act" disabled=${busy} onClick=${download}>${t("Download backup")}</button></div>
    </div>

    <div class="card settings-section backup-card restore-card">
      <div class="card-title"><div><h2>${t("Restore from a backup")}</h2>
        <p class="muted">${t("Replaces the configuration, administrators, NTRIP accounts, integrity settings and push-out targets with the file's. It does not merge.")}</p></div></div>
      <p class="note warn-note">${t("Every administrator session ends, this one included: you sign in again with the credentials from the backup. A snapshot of the current settings is written next to the database first, encrypted with the same passphrase, so there is a way back. The daemon restarts at the end.")}</p>
      <div class="field-row two">
        <p><label>${t("Backup file")}</label><input class="form-control" type="file" accept=".psbk"
          onChange=${e=>{setFile(e.target.files&&e.target.files[0]);setSummary(null);setTyped('')}}/></p>
        <p><label>${t("Passphrase")}</label><input class="form-control" type="password" value=${restorePass} autoComplete="off"
          onChange=${e=>{setRestorePass(e.target.value);setSummary(null);setTyped('')}}/></p>
      </div>
      <div class="submit"><button class="btn act" disabled=${busy||!file} onClick=${inspect}>${t("Open and check")}</button></div>
      ${summary?html`<div class="settings-subsection">
        <h3>${t("This file contains")}</h3>
        <dl class="kv">
          <dt>${t("Station")}</dt><dd class="mono">${summary.station||'—'}</dd>
          <dt>${t("Taken")}</dt><dd class="mono">${fmtDateTime(summary.created)}</dd>
          <dt>${t("Built by")}</dt><dd class="mono">${summary.app_version||'—'}</dd>
          <dt>${t("Administrators")}</dt><dd class="mono">${summary.admins}</dd>
          <dt>${t("NTRIP accounts")}</dt><dd class="mono">${summary.users}</dd>
          <dt>${t("Mountpoints")}</dt><dd class="mono">${summary.mountpoints}</dd>
          <dt>${t("Push-out targets")}</dt><dd class="mono">${summary.push_targets}</dd>
        </dl>
        <div class="power-confirm">
          <label>${t("Type RESTORE to confirm")}
            <input class="form-control" value=${typed} onChange=${e=>setTyped(e.target.value)}/></label>
          <button class="btn act danger" disabled=${busy||typed!=='RESTORE'} onClick=${restore}>${t("Restore these settings")}</button>
        </div>
      </div>`:null}
    </div>
    ${err?html`<p class="err">${err}</p>`:null}
    ${note?html`<p class="note ok-note">${note}</p>`:null}
  </div>`;
}

/* Uploading this station's stream to a remote caster. The feature follows
   RTKBase's ntrip_A/ntrip_B services; see CREDITS.md. A target publishes a
   local mountpoint, so a remote rover gets exactly what a local one gets. */
const PUSH_STATES = { connected:'ok', connecting:'warn', retrying:'bad', stopped:'' };
const blankTarget = sources => ({ name:'', enabled:false, source:sources[0]||'', host:'', mountpoint:'',
                                  protocol:'v2', username:'', password:'', has_password:false, state:'stopped' });

function PushoutSettings() {
  const [data,setData]=useState(null),[rows,setRows]=useState([]),[err,setErr]=useState(''),[busy,setBusy]=useState(false);
  const load=useCallback(()=>api('/api/pushout').then(x=>{setData(x);setRows(x.targets.map(t=>({...t,password:''})));setErr('')})
    .catch(e=>setErr(e.message||t('Could not load push-out configuration'))),[]);
  useEffect(()=>{load();},[load]);
  /* Live state moves on its own, so refresh while the page is open, but never
     over unsaved edits. */
  useEffect(()=>{const t=setInterval(()=>{if(!busy)api('/api/pushout').then(x=>setData(x)).catch(()=>{})},5000);return()=>clearInterval(t)},[busy]);
  if(!data)return html`<div class="card settings-section"><h2>${t("NTRIP push-out")}</h2><p class=${err?'err':'muted'}>${err||t('Loading…')}</p></div>`;
  const sources=data.sources||[];
  const set=(i,k,v)=>setRows(rows.map((r,n)=>n===i?{...r,[k]:v}:r));
  const live=name=>(data.targets||[]).find(t=>t.name===name)||{};
  const save=async e=>{e.preventDefault();setBusy(true);setErr('');
    try{const payload={targets:rows.map(({name,enabled,source,host,mountpoint,protocol,username,password})=>
          ({name,enabled,source,host,mountpoint,protocol,username,password}))};
      const x=await api('/api/pushout',{method:'PUT',headers:{'Content-Type':'application/json'},body:JSON.stringify(payload)});
      setData(x);setRows(x.targets.map(t=>({...t,password:''})));
    }catch(x){setErr(x.message||t('Could not save push-out configuration'))}finally{setBusy(false)}};
  return html`<div class="card settings-section pushout-card"><div class="card-title"><div><h2>${t("NTRIP push-out")}</h2>
      <p class="muted">${t("Uploads a local mountpoint to a remote caster, so this base can feed an external network without anything in front of it. Each target sends exactly the bytes a rover connected here receives. A slow remote caster never delays the rovers served here.")}</p></div>
      <button class="btn act" type="button" onClick=${()=>setRows([...rows,blankTarget(sources)])} disabled=${rows.length>=8}>${t("+ Add target")}</button></div>
    ${err?html`<p class="err">${err}</p>`:null}
    ${rows.length===0?html`<p class="note">${t("No push targets configured. Nothing is uploaded anywhere.")}</p>`:null}
    <form onSubmit=${save}>${rows.map((r,i)=>{const st=live(r.name);
      return html`<div class="mount-editor" key=${i}>
        <div class="mount-editor-head">
          <label class="check"><input class="form-check-input" type="checkbox" checked=${r.enabled} onChange=${e=>set(i,'enabled',e.target.checked)}/>${r.name||t('New target')}</label>
          <span class=${'pill '+(PUSH_STATES[st.state]||'')}>${t(st.state||'stopped')}</span>
          ${st.state==='connected'?html`<span class="mount-live mono">${t('{b} sent · {n} connects',{b:fmtBytes(st.bytes_sent||0),n:st.connects||0})}${st.dropped?' · '+t('{n} dropped',{n:st.dropped}):''}</span>`:null}
          ${st.last_error?html`<span class="mount-live bad-text">${st.last_error}${st.next_attempt_in?' · '+t('retry in {n}s',{n:st.next_attempt_in}):''}</span>`:null}
          <button class="user-link bad-text" type="button" onClick=${()=>setRows(rows.filter((_,n)=>n!==i))}>${t("Remove")}</button>
        </div>
        <div class="settings-fields">
          <div><label>${t("Target name")}</label><input class="form-control" value=${r.name} onChange=${e=>set(i,'name',e.target.value)} required maxlength="40"/></div>
          <div><label>${t("Local mountpoint to publish")}</label><select class="form-select" value=${r.source} onChange=${e=>set(i,'source',e.target.value)}>
            ${sources.map(m=>html`<option key=${m} value=${m}>${m}</option>`)}</select></div>
          <div><label>${t("Protocol")}</label><select class="form-select" value=${r.protocol} onChange=${e=>set(i,'protocol',e.target.value)}>
            <option value="v2">NTRIP 2.0 (POST)</option><option value="v1">NTRIP 1.0 (SOURCE)</option></select></div>
          <div><label>${t("Remote caster")}</label><input class="form-control mono" value=${r.host} onChange=${e=>set(i,'host',e.target.value)} placeholder="caster.example.org:2101" required/></div>
          <div><label>${t("Remote mountpoint")}</label><input class="form-control mono" value=${r.mountpoint} onChange=${e=>set(i,'mountpoint',e.target.value)} required/></div>
          <div><label>${t("Username")}</label><input class="form-control" autoComplete="off" value=${r.username} onChange=${e=>set(i,'username',e.target.value)} required=${r.protocol==='v2'} disabled=${r.protocol==='v1'} placeholder=${r.protocol==='v1'?t('not used by NTRIP 1.0'):''}/></div>
          <div><label>${t("Password")}</label><input class="form-control" type="password" autoComplete="new-password" value=${r.password} onChange=${e=>set(i,'password',e.target.value)}
            placeholder=${r.has_password?t('Stored securely · blank keeps it'):t('Required before enabling')}/></div>
        </div></div>`})}
      <div class="form-actions"><button class="btn act" disabled=${busy}>${busy?t('Saving…'):t('Save push-out targets')}</button>
        <span class="muted">${t("Saving applies immediately; rovers connected here are not interrupted.")}</span></div></form>
  </div>`;
}

function Settings() {
  const [settings,setSettings]=useState(null),[err,setErr]=useState(''),[section,setSection]=useState('station');
  useEffect(()=>{api('/api/settings').then(setSettings).catch(e=>setErr(e.message||t('Could not load settings')));},[]);
  if(!settings)return html`<div>${err?html`<p class="err">${err}</p>`:html`<p class="muted">${t("Loading settings…")}</p>`}</div>`;
  /* Every section stays mounted and is hidden rather than unmounted, so a
     pending verified receiver change keeps its countdown and its Keep/Revert
     buttons while another section is open. */
  const panel=(key,body)=>html`<section class="settings-panel" key=${key} hidden=${section!==key}>${body}</section>`;
  return html`<div class="settings-page">
    <nav class="subnav">${Object.entries(SETTINGS_SECTIONS).map(([k,v])=>html`
      <button key=${k} class=${section===k?'on':''} onClick=${()=>setSection(k)}>${t(v)}</button>`)}
    </nav>
    ${panel('station',html`<${GeneralSettings} settings=${settings}/>`)}
    ${panel('receiver',html`<${Receiver} position=${settings.position} stationID=${settings.station.station_id}/>`)}
    ${panel('mountpoints',html`<${MountpointSettings} settings=${settings}/>`)}
    ${panel('pushout',html`<${PushoutSettings}/>`)}
    ${panel('admins',html`<${AdminSettings}/>`)}
    ${panel('backup',html`<${BackupSettings}/>`)}
  </div>`;
}

const fmtUptime = s => {
  if (s == null) return '—';
  const d = Math.floor(s / 86400);
  return d ? d + 'd ' + fmtDur(s % 86400) : fmtDur(s);
};

function PendingChange({ pending, onSettle, onExpired }) {
  const [left, setLeft] = useState(0);
  useEffect(() => {
    const tick = () => setLeft(Math.max(0, Math.round((new Date(pending.revert_at) - Date.now()) / 1000)));
    tick(); const t = setInterval(tick, 1000); return () => clearInterval(t);
  }, [pending.revert_at]);
  /* The server reverts on its own; re-read once it has had time to finish. */
  useEffect(() => {
    if (left !== 0) return;
    const t = setTimeout(onExpired, 4000); return () => clearTimeout(t);
  }, [left === 0]);
  const cs = pending.changes || [];
  return html`<div class="card receiver-pending">
    <div class="card-title"><h2>${t('Verified RAM change pending')}${pending.label ? ' · ' + t(pending.label) : ''}</h2>
      <span class=${'pill ' + (left > 15 ? 'warn' : 'bad')}>${left ? t('auto-revert in {n}s',{n:left}) : t('reverting…')}</span></div>
    <p>${t(cs.length === 1 ? 'This key is live in RAM only, and was read back from the receiver.' : 'These {n} keys are live in RAM only, and were read back from the receiver.',{n:cs.length})}
      ${t('Check the live stream and rovers, then keep or revert. Doing nothing reverts at {t}.',{t:fmtDateTime(pending.revert_at)})}</p>
    <div class="tbl-scroll"><table class="table table-vcenter"><thead><tr><th>${t("Key")}</th><th>${t("Before")}</th><th>${t("Now in RAM")}</th><th>${t("Verified")}</th></tr></thead><tbody>
      ${cs.map(c => html`<tr key=${c.key}><td>${keyLabel(c)}</td><td class="mono">${c.old_display}</td>
        <td class="mono">${c.display}</td><td>${c.verified ? html`<span class="pill ok">${t("read back")}</span>` : html`<span class="pill bad">${t("no")}</span>`}</td></tr>`)}
    </tbody></table></div>
    <div class="form-actions">
      <button class="btn act" onClick=${() => onSettle(true)}>${t("Keep and write BBR + Flash")}</button>
      <button class="btn act danger" onClick=${() => onSettle(false)}>${t("Revert now")}</button>
    </div>
  </div>`;
}

const keyLabel = r => r.documented
  ? html`<span title=${r.desc || ''}>${r.name}</span> <span class="muted mono">${r.key}</span>`
  : html`<span class="muted">${t("Undocumented key")}</span> <span class="mono">${r.key}</span>`;

function MessageRates({ data, locked, onApply, onRefresh }) {
  const [proto, setProto] = useState('RTCM');
  const [onlyActive, setOnlyActive] = useState(true);
  const [draft, setDraft] = useState({});
  const ports = data.ports || [];
  const msgs = (data.messages || []).filter(m => proto === 'All' || m.protocol === proto)
    .filter(m => !onlyActive || Object.values(m.ports).some(p => p.rate > 0) ||
      Object.values(m.ports).some(p => draft[p.key] !== undefined));
  const changes = Object.entries(draft).map(([key, v]) => ({key, text: String(v)}));
  const byKey = {};
  (data.messages || []).forEach(m => Object.entries(m.ports).forEach(([port, p]) => { byKey[p.key] = {m, port, p}; }));
  const set = (p, raw) => {
    const n = raw === '' ? 0 : Math.max(0, Math.min(255, parseInt(raw, 10) || 0));
    setDraft(d => { const x = {...d}; if (n === p.rate) delete x[p.key]; else x[p.key] = n; return x; });
  };
  const summary = changes.map(c => byKey[c.key].m.label + ' ' + byKey[c.key].port + ' → ' + c.text).join(', ');
  const touchesUSB = changes.some(c => byKey[c.key].port === 'USB');
  const submit = () => {
    const warn = touchesUSB ? '\n\n' + t('USB is the port PSGNSSB reads. Changing it changes what the hub, mountpoints and archives receive.') : '';
    if (confirm(t('Apply to receiver RAM with auto-revert?') + '\n\n' + summary + warn)) { onApply('Message output rates', changes); setDraft({}); }
  };
  return html`<div class="card">
    <div class="card-title"><h2>${t("Output messages and rates")}</h2>
      <button class="btn act" onClick=${onRefresh} disabled=${locked}>${t("Refresh from receiver")}</button></div>
    <p class="muted">${t("Rate is the number of navigation epochs between outputs; 0 disables the message on that port. PSGNSSB reads the receiver on USB. Mountpoint message filters are separate and are not changed here.")}</p>
    <div class="quick">
      <div class="seg">${['RTCM', 'UBX', 'NMEA', 'All'].map(p => html`<button key=${p} class=${proto === p ? 'on' : ''} onClick=${() => setProto(p)}>${p}</button>`)}</div>
      <label class="check"><input class="form-check-input" type="checkbox" checked=${onlyActive} onChange=${e => setOnlyActive(e.target.checked)}/> ${t("Only messages enabled on some port")}</label>
    </div>
    <div class="tbl-scroll msg-rates"><table class="table table-vcenter"><thead><tr><th>${t("Message")}</th>${ports.map(p => html`<th key=${p}>${p}</th>`)}</tr></thead><tbody>
      ${msgs.map(m => html`<tr key=${m.id}><td>${m.label}</td>${ports.map(port => {
        const p = m.ports[port];
        if (!p) return html`<td key=${port} class="muted">—</td>`;
        const v = draft[p.key] !== undefined ? draft[p.key] : p.rate;
        return html`<td key=${port}><input class=${'rate mono' + (draft[p.key] !== undefined ? ' changed' : '') + (v > 0 ? ' on' : '')}
          type="number" min="0" max="255" value=${v} disabled=${locked} title=${p.key}
          onChange=${e => set(p, e.target.value)}/></td>`;
      })}</tr>`)}
      ${msgs.length ? null : html`<tr><td colspan=${ports.length + 1} class="muted">${t("No messages match.")}</td></tr>`}
    </tbody></table></div>
    <div class="form-actions">
      <button class="btn act" disabled=${locked || !changes.length} onClick=${submit}>${t(changes.length === 1 ? 'Apply {n} rate change to RAM' : 'Apply {n} rate changes to RAM',{n:changes.length || ''})}</button>
      ${changes.length ? html`<button class="btn act" onClick=${() => setDraft({})}>${t("Discard")}</button><span class="muted">${summary}</span>` : null}
    </div>
  </div>`;
}

function SignalToggles({ data, locked, onApply }) {
  const [draft, setDraft] = useState({});
  const cons = data.signals || [];
  const cur = {};
  cons.forEach(c => { cur[c.key] = c.enabled; c.signals.forEach(s => { cur[s.key] = s.enabled; }); });
  const val = k => draft[k] !== undefined ? draft[k] : cur[k];
  const toggle = (k, on) => setDraft(d => { const x = {...d}; if (on === cur[k]) delete x[k]; else x[k] = on; return x; });
  const changes = Object.entries(draft).map(([key, on]) => ({key, text: on ? 'true' : 'false'}));
  const submit = () => {
    const off = cons.filter(c => draft[c.key] === false).map(c => c.label);
    const msg = t('Apply signal configuration to receiver RAM with auto-revert?') + '\n\n' +
      t('u-blox: signal changes reset the GNSS subsystem. Tracking restarts, and rovers lose corrections until it has reacquired.') +
      (off.length ? '\n\n' + t('Disabling {list} removes it from every mountpoint.',{list:off.join(', ')}) : '');
    if (confirm(msg)) { onApply('Constellations and signals', changes); setDraft({}); }
  };
  return html`<div class="card">
    <h2>${t("Constellations and signals")}</h2>
    <p class="muted">${t("A signal takes effect only while its constellation is enabled. Systems not listed by MON-VER are not supported by this firmware and are marked as such.")}</p>
    <div class="signal-grid">${cons.map(c => html`<div class=${'signal-sys' + (c.supported ? '' : ' unsupported')} key=${c.id}>
      <label class="check sys-toggle"><input class="form-check-input" type="checkbox" checked=${val(c.key)} disabled=${locked} onChange=${e => toggle(c.key, e.target.checked)}/>
        <strong>${c.label}</strong>
        ${c.supported ? null : html`<span class="pill warn">${t("not in firmware")}</span>`}
        ${draft[c.key] !== undefined ? html`<span class="pill warn">${t("changed")}</span>` : null}</label>
      ${c.signals.map(s => html`<label class="check" key=${s.key} title=${s.key}>
        <input class="form-check-input" type="checkbox" checked=${val(s.key)} disabled=${locked || !val(c.key)} onChange=${e => toggle(s.key, e.target.checked)}/>
        ${s.label}${draft[s.key] !== undefined ? html` <span class="pill warn">${t("changed")}</span>` : null}</label>`)}
    </div>`)}</div>
    <div class="form-actions">
      <button class="btn act" disabled=${locked || !changes.length} onClick=${submit}>${t(changes.length === 1 ? 'Apply {n} signal change to RAM' : 'Apply {n} signal changes to RAM',{n:changes.length || ''})}</button>
      ${changes.length ? html`<button class="btn act" onClick=${() => setDraft({})}>${t("Discard")}</button>` : null}
    </div>
  </div>`;
}

function RawKeys({ data, locked, onApply }) {
  const [q, setQ] = useState('');
  const [sel, setSel] = useState(null);
  const [text, setText] = useState('');
  const [open, setOpen] = useState(false);
  const needle = q.trim().toLowerCase();
  const pick = r => { setSel(r); setText(r.documented ? (r.type === 'L' ? r.display : (r.enum ? r.display.split(' ')[0] : rawNumber(r))) : 'hex:' + r.value); };
  const submit = e => {
    e.preventDefault();
    if (confirm(t('Apply {k} = {v} to RAM with auto-revert?',{k:sel.name || sel.key,v:text}))) onApply('Advanced key edit', [{key: sel.key, text}]);
  };
  return html`<div class="card">
    <div class="card-title"><h2>${t("Advanced: individual configuration keys")}</h2>
      <button class="btn act" onClick=${() => setOpen(!open)}>${open ? t('Hide') : t('Show')}</button></div>
    <p class="muted">${t("For keys without a dedicated control above. Names, units and constants come from the u-blox HPG interface description; keys u-blox does not document are marked and take raw hex.")}</p>
    ${open ? html`<div>
      ${sel ? html`<form class="user-form raw-edit" onSubmit=${submit}>
        <div class="field"><label>${sel.documented ? sel.name : t('Undocumented key')} <span class="mono">${sel.key}</span></label>
          <input class="form-control mono" value=${text} onChange=${e => setText(e.target.value)} disabled=${locked} required/>
          <div class="field-help">${sel.desc || ''} ${valueHelp(sel)}</div></div>
        <div class="form-actions"><button class="btn act" type="submit" disabled=${locked}>${t("Apply to RAM and verify")}</button>
          <button class="btn act" type="button" onClick=${() => setSel(null)}>${t("Cancel")}</button></div>
      </form>` : null}
      <input class="form-control" placeholder=${t("Filter by name, key or description")} value=${q} onInput=${e => setQ(e.target.value)}/>
      ${(data.groups || []).map(g => {
        const rows = (g.keys || []).filter(k => !needle || (k.name || '').toLowerCase().includes(needle) ||
          k.key.toLowerCase().includes(needle) || (k.desc || '').toLowerCase().includes(needle));
        if (!g.error && !rows.length) return null;
        return html`<div key=${g.name} class="raw-group"><h3>${g.name} <span class="muted">(${rows.length})</span></h3>
          ${g.error ? html`<p class="err">${g.error}</p>` : html`<div class="tbl-scroll raw-scroll"><table class="table table-vcenter"><thead><tr><th>${t("Key")}</th><th>${t("Value")}</th><th></th></tr></thead><tbody>
          ${rows.slice(0, 300).map(k => html`<tr key=${k.key}><td>${keyLabel(k)}</td><td class="mono">${k.display}</td>
            <td><button class="user-link" disabled=${locked} onClick=${() => pick(k)}>${t("Edit")}</button></td></tr>`)}
          ${rows.length > 300 ? html`<tr><td colspan="3" class="muted">${t('{n} more — refine the filter.',{n:rows.length - 300})}</td></tr>` : null}
          </tbody></table></div>`}</div>`;
      })}
    </div>` : null}
  </div>`;
}

/* Number without the display's scale and unit: what ParseValue expects. */
const rawNumber = r => r.display.split(' ')[0];
const valueHelp = r => {
  if (!r.documented) return t('Little-endian bytes, e.g. hex:01.');
  if (r.type === 'L') return t('true or false.');
  if (r.enum) return t('One of:') + ' ' + r.enum.map(e => e.name).join(', ') + '.';
  if (r.scale) return t('Enter unscaled units (value × {s}).',{s:r.scale + (r.unit ? ' ' + r.unit : '')});
  return (r.type || '') + (r.unit ? ', ' + r.unit : '') + '.';
};

function readout(label, value) { return html`<div class="rd"><div class="rd-l">${label}</div><div class="rd-v mono">${value}</div></div>`; }

/* ------------------------------------------------- logs and power control */

/* Two log sources, because they answer different questions. The in-memory
   buffer is structured and free to read, but a restart empties it -- which is
   exactly when an operator wants the log. The journal survives restarts and is
   what a bug report should carry. */
const LOG_SOURCES = { memory:'Live (in memory)', journal:'Journal (persisted)' };
const LOG_WINDOWS = { '15m':'15 minutes', '1h':'1 hour', '6h':'6 hours', '24h':'24 hours', '7d':'7 days' };
const LOG_LEVELS = ['ALL','ERROR','WARN','INFO','DEBUG'];

function LogViewer() {
  const [source,setSource]=useState('memory'),[window_,setWindow]=useState('1h');
  const [level,setLevel]=useState('ALL'),[q,setQ]=useState(''),[rows,setRows]=useState([]);
  const [err,setErr]=useState(''),[busy,setBusy]=useState(false),[auto,setAuto]=useState(true);
  const load=useCallback(async()=>{
    setBusy(true);
    try{
      if(source==='memory'){
        const r=await api('/api/operations/logs?limit=500&level='+encodeURIComponent(level)+'&q='+encodeURIComponent(q));
        setRows((r.entries||[]).map(e=>({time:e.time,level:e.level,
          message:e.message+(e.attrs?' '+Object.entries(e.attrs).map(([k,v])=>k+'='+v).join(' '):'')})));
      }else{
        const r=await api('/api/logs?window='+window_+'&limit=1000&level='+encodeURIComponent(level)+'&q='+encodeURIComponent(q));
        setRows((r.lines||[]).slice().reverse());
      }
      setErr('');
    }catch(e){setErr(e.message||t('Could not read the log'))}
    finally{setBusy(false)}
  },[source,window_,level,q]);
  useEffect(()=>{load()},[load]);
  useEffect(()=>{if(!auto)return;const t=setInterval(load,10000);return()=>clearInterval(t)},[auto,load]);
  return html`<div class="card log-card"><div class="card-title">
      <div><h2>${t("Daemon log")}</h2><p class="muted">${source==='memory'
        ?t('The last 1000 entries this daemon has logged since it started. Lost on restart.')
        :t('Read from the systemd journal, so it survives restarts and reboots.')}</p></div>
      ${source==='journal'?html`<a class="btn act" href=${'/api/logs/download?window='+window_+'&limit=5000&level='+encodeURIComponent(level)+'&q='+encodeURIComponent(q)}>${t("↓ Download")}</a>`:null}
    </div>
    <div class="log-filters">
      <div><label>${t("Source")}</label><select class="form-select" value=${source} onChange=${e=>setSource(e.target.value)}>
        ${Object.entries(LOG_SOURCES).map(([k,v])=>html`<option key=${k} value=${k}>${t(v)}</option>`)}</select></div>
      <div><label>${t("Window")}</label><select class="form-select" value=${window_} disabled=${source!=='journal'} onChange=${e=>setWindow(e.target.value)}>
        ${Object.entries(LOG_WINDOWS).map(([k,v])=>html`<option key=${k} value=${k}>${t(v)}</option>`)}</select></div>
      <div><label>${t("Level")}</label><select class="form-select" value=${level} onChange=${e=>setLevel(e.target.value)}>
        ${LOG_LEVELS.map(l=>html`<option key=${l} value=${l}>${l}</option>`)}</select></div>
      <div><label>${t("Contains")}</label><input class="form-control" value=${q} placeholder=${t("mountpoint, ip, message…")} onChange=${e=>setQ(e.target.value)}/></div>
      <div class="log-actions"><button class="btn act" type="button" onClick=${load} disabled=${busy}>${busy?t('Reading…'):t('Refresh')}</button>
        <label class="check"><input class="form-check-input" type="checkbox" checked=${auto} onChange=${e=>setAuto(e.target.checked)}/>${t("every 10 s")}</label></div>
    </div>
    ${err?html`<p class="err">${err}</p>`:null}
    <div class="log-lines">
      ${rows.length?rows.map((l,i)=>html`<div class=${'log-line '+(l.level||'INFO').toLowerCase()} key=${i}>
        <span class="mono">${l.time?fmtDateTime(l.time):''}</span><b>${l.level||''}</b><code>${l.message}</code></div>`)
        :html`<p class="muted">${t("Nothing matched.")}</p>`}
    </div>
    <p class="note">${t(rows.length===1?'{n} line shown, newest first.':'{n} lines shown, newest first.',{n:rows.length})}</p>
  </div>`;
}

/* Taking the base off the air is a physical act: a shutdown needs someone at
   the Pi to power it back on. Each button therefore asks for its word to be
   typed, and the daemon logs who asked. */
const POWER_ACTIONS = {
  restart: { label:'Restart PSGNSS', confirm:'RESTART', path:'/api/operations/restart',
             note:'Stops and starts the daemon. The receiver stream, caster and archive pause for a few seconds.' },
  reboot:  { label:'Reboot the Pi', confirm:'REBOOT', path:'/api/system/power',
             note:'Reboots the host. Everything is down for about a minute, and the rover loses corrections.' },
  shutdown:{ label:'Shut down the Pi', confirm:'SHUTDOWN', path:'/api/system/power',
             note:'Powers the host off. It will not come back without someone pressing power at the Pi.' },
};

function PowerControls() {
  const [pending,setPending]=useState(''),[typed,setTyped]=useState(''),[err,setErr]=useState(''),[done,setDone]=useState('');
  const act=POWER_ACTIONS[pending];
  const go=async()=>{
    if(!act||typed!==act.confirm)return;
    setErr('');
    try{
      await api(act.path,{method:'POST',headers:{'Content-Type':'application/json'},
        body:JSON.stringify(pending==='restart'?{confirm:act.confirm}:{action:pending,confirm:act.confirm})});
      setDone(t('{action} requested.',{action:t(act.label)})+' '+(pending==='shutdown'
        ?t('This page will stop responding; the Pi needs a physical power-on.')
        :t('This page will stop responding for a moment and then come back.')));
      setPending('');setTyped('');
    }catch(e){setErr(e.message||t('The request was refused'))}
  };
  return html`<div class="card power-card"><div class="card-title"><div><h2>${t("Power")}</h2>
    <p class="muted">${t("Restarting or powering down interrupts corrections for every connected rover.")}</p></div></div>
    ${err?html`<p class="err">${err}</p>`:null}
    ${done?html`<p class="note">${done}</p>`:null}
    <div class="power-actions">
      ${Object.entries(POWER_ACTIONS).map(([k,a])=>html`<div class="power-action" key=${k}>
        <b>${t(a.label)}</b><small>${t(a.note)}</small>
        <button class=${'act'+(k==='restart'?'':' danger')} type="button"
          onClick=${()=>{setPending(pending===k?'':k);setTyped('');setErr('');setDone('')}}>
          ${pending===k?t('Cancel'):t(a.label)}</button></div>`)}
    </div>
    ${act?html`<div class="power-confirm">
      <label>${t("Type")} <b class="mono">${act.confirm}</b> ${t("to confirm")}</label>
      <input class="form-control mono" value=${typed} autoFocus onChange=${e=>setTyped(e.target.value)}/>
      <button class="btn act danger" type="button" disabled=${typed!==act.confirm} onClick=${go}>${t(act.label)}</button>
    </div>`:null}
  </div>`;
}

/* Check names come from the server. Most are fixed; the rest are a fixed
   prefix and the name of a mountpoint, archive or target, which stays as is. */
const CHECK_PREFIXES = ['Mountpoint ', 'Archive ', 'NTRIP push-out · ', 'RTCM output · '];
const checkName = name => {
  const p = CHECK_PREFIXES.find(x => name.startsWith(x));
  return p ? t(p.replace(/[ ·]+$/, '')) + p.slice(p.replace(/[ ·]+$/, '').length) + name.slice(p.length) : t(name);
};

function Operations() {
  const [result,setResult]=useState(null),[settings,setSettings]=useState(null),[err,setErr]=useState(''),[busy,setBusy]=useState(false);
  const diagnose=useCallback(async()=>{setBusy(true);setErr('');try{setResult(await api('/api/operations/diagnose',{method:'POST'}))}catch(e){setErr(e.message||t('Diagnosis failed'))}finally{setBusy(false)}},[]);
  useEffect(()=>{api('/api/settings').then(x=>setSettings(x.general)).catch(e=>setErr(e.message||t('Could not read diagnosis settings')))},[]);
  useEffect(()=>{if(!settings||!settings.diagnostics_auto)return;diagnose();const t=setInterval(diagnose,Math.max(1,settings.diagnostics_interval_minutes||60)*60000);return()=>clearInterval(t)},[settings&&settings.diagnostics_auto,settings&&settings.diagnostics_interval_minutes,diagnose]);
  const checks=(result&&result.checks)||[],passed=checks.filter(x=>x.ok).length,h=(result&&result.host)||{},hub=(result&&result.hub)||{};
  return html`<div class="operations-page diagnostic-runner">
    <div class="card diagnostic-launch"><span class="instrument-title">${t("System verification")}</span><h2>${t("Run a complete PSGNSSB diagnosis")}</h2><p class="muted">${t("Tests receiver and GNSS freshness, configuration, telemetry database, RINEX converter, NTRIP mountpoints, archive writers, local spool and network storage. When enabled, the most recent external position-integrity result is included. The storage tests create, sync and remove a small probe file.")}</p>
      <button class="btn act diagnose-button" onClick=${diagnose} disabled=${busy}>${busy?t('DIAGNOSING…'):t('DIAGNOSE')}</button>
      ${settings?html`<span class="diagnostic-schedule">${settings.diagnostics_auto?t('Automatic diagnosis every {n} min · configure in Settings',{n:settings.diagnostics_interval_minutes}):t('Automatic diagnosis is off · configure in Settings')}</span>`:null}
      ${err?html`<p class="err">${err}</p>`:null}
    </div>
    ${result?html`<div class="card diagnostic-results"><div class="card-title"><div><h2>${t("Diagnosis result")}</h2><p class="muted">${t('Completed {t}',{t:fmtDateTime(result.time)})}</p></div><span class=${'pill '+(result.ok?'ok':'bad')}>${result.ok?t('ALL {n} TESTS PASSED',{n:passed}):t('{p} / {n} PASSED',{p:passed,n:checks.length})}</span></div>
      <div class="diagnostic-test-list">${checks.map(c=>html`<div class=${'diagnostic-test '+(c.ok?'pass':'fail')} key=${c.name}><span class="diagnostic-test-icon">${c.ok?'✓':'!'}</span><div><b>${checkName(c.name)}</b><small>${c.detail}</small></div><strong>${c.ok?t('PASS'):t('FAIL')}</strong></div>`)}</div>
      <div class="diagnostic-readouts">${readout('CPU',(h.cpu_percent||0).toFixed(1)+'%')}${readout(t('Temperature'),(h.temp_c||0).toFixed(1)+' °C')}${readout(t('Memory free'),fmtBytes((h.mem_free_mb||0)*1048576)+' / '+fmtBytes((h.mem_total_mb||0)*1048576))}${readout(t('Disk free'),fmtBytes((h.disk_free_mb||0)*1048576)+' / '+fmtBytes((h.disk_total_mb||0)*1048576))}${readout(t('Valid frames'),fmtNum(hub.frames||0))}${readout(t('Discarded bytes'),fmtBytes(hub.bytes_dropped||0))}${readout(t('NTRIP clients'),fmtNum(result.clients||0))}${readout(t('Rejected clients'),fmtNum(result.rejected||0))}${result.ephemeris?readout(t('Ephemeris ready'),fmtNum(result.ephemeris.ready||0)+' / '+fmtNum(result.ephemeris.satellites||0)+' '+t('sats')+(result.ephemeris.systems?' · '+Object.entries(result.ephemeris.systems).filter(([,v])=>v.satellites).map(([k,v])=>k+' '+v.ready+'/'+v.satellites).join(' · '):'')):null}</div>
    </div>`:html`<div class="card diagnostic-empty"><p>${t("No diagnosis has been run in this browser session.")}</p></div>`}
    <${IntegrityMonitor}/>
    <${LogViewer}/>
    <${PowerControls}/>
  </div>`;
}

const PAGE_HELP = {
  dashboard:[
    ['Tracked satellites','Satellites for which the receiver currently reports positive carrier-to-noise density. Almanac, ephemeris and navigation-file entries are excluded.'],
    ['Used by receiver','Tracked satellites marked by the receiver as contributing to its present navigation and timing solution. This is not an RTK rover fix count.'],
    ['SNR / C/N0','Carrier-to-noise density in dB-Hz for each received band. Higher values generally mean a cleaner observation; the useful threshold depends on antenna, environment and elevation.'],
    ['Skyplot','Live azimuth is clockwise from north; elevation runs from 0° at the outer horizon to 90° at the centre zenith.'],
    ['Signal bands','Each SNR bar is one signal band such as GPS L1/L2/L5 or Galileo E1/E5/E6, so one satellite can have several bars.'],
    ['Constellation controls','Display controls hide a system from both skyplot and SNR without changing receiver configuration or recorded data.'],
    ['15° mask','The dashed ring marks low-elevation signals, which are more exposed to obstruction, multipath and atmospheric path length.'],
    ['Base position map','The configured antenna position on raster tiles. Tiles are the only thing this UI fetches from another origin, and only while a tile source is set; the marker and the coordinates still show when the tile host is unreachable.'],
    ['Working range','The dashed circle is how far corrections from this base stay usable, computed from what it is broadcasting now: the constellations tracked and the number of carrier bands. Dual-frequency streams are normally limited by ambiguity resolution rather than by error growth, and each further constellation holds a fix a little further out. It is a planning figure — ionospheric activity moves it both ways — not a guarantee or a subscription boundary.']],
  history:[
    ['Window','Selects how much retained telemetry is requested. Charts are decimated to a bounded number of points for responsive rendering.'],
    ['Auto-refresh','The selected window reloads periodically and follows new epochs. Playing or moving the scrubber holds the chosen historical position until the window is reloaded.'],
    ['Playback','Steps the live instruments through stored receiver epochs. Pause or move the scrubber to inspect one epoch.'],
    ['Tracked line','Unique satellites with positive carrier-to-noise density at each epoch. Hover for the exact timestamp and count.'],
    ['Used line','Subset the receiver marked as used in its navigation/time solution at that epoch.'],
    ['Visibility heatmap','Rows are satellites observed anywhere in the selected window, columns are time and colour is aggregate SNR in dB-Hz; blank cells mean no positive observation.'],
    ['Throughput','Historical RTCM and UBX input rates are shown in kb/s; the client trace uses the right-hand axis.'],
    ['Host resources','CPU, temperature, available memory and archive free space are stored independently from receiver epochs.']],
  operations:[
    ['Diagnose','Runs one point-in-time verification pass. It does not restart or reconfigure the receiver.'],
    ['Receiver freshness','Requires a framed receiver message and a NAV-SAT telemetry epoch within the expected live interval.'],
    ['Configuration','Validates the complete loaded TOML configuration with the same rules used at daemon startup.'],
    ['Database','Reads the SQLite schema to confirm telemetry storage is reachable.'],
    ['RINEX converter','Checks that the configured RTKLIB convbin executable exists and is executable.'],
    ['Mountpoints','Every enabled NTRIP output must have emitted fresh correction data; client count is informational.'],
    ['Archive writers','RAW and NAV writers must have synchronized within three configured sync intervals.'],
    ['Storage probes','The local spool and mounted network archive are tested by creating, syncing and deleting a tiny temporary file.'],
    ['Automatic diagnosis','When enabled in Settings, the same complete pass repeats at the selected interval while this page is open.'],
    ['External integrity','An independent RTK static solution from this station’s live observations against an external NTRIP reference stream, compared with the broadcast base position against millimetre tolerances. It runs on a UTC schedule or on demand, takes the configured observation window, and holds one login on the reference network while it runs. Credentials are encrypted in SQLite.'],
    ['Integrity result','A result needs 3 fixed or 10 float epochs. Only the converged tail of the window is measured, because static processing accumulates and the early epochs are convergence.'],
    ['Log sources','The in-memory buffer holds the last 1000 entries since the daemon started and is emptied by a restart. The journal is read from systemd, survives restarts and reboots, and can be downloaded as text.'],
    ['Ephemeris','Complete broadcast data sets held for each satellite, decoded from the receiver\u2019s navigation subframes: GPS (RTCM 1019), BeiDou (1042) and Galileo (1046). A mountpoint that lists one of those types sends one message per ready satellite of that system; a set takes about 30 seconds to assemble from cold for GPS and BeiDou and up to a minute for Galileo, whose five word types repeat every 30 seconds. The receiver cannot emit ephemeris RTCM itself.'],
    ['Power','Restart affects the daemon only. Reboot takes the host down for about a minute. Shutdown requires someone at the Pi to power it back on. Each action is confirmed by typing its name and is recorded in the log with the administrator who asked.']],
  settings:[
    ['General save','Atomically validates and writes the managed TOML configuration, then gracefully restarts the single PSGNSSB daemon.'],
	['Board profile','Selects the receiver family and control protocol. X20P is fully supported; listed driver stubs are disabled until their hardware-specific control path is implemented.'],
    ['Base position','Surveyed antenna reference point. Latitude/longitude and ellipsoidal height must come from one stated reference frame and survey epoch.'],
    ['ETRS89 vs WGS-84','ETRS89 is fixed to stable Europe while WGS-84 follows a global frame. Do not mix their coordinates at centimetre precision.'],
    ['Station ID','RTCM DF003 identifier. Receiver observation messages and PSGNSSB-generated station messages must always carry the same value.'],
    ['Map tile source','An XYZ raster tile template for the dashboard map. Empty means no map and no external origin is permitted by the page policy at all. Attribution is required when it is set.'],
    ['Display defaults','Selects which constellations are shown by default in Dashboard and Data History; unchecked systems begin hidden. It does not enable or disable signals in the receiver.'],
    ['Automatic diagnosis','Controls repeat diagnosis on the Diagnostics page and its interval in minutes.'],
    ['Mountpoint','Named NTRIP correction stream with its own lossless RTCM message filter and sourcetable metadata.'],
    ['Push-out','Uploads a local mountpoint to a remote caster as an NTRIP server. A target publishes the mountpoint\u2019s exact broadcast bytes, including the generated station messages, so a remote rover receives what a local one receives. Credentials are encrypted in SQLite.'],
    ['Message interval','Seconds between selected RTCM message types on a mountpoint; filtering never transcodes receiver observations.'],
    ['Telemetry retention','Days of receiver epochs and host samples available to Data History. Archive retention separately controls files on storage.'],
    ['Backup contents','Configuration, administrators, NTRIP accounts with their access rules, integrity settings and push-out targets. Not telemetry, the connection log, integrity runs or the receiver audit trail: those are records of this machine, not settings.'],
    ['Backup passphrase','NTRIP passwords are recoverable by design, so a backup holds live credentials. The file is encrypted with this passphrase and the station\u2019s master key is never included, which is what lets a restore work on new hardware. A forgotten passphrase cannot be recovered and the file is then useless.'],
    ['Restore','Replaces rather than merges. Every administrator session ends, so you sign in again with the credentials from the backup, and the daemon restarts. A snapshot of the settings being replaced is written beside the database first, encrypted with the same passphrase.']],
  users:[
    ['Connection limit','Maximum simultaneous NTRIP sessions allowed for the account.'],
    ['Mountpoint access','Correction streams this rover account may request.'],
    ['IP rules','Optional permitted source addresses or CIDR networks; empty means unrestricted.'],
    ['Bytes / duration','Totals calculated from completed and active NTRIP sessions.'],
    ['Expiry','The selected calendar day remains valid through 23:59:59 UTC; the account becomes unusable at the next UTC midnight.'],
    ['Enabled','Disabling blocks new sessions without deleting credentials or historical connection records.'],
    ['Active','A rover session that is connected now and has no end timestamp yet.'],
    ['Mountpoint','The correction stream requested by the rover.'],
    ['Agent','Client software identification supplied in the NTRIP request.'],
    ['Bytes','Correction payload sent during that session.'],
    ['Client IP','Source address seen by PSGNSSB. With direct DNAT it may not identify the original public client unless PROXY protocol is deployed.'],
    ['Duration','Elapsed session time for active clients, or final connected time for completed sessions.']],
  files:[
    ['RINEX preset','Presets match what a post-processing service accepts: IGN wants RINEX 2.11, GPS only, 30 s; NRCan wants 3.04 with GPS, GLONASS and Galileo at 30 s. Station default keeps every epoch and every constellation. Parameters follow RTKBase; see CREDITS.'],
    ['RINEX header','Marker, antenna, receiver and the approximate station position are written into every file, which is what a service needs to accept a submission.'],
    ['RAW','Receiver correction stream recorded without transcoding.'],
    ['NAV','UBX navigation subframes used to build the RINEX navigation file.'],
    ['RINEX','Standard GNSS observation/navigation files generated for the selected UTC interval.'],
    ['Observable','A measurement code actually present after conversion, grouped by constellation and band.'],
    ['Date range','Choose a start and end day, up to 31 days. Every touched archive file is joined by product before conversion.'],
    ['UTC interval','Start and end are converted to exact UTC cut points. The downloader can trim a currently growing archive to the latest complete data.'],
    ['OBS / NAV','Observation files contain measurements used for positioning; navigation files contain broadcast orbit and clock messages.'],
    ['Compression','The finished RINEX products are packaged into one downloadable ZIP; conversion does not modify the source archives.']],
};

/* The orbital mark.
   Every satellite shares one period and one direction per ring, and is spaced
   by a negative begin offset -- a phase, not a different speed. The previous
   version gave each satellite its own duration (48s, 52s, 56s...) and mixed
   directions on the same ellipse, so their spacing drifted and they bunched at
   one point every time the periods came back into step. Equal periods cannot
   drift. Do not "vary" the durations again; vary the phases instead. */
const ORBIT_PERIOD = 60;
const ORBIT_RING = 'M5 14a18 7 0 1 0 36 0a18 7 0 1 0-36 0';
const ORBIT_RING_TIGHT = 'M6 14a17 6 0 1 0 34 0a17 6 0 1 0-34 0';
const ORBIT_RING_WIDE = 'M8 14a15 9 0 1 0 30 0a15 9 0 1 0-30 0';
const ORBIT_GROUPS = [
  { tilt:-20, path:ORBIT_RING, sats:[['gps',0],['sbas',20],['gps',40]] },
  { tilt:35, path:ORBIT_RING, sats:[['gal',10],['glo',30],['gal',50]] },
  { tilt:82, path:ORBIT_RING_TIGHT, sats:[['bds',5],['qzss',35]] },
  { tilt:-70, path:ORBIT_RING_WIDE, sats:[['navic',25],['gps',55]] },
];

function Brand() {
  const markRef=useRef(null);
  useEffect(()=>{
    /* SMIL starts on its own, but an element mounted after page load can be
       left at the start of its cycle; beginning it at minus its phase puts it
       where it belongs relative to the others rather than collapsing every
       satellite onto the same point, which a plain beginElement() does. */
    const frame=requestAnimationFrame(()=>{
      if(!markRef.current)return;
      markRef.current.querySelectorAll('animateMotion').forEach(animation=>{
        const phase=Number(animation.dataset.phase||0);
        try{ animation.beginElementAt(-phase); }catch(e){ /* left to the declarative begin */ }
      });
    });
    return()=>cancelAnimationFrame(frame);
  },[]);
  return html`<div class="brand" aria-label="PSGNSSB">
    <span class="brand-word">PSGNSSB</span>
    <svg class="brand-mark" ref=${markRef} viewBox="0 0 46 28" role="img" aria-label=${t("GNSS orbital mark")}>
      <ellipse cx="23" cy="14" rx="18" ry="7" transform="rotate(-20 23 14)"></ellipse>
      <ellipse cx="23" cy="14" rx="18" ry="7" transform="rotate(35 23 14)"></ellipse>
      <ellipse cx="23" cy="14" rx="17" ry="6" transform="rotate(82 23 14)"></ellipse>
      <ellipse cx="23" cy="14" rx="15" ry="9" transform="rotate(-70 23 14)"></ellipse>
      <circle class="brand-core" cx="23" cy="14" r="3.6"></circle>
      ${ORBIT_GROUPS.map(g=>html`<g key=${g.tilt} transform=${'rotate('+g.tilt+' 23 14)'}>
        ${g.sats.map(([system,phase])=>html`<circle key=${system+phase} class=${'brand-sat '+system} r="1.35">
          <animateMotion data-phase=${phase} dur=${ORBIT_PERIOD+"s"} begin=${phase?"-"+phase+"s":"0s"}
            calcMode="linear" repeatCount="indefinite" path=${g.path}></animateMotion></circle>`)}
      </g>`)}
      <path d="M23 9.2v-4M18.2 14h-4M27.8 14h4"></path>
    </svg>
  </div>`;
}

const PAGE_TITLES = { dashboard:'Dashboard', history:'Data History', files:'File download',
                      operations:'Diagnostics', users:'Users', settings:'Settings' };

function PageHelp({ tab }) {
  const rows=PAGE_HELP[tab]||[];
  return html`<div class="page-help">
    <button class="icon-button info-button" aria-label=${t('Information about {tab}',{tab:t(PAGE_TITLES[tab]||tab)})} title=${t("Page information")}>i</button>
    <div class="help-popover" role="tooltip"><h2>${t(PAGE_TITLES[tab]||tab)}</h2>
      <dl>${rows.map(([term,description])=>html`<div key=${term}><dt>${t(term)}</dt><dd>${t(description)}</dd></div>`)}</dl>
    </div>
  </div>`;
}

const PUBLIC_TABS = { dashboard: 'Dashboard', history: 'Data History', files: 'File download' };
const ADMIN_TABS = { users: 'Users', operations: 'Diagnostics' };

function App() {
  // admin === null means "not asked yet"; false is a visitor, not a failure.
  const [admin, setAdmin] = useState(null);
  const [tab, setTab] = useState('dashboard');
  const [live, setLive] = useState(null);
  const [signin, setSignin] = useState(false);
  const [, setLangState] = useState(LANG);
  const timer = useRef(null);

  const check = useCallback(async () => {
    try { const me = await api('/api/me'); setAdmin(!!me.admin); }
    catch { setAdmin(false); }
  }, []);
  useEffect(() => { check(); }, [check]);

  /* A session can expire while the page is open. That demotes the page to the
     public view -- it never blanks it. */
  const demote = useCallback(() => { setAdmin(false); setTab(t => PUBLIC_TABS[t] ? t : 'dashboard'); }, []);

  useEffect(() => {
    if (admin === null) return;
    const go = () => { if (document.hidden) return; api('/api/live').then(setLive).catch(e => { if (e.unauth) demote(); }); };
    go();
    timer.current = setInterval(go, tab === 'dashboard' || tab === 'files' ? 2000 : 10000);
    return () => clearInterval(timer.current);
  }, [admin,tab,demote]);

  if (admin === null) return html`<div class="wrap"><p class="muted">${t("Loading…")}</p></div>`;

  const tabs = admin ? { ...PUBLIC_TABS, ...ADMIN_TABS } : PUBLIC_TABS;
  const shown = tabs[tab] || (admin && tab === 'settings') ? tab : 'dashboard';
  return html`
    <header>
      <${Brand}/>
      ${live ? html`<span class="badge">${t('{n} tracked',{n:trackedSatelliteCount(live)})}</span>` : null}
      ${live ? html`<span class="badge">${live.ntrip_clients} NTRIP</span>` : null}
      <span class="sp"></span>
      ${live ? html`<span class="muted mono header-version">${displayVersion(live.version)}</span>` : null}
      <${PageHelp} tab=${shown}/>
      <${LangButton} onChange=${setLangState}/>
      <${ThemeButton}/>
      ${admin ? html`<button class=${'icon-button settings-button '+(shown==='settings'?'on':'')} title=${t("Settings")} aria-label=${t("Settings")} onClick=${()=>setTab('settings')}>
        <svg viewBox="0 0 24 24" aria-hidden="true"><circle cx="12" cy="12" r="3"></circle><path d="M12.22 2h-.44a2 2 0 0 0-2 2v.18a2 2 0 0 1-1 1.73l-.43.25a2 2 0 0 1-2 0l-.15-.08a2 2 0 0 0-2.73.73l-.22.38a2 2 0 0 0 .73 2.73l.15.09a2 2 0 0 1 1 1.74v.5a2 2 0 0 1-1 1.74l-.15.09a2 2 0 0 0-.73 2.73l.22.38a2 2 0 0 0 2.73.73l.15-.08a2 2 0 0 1 2 0l.43.25a2 2 0 0 1 1 1.73V20a2 2 0 0 0 2 2h.44a2 2 0 0 0 2-2v-.18a2 2 0 0 1 1-1.73l.43-.25a2 2 0 0 1 2 0l.15.08a2 2 0 0 0 2.73-.73l.22-.38a2 2 0 0 0-.73-2.73l-.15-.09a2 2 0 0 1-1-1.74v-.5a2 2 0 0 1 1-1.74l.15-.09a2 2 0 0 0 .73-2.73l-.22-.38a2 2 0 0 0-2.73-.73l-.15.08a2 2 0 0 1-2 0l-.43-.25a2 2 0 0 1-1-1.73V4a2 2 0 0 0-2-2Z"></path></svg>
      </button>` : null}
      ${admin
        ? html`<button class="btn act" onClick=${async () => { await api('/api/logout', { method: 'POST' }).catch(()=>{}); demote(); }}>${t("Sign out")}</button>`
        : html`<button class="btn act" onClick=${() => setSignin(true)}>${t("Sign in")}</button>`}
    </header>
    <nav>${Object.entries(tabs).map(([k, v]) => html`
      <button key=${k} class=${shown === k ? 'on' : ''} onClick=${() => setTab(k)}>${t(v)}</button>`)}
    </nav>
    <div class="wrap">
      ${shown === 'dashboard' ? html`<${Dashboard} live=${live}/>` : null}
      ${shown === 'history' ? html`<${History}/>` : null}
      ${shown === 'files' ? html`<${FileDownload} live=${live}/>` : null}
      ${admin && shown === 'settings' ? html`<${Settings}/>` : null}
      ${admin && shown === 'operations' ? html`<${Operations}/>` : null}
      ${admin && shown === 'users' ? html`<${UsersPage}/>` : null}
    </div>
    ${signin ? html`<${Login} onClose=${() => setSignin(false)}
      onDone=${() => { setSignin(false); check(); }}/>` : null}`;
}

class UIErrorBoundary extends React.Component {
  constructor(props) { super(props); this.state = {error:null}; }
  static getDerivedStateFromError(error) { return {error}; }
  componentDidCatch(error, info) { console.error('PSGNSSB UI render failed', error, info); }
  render() {
    if (!this.state.error) return this.props.children;
    return html`<div class="login"><div class="card error-card"><h2>${t("Dashboard rendering error")}</h2>
      <p class="err">${this.state.error.message || String(this.state.error)}</p>
      <p class="muted">${t("Reload to fetch the current application assets.")}</p>
      <button class="btn act" onClick=${() => window.location.reload()}>${t("Reload dashboard")}</button>
    </div></div>`;
  }
}

setLang(LANG);
ReactDOM.createRoot(document.getElementById('root')).render(html`<${UIErrorBoundary}><${App}/><//>`);
