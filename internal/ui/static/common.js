// Shared by the owner page (app.js) and the reviewer page (review.js):
// the API request helper, the DOM builder and the full-screen viewer.
// Loaded first; top-level declarations here are visible to the page
// script that follows.
"use strict";

// --- API ---

// HTTPError is a non-2xx API response; status is the HTTP status code.
class HTTPError extends Error {
  constructor(status, message) {
    super(message);
    this.status = status;
  }
}

// Unauthorized is a 401: no valid session for this page's role.
class Unauthorized extends HTTPError {}

// request sends a same-origin request (session cookies go along
// automatically) and returns the decoded JSON body, or null for 204.
// opts.keepalive lets the request outlive the page (for saves on close).
async function request(method, path, body, opts = {}) {
  const init = { method, cache: "no-store", keepalive: !!opts.keepalive };
  if (body !== undefined) {
    init.headers = { "Content-Type": "application/json" };
    init.body = JSON.stringify(body);
  }
  const resp = await fetch(path, init);
  if (resp.status === 204) return null;
  const data = await resp.json().catch(() => ({}));
  if (resp.status === 401) throw new Unauthorized(401, data.error || "unauthorized");
  if (!resp.ok) throw new HTTPError(resp.status, data.error || `HTTP ${resp.status}`);
  return data;
}

// --- DOM ---

// h("a", {href: "#"}, "text", childNode) builds an element. Strings become
// text nodes (never parsed as HTML); attributes starting with "on" become
// event listeners; null/false attributes and null children are skipped.
function h(tag, attrs = {}, ...children) {
  const el = document.createElement(tag);
  for (const [name, value] of Object.entries(attrs)) {
    if (value == null || value === false) continue;
    if (name.startsWith("on")) el.addEventListener(name.slice(2), value);
    else el.setAttribute(name, value === true ? "" : value);
  }
  el.append(...children.filter((c) => c != null));
  return el;
}

// fill replaces el's children, skipping null like h does. Use it instead
// of el.replaceChildren, which renders null as the text "null".
function fill(el, ...children) {
  el.replaceChildren(...children.filter((c) => c != null));
  return el;
}

// --- Full-screen viewer ---

// isComposing is set by pages with text inputs while an IME (e.g. Japanese
// kana-to-kanji conversion) is composing, so viewer shortcuts stay out of
// the way. KeyboardEvent.isComposing covers most cases; this flag covers
// browsers that report Escape/arrow keys with it unset.
let isComposing = false;
document.addEventListener("compositionstart", () => { isComposing = true; });
document.addEventListener("compositionend", () => { isComposing = false; });

// Viewer swiping.
const SLIDE_GAP = 0.125; // gap between photos, as a fraction of the viewer width
const SWIPE_SLOP = 10; // px moved before deciding horizontal vs vertical
const FLICK_SPEED = 0.4; // px/ms over the last FLICK_WINDOW: a flick changes photo
const FLICK_MIN = 20; // px: shorter flicks are taps that wobbled
const FLICK_WINDOW = 100; // ms of movement that counts towards a flick's speed
// The track glides into place like a critically damped spring with this
// rate (1/ms): it carries on at the finger's speed and eases out, mostly
// there after ~0.35 s and fully settled after ~0.6 s. Lower is slower.
const SPRING = 0.015;
const WHEEL_IDLE = 150; // ms without wheel events that ends a trackpad swipe

