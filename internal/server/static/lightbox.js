// lightbox.js — the image lightbox: one persistent overlay paging through a
// message's thumbnails (RFC lightbox-gallery-nav). A leaf: dashboard's module
// body runs initLightbox(), which builds the overlay and adds its listeners.
import { ICONS } from './icons.js';

// lb: the overlay's elements (filled by initLightbox) and its view state.
const lb = {
  ov: null, img: null, hint: null, counter: null, prevBtn: null, nextBtn: null,
  scale: 1, panX: 0, panY: 0, rotation: 0, dragging: false, lx: 0, ly: 0, ht: null, rotateAnimTimer: null,
  // Gallery group state: items is a click-time snapshot of {full, thumb} URL
  // strings (no DOM references), so the poll-driven innerHTML re-renders that
  // destroy the thumbnails cannot invalidate an open lightbox.
  items: [], idx: 0, lastFocus: null,
  // preloaded dedupes warm-up requests across fast paging within one open
  // gallery; the Image objects themselves are throwaway — the browser HTTP
  // cache holds the bytes. Reset per openLightboxGroup: attachment URLs carry
  // a ?v=<time> cache-buster, so a session-lifetime map would only grow.
  preloaded: {},
  // Touch gesture state; see the arbitration rules above onTouchEnd.
  iDist: 0, iScale: 1, lastTap: 0, sx: 0, sy: 0, swipeScale: 1, pinched: false, swipeAt: 0,
};
function showHint(text){lb.hint.textContent=text||(Math.round(lb.scale*100)+'%');lb.hint.classList.add('visible');clearTimeout(lb.ht);lb.ht=setTimeout(function(){lb.hint.classList.remove('visible')},1200)}
function apply(){
  // Rotation always emits a transform — even at neutral pan/scale — because
  // resetting transform to '' would visibly snap the image back. Order
  // matters: translate → scale → rotate keeps panning intuitive (drag in
  // screen-space, not image-space).
  var neutral=lb.scale===1&&!lb.panX&&!lb.panY&&lb.rotation===0;
  lb.img.style.transform=neutral?'':'translate('+lb.panX+'px,'+lb.panY+'px) scale('+lb.scale+') rotate('+lb.rotation+'deg)';
  lb.ov.classList.toggle('zoomed',lb.scale>1);
}
function reset(){lb.scale=1;lb.panX=0;lb.panY=0;lb.rotation=0;lb.dragging=false;lb.img.style.transform='';lb.img.classList.remove('lb-rotating');lb.ov.classList.remove('zoomed','dragging');lb.hint.classList.remove('visible');clearTimeout(lb.ht);clearTimeout(lb.rotateAnimTimer)}
function closeLightbox(){
  lb.ov.classList.remove('active');reset();
  // Return focus to wherever the user was before the lightbox grabbed it,
  // but only if focus is still inside the overlay — if the user already
  // clicked elsewhere, stealing focus back would be hostile.
  if(lb.lastFocus&&lb.ov.contains(document.activeElement)){try{lb.lastFocus.focus()}catch(_){/* detached node */}}
  lb.lastFocus=null;
}
function zoomBy(f){lb.scale=Math.min(Math.max(lb.scale*f,.5),10);apply();showHint()}
// ── Gallery group navigation (RFC lightbox-gallery-nav §3) ──
function preload(i){
  if(i<0||i>=lb.items.length)return;
  var u=lb.items[i].full;
  if(!u||lb.preloaded[u])return;
  lb.preloaded[u]=1;
  var im=new Image();
  // Swallow 404s (GC-expired attachments): a failed warm-up must not
  // surface through the RNEW-UX-002 global error handler as a toast.
  im.onerror=function(){};
  im.src=u;
}
function updateNav(){
  var multi=lb.items.length>1;
  lb.ov.classList.toggle('lb-single',!multi);
  if(!multi)return;
  lb.counter.textContent=(lb.idx+1)+' / '+lb.items.length;
  // aria-disabled (not the disabled attribute) keeps boundary buttons in
  // the tab order so screen-reader users can perceive the edge.
  lb.prevBtn.setAttribute('aria-disabled',lb.idx<=0?'true':'false');
  lb.nextBtn.setAttribute('aria-disabled',lb.idx>=lb.items.length-1?'true':'false');
}
// loadWithFallback(item) loads item.full and silently degrades to item.thumb,
// the thumbnail data URI persisted alongside it, when the attachment-GC'd
// original is gone (RFC §3.6.3); without it the user sees a broken-image glyph.
// Two failure modes: an HTTP 404 / network error fires onerror, but an HTTP 200
// with a wrong Content-Type or corrupt body does not on every browser, so
// onload also treats naturalWidth === 0 as a failure. The img element is
// reused, so its handlers are re-assigned (not addEventListener'd) to keep
// stale listeners from piling up across opens.
function loadWithFallback(item){
  var img=lb.img,src=item.full,fallback=item.thumb,primaryTried=false;
  function useFallback(){
    if(!fallback||fallback===src)return false;
    // Guard against infinite recursion if the fallback itself 404s.
    img.onerror=function(){img.onerror=null;img.onload=null};
    img.onload=function(){img.onerror=null;img.onload=null};
    img.src=fallback;
    return true;
  }
  img.onerror=function(){
    img.onerror=null;
    if(!useFallback())img.onload=null;
  };
  img.onload=function(){
    if(!primaryTried){
      primaryTried=true;
      // Loaded (no onerror) but decoded to nothing, usually a Content-Type
      // Chrome refuses to render as an image: fall back as for onerror.
      if(img.naturalWidth===0&&useFallback())return;
    }
    img.onerror=null;img.onload=null;
  };
  img.src=src;
}
function show(i,dir){
  if(i<0||i>=lb.items.length)return;
  lb.idx=i;
  // Zoom/pan/rotation reset on every page turn — carrying the previous
  // image's pan could push the next one fully off-screen.
  reset();
  loadWithFallback(lb.items[lb.idx]);
  updateNav();
  preload(lb.idx+(dir||1));
}
function nav(dir){show(lb.idx+dir,dir)}
function rotateBy(deg){
  // Accumulate the raw angle without normalization so the CSS transition
  // always rotates the visually shorter 90° path. If we wrapped to
  // (-180, 180] the browser would interpolate a 270° spin in the wrong
  // direction (e.g. -180 → +180 renders as +360 of CW spin).
  lb.rotation+=deg;
  lb.img.classList.add('lb-rotating');
  apply();
  // Display label normalized to (-180, 180] so the hint stays human-readable
  // ("90°" beats "450°"). The stored `rotation` keeps growing.
  var disp=((lb.rotation%360)+360)%360;
  if(disp>180)disp-=360;
  showHint(disp+'°');
  clearTimeout(lb.rotateAnimTimer);
  // Strip the transition class once the animation settles so subsequent
  // pan/zoom interactions stay snappy (no easing on every drag frame).
  lb.rotateAnimTimer=setTimeout(function(){lb.img.classList.remove('lb-rotating')},280);
}
function onOverlayClick(e){
  // A nav swipe's browser-synthesized click can land anywhere on the overlay:
  // the backdrop (would close) or a nav rail under the lifted finger (would
  // double-navigate), so ANY click within 500ms of one is swallowed. A
  // timestamp, not a flag: preventDefault in touchend suppresses the synthetic
  // click on most engines, and a leftover flag would eat the NEXT real click.
  if(Date.now()-lb.swipeAt<500){lb.swipeAt=0;return}
  var btn=e.target&&e.target.closest&&e.target.closest('[data-lb-action]');
  if(btn){
    // Toolbar/nav click — handle action and don't fall through to backdrop close.
    // prev/next respect aria-disabled (ARIA 1.2 §6.6.3): pointer-events:none
    // blocks mouse, but the buttons stay focusable, so Enter would otherwise
    // activate a control that announces itself as disabled.
    var action=btn.getAttribute('data-lb-action');
    var dis=btn.getAttribute('aria-disabled')==='true';
    if(action==='rotate-left')rotateBy(-90);
    else if(action==='rotate-right')rotateBy(90);
    else if(action==='zoom-in')zoomBy(1.2);
    else if(action==='zoom-out')zoomBy(1/1.2);
    else if(action==='prev'&&!dis)nav(-1);
    else if(action==='next'&&!dis)nav(1);
    return;
  }
  if(e.target===lb.ov)closeLightbox();
}
// Touch: pinch zoom + drag pan + double-tap + horizontal swipe nav.
// Gesture arbitration (RFC lightbox-gallery-nav §3):
//   - swipeScale is captured at touchSTART. A pinch leaves `scale` at an
//     arbitrary float when the second finger lifts, so reading it at
//     touchend would misclassify a pinch-then-swipe as a pan.
//   - `pinched` marks any gesture that ever had two fingers down; such a
//     gesture can never end as a navigation swipe.
//   - scale < 1.05 (tolerance, not ===1): swipe navigates. Above it the
//     single finger pans the zoomed image — mutually exclusive paths.
function t2d(t){return Math.hypot(t[1].clientX-t[0].clientX,t[1].clientY-t[0].clientY)}
function onTouchEnd(e){
  if(e.touches.length<2)lb.iDist=0;
  if(e.touches.length!==0)return;
  lb.dragging=false;
  if(e.changedTouches.length!==1)return;
  var t=e.changedTouches[0],dx=t.clientX-lb.sx,dy=t.clientY-lb.sy;
  if(!lb.pinched&&lb.items.length>1&&lb.swipeScale<1.05&&Math.abs(dx)>50&&Math.abs(dx)>Math.abs(dy)*1.2){
    e.preventDefault();
    nav(dx<0?1:-1);
    // A nav swipe must not double as the first/second tap of a double-tap
    // zoom (rapid successive swipes land within the 300ms window), nor may
    // its synthesized click reach the overlay click handler (backdrop close
    // or a nav rail under the lifted finger).
    lb.lastTap=0;
    lb.swipeAt=Date.now();
    return;
  }
  var now=Date.now();
  if(now-lb.lastTap<300){e.preventDefault();if(lb.scale>1.05)reset();else lb.scale=2.5;apply();showHint()}
  lb.lastTap=now;
}
// openLightboxGroup(list, start) opens a gallery over `list`, an array of
// {full, thumb} URL-string snapshots, starting at index `start`. Single
// lightbox instance: calling while already open simply replaces the group.
function openLightboxGroup(list,start){
  lb.items=(list&&list.length)?list:[];
  if(!lb.items.length)return;
  lb.preloaded={};
  var wasOpen=lb.ov.classList.contains('active');
  show(Math.min(Math.max(start||0,0),lb.items.length-1));
  lb.ov.classList.add('active');
  if(!wasOpen){
    // Move focus onto the overlay so ←/→/Esc work immediately; remember
    // where it came from so closeLightbox() can restore it.
    lb.lastFocus=document.activeElement;
    try{lb.ov.focus()}catch(_){/* focus can throw on detached roots */}
  }
}
// openLightboxFromThumb(el) collects every sibling thumbnail inside the
// clicked .event-images container into a gallery group of dataset strings.
function openLightboxFromThumb(el){
  var box=el.closest&&el.closest('.event-images');
  var imgs=box?Array.prototype.slice.call(box.querySelectorAll('img[data-full]')):[el];
  var list=imgs.map(function(i){return{full:i.dataset.full||i.src,thumb:i.dataset.thumb||i.src}});
  // indexOf can only miss if `el` somehow lacks data-full (not rendered by
  // eventHtml); degrade to the first image rather than refusing to open.
  openLightboxGroup(list,Math.max(0,imgs.indexOf(el)));
}
function onKeydown(e){
  var ov=lb.ov;if(!ov.classList.contains('active'))return;
  // Skip shortcuts when an editable element holds focus — otherwise typing
  // 'r' in a chat input or stacked dialog would silently rotate the
  // backgrounded preview.
  var ae=document.activeElement;
  if(ae&&(ae.tagName==='INPUT'||ae.tagName==='TEXTAREA'||ae.isContentEditable)){
    if(e.key==='Escape')closeLightbox();
    return;
  }
  if(e.key==='Escape'){closeLightbox();return}
  // Tab containment: aria-modal alone isn't honored by every AT, and a
  // sighted keyboard user could Tab into the chat behind the overlay.
  // nz_util's trapFocus is unsuitable here — its Escape branch removes the
  // overlay element, but this lightbox is a persistent singleton.
  if(e.key==='Tab'){
    var nodes=Array.prototype.filter.call(ov.querySelectorAll('button'),function(n){return n.offsetParent!==null});
    if(!nodes.length){e.preventDefault();return}
    var first=nodes[0],last=nodes[nodes.length-1],cur=document.activeElement;
    if(e.shiftKey&&(cur===first||cur===ov)){e.preventDefault();last.focus()}
    else if(!e.shiftKey&&cur===last){e.preventDefault();first.focus()}
    else if(!ov.contains(cur)){e.preventDefault();first.focus()}
    return;
  }
  if(e.key==='ArrowLeft'){e.preventDefault();nav(-1);return}
  if(e.key==='ArrowRight'){e.preventDefault();nav(1);return}
  if(e.key==='+'||e.key==='='){lb.scale=Math.min(lb.scale*1.2,10);apply();showHint();return}
  if(e.key==='-'){lb.scale=Math.max(lb.scale/1.2,.5);apply();showHint();return}
  if(e.key==='0'){reset();apply();showHint();return}
  // Rotation shortcuts: r = CCW 90°, R / Shift+R = CW 90°. Match by lowercase
  // so the lightbox responds the same regardless of caps lock state.
  if(e.key==='r'||e.key==='R'){e.preventDefault();rotateBy(e.shiftKey?90:-90);return}
}
// initLightbox builds the overlay and adds its listeners, once, at dashboard load.
export function initLightbox() {
  var ov=document.createElement('div');ov.className='lightbox-overlay';
  ov.setAttribute('role','dialog');ov.setAttribute('aria-modal','true');ov.setAttribute('aria-label','Image preview');
  // tabindex=-1 lets openLightboxGroup move focus onto the overlay, so the
  // ←/→/r/+/- shortcuts work even when the click left focus in the chat
  // textarea (the keydown handler skips editable elements by design).
  ov.setAttribute('tabindex','-1');
  // Toolbar buttons sit absolute top-right; rotation buttons trigger 90° steps,
  // zoom buttons give pointer-only users (touch device + mouse without wheel)
  // a clickable zoom entry. The overlay's click-to-close handler ignores events
  // that didn't target the overlay itself, so toolbar clicks won't propagate up
  // and dismiss the modal. The prev/next rails + counter belong to the gallery
  // group model (RFC lightbox-gallery-nav): hidden via .lb-single when the
  // group holds one image.
  ov.innerHTML='<div class="lb-toolbar">'
    +'<button type="button" class="lb-tool-btn" data-lb-action="zoom-out" aria-label="Zoom out" title="缩小 (-)">' + ICONS.zoomOut + '</button>'
    +'<button type="button" class="lb-tool-btn" data-lb-action="zoom-in" aria-label="Zoom in" title="放大 (+)">' + ICONS.zoomIn + '</button>'
    +'<button type="button" class="lb-tool-btn" data-lb-action="rotate-left" aria-label="Rotate left" title="Rotate left (R)">' + ICONS.rotateLeft + '</button>'
    +'<button type="button" class="lb-tool-btn" data-lb-action="rotate-right" aria-label="Rotate right" title="Rotate right (Shift+R)">' + ICONS.rotateRight + '</button>'
    +'</div>'
    +'<button type="button" class="lb-nav lb-nav-prev" data-lb-action="prev" aria-label="上一张" title="上一张 (←)">' + ICONS.galleryPrev + '</button>'
    +'<button type="button" class="lb-nav lb-nav-next" data-lb-action="next" aria-label="下一张" title="下一张 (→)">' + ICONS.galleryNext + '</button>'
    +'<div class="lb-counter" aria-live="polite"></div>'
    +'<img alt=""><div class="lb-zoom-hint"></div>';
  document.body.appendChild(ov);
  var img=ov.querySelector('img');lb.ov=ov;lb.img=img;lb.hint=ov.querySelector('.lb-zoom-hint');
  lb.counter=ov.querySelector('.lb-counter');lb.prevBtn=ov.querySelector('.lb-nav-prev');lb.nextBtn=ov.querySelector('.lb-nav-next');
  ov.addEventListener('click',onOverlayClick);
  // Scroll wheel zoom (toward cursor)
  ov.addEventListener('wheel',function(e){e.preventDefault();var f=e.deltaY<0?1.15:1/1.15,ns=Math.min(Math.max(lb.scale*f,.5),10);var r=lb.img.getBoundingClientRect(),cx=e.clientX-(r.left+r.width/2),cy=e.clientY-(r.top+r.height/2);lb.panX-=cx*(ns/lb.scale-1);lb.panY-=cy*(ns/lb.scale-1);lb.scale=ns;apply();showHint()},{passive:false});
  // Mouse drag pan
  img.addEventListener('mousedown',function(e){if(lb.scale<=1)return;e.preventDefault();lb.dragging=true;lb.lx=e.clientX;lb.ly=e.clientY;ov.classList.add('dragging')});
  document.addEventListener('mousemove',function(e){if(!lb.dragging)return;lb.panX+=e.clientX-lb.lx;lb.panY+=e.clientY-lb.ly;lb.lx=e.clientX;lb.ly=e.clientY;apply()});
  document.addEventListener('mouseup',function(){if(lb.dragging){lb.dragging=false;ov.classList.remove('dragging')}});
  // Double-click toggle zoom
  img.addEventListener('dblclick',function(e){e.preventDefault();e.stopPropagation();if(lb.scale>1.05){reset();apply()}else{var r=lb.img.getBoundingClientRect(),cx=e.clientX-(r.left+r.width/2),cy=e.clientY-(r.top+r.height/2);lb.scale=2.5;lb.panX=-cx*1.5;lb.panY=-cy*1.5;apply()}showHint()});
  // Touch gestures: the arbitration rules are on onTouchEnd.
  img.addEventListener('touchstart',function(e){if(e.touches.length===2){e.preventDefault();lb.pinched=true;lb.iDist=t2d(e.touches);lb.iScale=lb.scale}else if(e.touches.length===1){lb.pinched=false;lb.sx=e.touches[0].clientX;lb.sy=e.touches[0].clientY;lb.swipeScale=lb.scale;if(lb.scale>1){lb.lx=lb.sx;lb.ly=lb.sy;lb.dragging=true}}},{passive:false});
  img.addEventListener('touchmove',function(e){if(e.touches.length===2&&lb.iDist){e.preventDefault();lb.scale=Math.min(Math.max(lb.iScale*(t2d(e.touches)/lb.iDist),.5),10);apply();showHint()}else if(e.touches.length===1&&lb.dragging){e.preventDefault();lb.panX+=e.touches[0].clientX-lb.lx;lb.panY+=e.touches[0].clientY-lb.ly;lb.lx=e.touches[0].clientX;lb.ly=e.touches[0].clientY;apply()}},{passive:false});
  img.addEventListener('touchend',onTouchEnd,{passive:false});
  // Delegated thumbnail click handler — registered once on document, so it
  // survives the chat transcript's innerHTML re-renders (eventHtml emits the
  // thumbnails without inline onclick; RFC lightbox-gallery-nav §3).
  document.addEventListener('click',function(e){
    var t=e.target&&e.target.closest&&e.target.closest('.event-images img[data-full]');
    if(t)openLightboxFromThumb(t);
  });
  document.addEventListener('keydown',onKeydown);
}