// makeViewer wires up the page's <dialog id="viewer"> markup. caption(photo,
// index) returns the text shown (and used as alt text) for a photo; photos
// are objects with images.preview. onShow(photo, index), if given, runs
// after a photo is shown (e.g. to render its feedback controls).
//
// The photos sit side by side on a track, like a phone's photo app: the
// previous and next photos are loaded off screen, one SLIDE_GAP apart, and
// slide in as the reviewer drags (touch or mouse) or swipes a trackpad.
// Letting go with the incoming photo covering more of the screen than the
// outgoing one, or flicking, moves on; otherwise the track springs back.
// The ‹ › buttons and arrow keys glide the same way.
//
// Moving on happens the moment the finger lifts (caption, counter and
// feedback panel update at once); the track then glides the rest of the
// way. The glide can be grabbed and dragged from wherever it is.
function makeViewer(caption, onShow) {
  const dialog = document.getElementById("viewer");
  const body = dialog.querySelector(".viewer-body");
  const track = document.getElementById("viewer-track");
  const name = document.getElementById("viewer-name");
  const pos = document.getElementById("viewer-pos");
  const prev = document.getElementById("viewer-prev");
  const next = document.getElementById("viewer-next");
  const close = document.getElementById("viewer-close");
  let slides = [...track.querySelectorAll("img")]; // [previous, current, next]
  let photos = [];
  let index = 0;

  for (const s of slides) {
    s.draggable = false; // no native image drag on mouse drag
    s.addEventListener("load", () => {
      s.dataset.ratio = s.naturalWidth / s.naturalHeight;
      layout();
    });
  }

  // load points slide at photo p (or empties it), without reloading an
  // image it already shows.
  function load(slide, p) {
    const url = p ? p.images.preview : "";
    if (slide.dataset.url === url) return;
    slide.dataset.url = url;
    delete slide.dataset.ratio; // known once the new image loads
    slide.hidden = !url;
    if (url) slide.src = url;
    else slide.removeAttribute("src");
  }

  function show(i) {
    index = i;
    const p = photos[i];
    const text = caption(p, i);
    load(slides[0], photos[i - 1]);
    load(slides[1], p);
    load(slides[2], photos[i + 1]);
    slides.forEach((s, k) => {
      s.alt = k === 1 ? text : "";
      s.setAttribute("aria-hidden", k === 1 ? "false" : "true");
    });
    name.textContent = text;
    pos.textContent = `${i + 1} / ${photos.length}`;
    prev.disabled = i === 0;
    next.disabled = i === photos.length - 1;
    if (onShow) onShow(p, i);
    layout(); // after onShow, which can change the photo area's height
  }

  // --- Track movement ---

  // dir is -1 (previous photo), 0 (stay) or 1 (next photo). allowed turns
  // a direction with no photo in it into 0.
  const allowed = (dir) => (dir < 0 && index === 0) || (dir > 0 && index === photos.length - 1) ? 0 : dir;

  // shownWidth is how wide slide's photo is drawn (object-fit: contain
  // letterboxes it); until it has loaded, assume the full width.
  function shownWidth(slide) {
    const r = Number(slide.dataset.ratio);
    return r ? Math.min(body.clientWidth, body.clientHeight * r) : body.clientWidth;
  }

  // apart(dir) is how far the neighbour in direction dir sits from the
  // current photo (centre to centre). It's measured between the photos
  // themselves, not their slides: on a wide screen a photo has black bars
  // beside it, and spacing whole slides would hide the next photo behind
  // them until most of a drag had gone by. So photos are SLIDE_GAP apart,
  // or, if that would let a narrow neighbour peek in at rest, just off the
  // screen edge (it slides in as soon as a drag starts).
  function apart(dir) {
    const w = body.clientWidth;
    const other = shownWidth(slides[dir > 0 ? 2 : 0]);
    return Math.max((shownWidth(slides[1]) + other) / 2 + SLIDE_GAP * w, (w + other) / 2);
  }

  function layout() {
    slides[0].style.transform = `translateX(${-apart(-1)}px)`;
    slides[1].style.transform = "";
    slides[2].style.transform = `translateX(${apart(1)}px)`;
  }
  new ResizeObserver(layout).observe(body);

  // offset is how far (px) the track is moved right of the current photo:
  // negative shows some of the next photo, positive some of the previous.
  let offset = 0;
  let glide = null; // {start, c1, c2, frame} while gliding back to offset 0

  function setOffset(px) {
    offset = Math.max(-apart(1), Math.min(apart(-1), px));
    track.style.transform = offset ? `translateX(${offset}px)` : "";
  }

  // Critically damped spring towards 0: x(t) = (c1 + c2·t)·e^(−SPRING·t),
  // with x(0) = offset and x'(0) = v (px/ms).
  function glideHome(v = 0) {
    stopGlide();
    if (offset === 0 && v === 0) return;
    const c1 = offset;
    // Cap the speed towards home so the spring doesn't overshoot past it.
    if (Math.sign(v) === -Math.sign(c1)) v = Math.sign(v) * Math.min(Math.abs(v), SPRING * Math.abs(c1));
    const g = { start: performance.now(), c1, c2: v + SPRING * c1, frame: 0 };
    const step = (now) => {
      const t = Math.max(0, now - g.start);
      const x = (g.c1 + g.c2 * t) * Math.exp(-SPRING * t);
      if (Math.abs(x) < 0.5 && Math.abs(glideSpeed(g, t)) < 0.02) {
        glide = null;
        setOffset(0);
        return;
      }
      setOffset(x);
      g.frame = requestAnimationFrame(step);
    };
    glide = g;
    g.frame = requestAnimationFrame(step);
  }
  const glideSpeed = (g, t) => (g.c2 - SPRING * (g.c1 + g.c2 * t)) * Math.exp(-SPRING * t);

  // stopGlide freezes the track where it is and returns its speed then.
  function stopGlide() {
    if (!glide) return 0;
    cancelAnimationFrame(glide.frame);
    const v = glideSpeed(glide, performance.now() - glide.start);
    glide = null;
    return v;
  }

  // go moves to photo dir now and glides the track the rest of the way,
  // starting at speed v. Slides are recycled, so the photos already on
  // screen aren't reloaded, and shifting offset by the distance between the
  // two photos keeps every photo exactly where it was.
  function go(dir, v = 0) {
    dir = allowed(dir);
    if (dir) {
      const d = apart(dir);
      if (dir > 0) slides.push(slides.shift()); // old current becomes previous
      if (dir < 0) slides.unshift(slides.pop()); // old current becomes next
      show(index + dir);
      setOffset(offset + dir * d);
    }
    glideHome(v);
  }

  // release ends a drag or trackpad swipe at speed v (px/ms, negative:
  // towards the next photo).
  function release(v) {
    const half = apart(-Math.sign(offset)) / 2;
    let dir = 0;
    if (Math.abs(v) >= FLICK_SPEED && Math.abs(offset) >= FLICK_MIN && Math.sign(v) === Math.sign(offset)) {
      dir = -Math.sign(offset); // flicked onwards (a flick back just returns)
    } else if (Math.abs(offset) > half && Math.abs(v) < FLICK_SPEED) {
      dir = -Math.sign(offset); // the incoming photo has most of the screen
    }
    go(dir, v);
  }

  // follow moves the track to px for a drag, resisting (a third as far)
  // where there's no photo to move to.
  function follow(px) {
    setOffset(allowed(-Math.sign(px)) ? px : px / 3);
  }

  // --- Pinch-zoom ---

  const zoomed = () => window.visualViewport && window.visualViewport.scale > 1.01;

  // touch-action decides which gestures the browser keeps for itself. At
  // normal zoom, horizontal drags must come to us (pan-y pinch-zoom: the
  // browser keeps vertical panning and pinch). Once pinch-zoomed in, the
  // reviewer needs one-finger panning to look around the photo, so hand
  // everything back to the browser (and swipes are ignored, see zoomed()).
  // iOS Safari doesn't know "pinch-zoom" and would drop the whole value,
  // so fall back to plain pan-y there.
  const normal = CSS.supports("touch-action", "pan-y pinch-zoom") ? "pan-y pinch-zoom" : "pan-y";
  function updateTouchAction() {
    body.style.touchAction = zoomed() ? "auto" : normal;
  }
  updateTouchAction();
  if (window.visualViewport) window.visualViewport.addEventListener("resize", updateTouchAction);

  // --- Touch and mouse drags ---

  let drag = null; // {id, x, y, base, horizontal, moves: [{t, x}]}
  const down = new Set(); // pointers currently touching: 2+ means a pinch
  let dragged = false; // a drag just ended: swallow the click it makes

  body.addEventListener("pointerdown", (e) => {
    down.add(e.pointerId);
    if (down.size > 1) {
      // A second finger: this is a pinch, not a swipe.
      if (drag) {
        drag = null;
        glideHome();
      }
      return;
    }
    // Drags may start on the ‹ › buttons: they cover the screen edges,
    // where a thumb swipe usually starts.
    if (e.button !== 0 || zoomed()) return;
    stopGlide(); // caught mid-glide: drag on from there
    drag = {
      id: e.pointerId, x: e.clientX, y: e.clientY, base: offset, horizontal: null,
      moves: [{ t: e.timeStamp, x: e.clientX }],
    };
  });

  body.addEventListener("pointermove", (e) => {
    if (!drag || e.pointerId !== drag.id) return;
    const dx = e.clientX - drag.x;
    const dy = e.clientY - drag.y;
    if (drag.horizontal === null) {
      if (Math.hypot(dx, dy) < SWIPE_SLOP) return;
      drag.horizontal = Math.abs(dx) > Math.abs(dy);
      if (!drag.horizontal) {
        drag = null; // vertical: not ours
        glideHome(); // carry on with any glide this touch interrupted
        return;
      }
      body.setPointerCapture(e.pointerId); // keep getting moves off the photo
    }
    drag.moves.push({ t: e.timeStamp, x: e.clientX });
    while (drag.moves.length > 2 && e.timeStamp - drag.moves[0].t > FLICK_WINDOW) drag.moves.shift();
    follow(drag.base + dx);
  });

  function end(e) {
    down.delete(e.pointerId);
    if (!drag || e.pointerId !== drag.id) return;
    const d = drag;
    drag = null;
    if (!d.horizontal) {
      glideHome(); // a tap: leave clicks alone, but carry on with an interrupted glide
      return;
    }
    dragged = true;
    setTimeout(() => { dragged = false; }, 0);
    if (e.type === "pointercancel") {
      glideHome(); // the browser took over: settle on the current photo
      return;
    }
    const first = d.moves[0];
    release((e.clientX - first.x) / Math.max(1, e.timeStamp - first.t));
  }
  body.addEventListener("pointerup", end);
  body.addEventListener("pointercancel", end);
  // A finger can also lift outside the area (no capture after a vertical
  // drag); forget it there too, or the next swipe would look like a pinch.
  window.addEventListener("pointerup", (e) => down.delete(e.pointerId));
  window.addEventListener("pointercancel", (e) => down.delete(e.pointerId));
  // A drag that started on ‹ or › isn't a press of it.
  body.addEventListener("click", (e) => {
    if (!dragged) return;
    e.stopPropagation();
    e.preventDefault();
  }, true);

  // --- Trackpad ---

  // Trackpads report two-finger swipes as horizontal wheel events, which
  // keep coming for a while after the fingers lift (momentum). The track
  // follows them; the swipe ends when they stop, or once a whole photo has
  // gone by, after which the rest of the gesture is ignored.
  let wheel = null; // {base, dx} for the current swipe
  let wheelLocked = false;
  let wheelIdle = null;
  body.addEventListener("wheel", (e) => {
    if (Math.abs(e.deltaX) <= Math.abs(e.deltaY)) return; // vertical scroll
    if (zoomed()) return; // panning around a zoomed-in photo
    e.preventDefault(); // otherwise the browser's back/forward gesture
    clearTimeout(wheelIdle);
    wheelIdle = setTimeout(() => {
      wheelLocked = false;
      if (wheel) {
        wheel = null;
        release(0);
      }
    }, WHEEL_IDLE);
    if (wheelLocked) return;
    if (!wheel) {
      stopGlide();
      wheel = { base: offset, dx: 0 };
    }
    wheel.dx -= e.deltaMode === 1 ? e.deltaX * 16 : e.deltaX; // 1: lines (Firefox)
    follow(wheel.base + wheel.dx);
    const at = wheel.base + wheel.dx;
    if (at !== 0 && Math.abs(at) >= apart(-Math.sign(at))) {
      wheel = null;
      wheelLocked = true;
      release(0);
    }
  }, { passive: false });

  // --- Buttons and keys ---

  // Pressing again mid-glide carries on from where the track is.
  const stepTo = (dir) => go(dir, stopGlide());
  prev.addEventListener("click", () => stepTo(-1));
  next.addEventListener("click", () => stepTo(1));
  close.addEventListener("click", () => dialog.close());
  dialog.addEventListener("keydown", (e) => {
    // Arrow keys move the text cursor while typing, not the photo.
    const typing = e.target.matches("input, textarea, select");
    if (typing || e.isComposing || isComposing) return;
    if (e.key === "ArrowLeft") stepTo(-1);
    if (e.key === "ArrowRight") stepTo(1);
  });
  // Escape closes the dialog, except while an IME is composing: there it
  // cancels the conversion.
  dialog.addEventListener("cancel", (e) => {
    if (isComposing) e.preventDefault();
  });
  dialog.addEventListener("close", () => {
    stopGlide();
    drag = null;
    wheel = null;
    setOffset(0);
    for (const s of slides) load(s, null);
  });

  return {
    open(list, i) {
      photos = list;
      stopGlide();
      setOffset(0);
      show(i);
      dialog.showModal();
    },
    close() {
      if (dialog.open) dialog.close();
    },
    // current returns the photo on screen, or null when closed.
    current() {
      return dialog.open ? photos[index] : null;
    },
    // setLabels translates the dialog's own controls.
    setLabels({ close: closeText, prev: prevLabel, next: nextLabel, dialog: dialogLabel }) {
      close.textContent = closeText;
      close.setAttribute("aria-label", closeText);
      prev.setAttribute("aria-label", prevLabel);
      next.setAttribute("aria-label", nextLabel);
      dialog.setAttribute("aria-label", dialogLabel);
      if (dialog.open) show(index); // re-caption the current photo
    },
  };
}
