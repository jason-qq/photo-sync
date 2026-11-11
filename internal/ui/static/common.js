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

// Swipe thresholds.
const SWIPE_DISTANCE = 60; // px: a drag this long changes photo
const SWIPE_FLICK = 0.4; // px/ms: a faster, shorter flick (≥ 20 px) also does
const SWIPE_SLOP = 10; // px moved before deciding horizontal vs vertical
const WHEEL_DISTANCE = 80; // accumulated trackpad deltaX that changes photo

// addSwipe lets the reviewer change photo by swiping the image area:
// touch, mouse drag, or a horizontal two-finger trackpad swipe. The image
// follows the finger. It stays out of the way of pinch-zoom (a second
// finger, or an already-zoomed page, cancels it), vertical drags, and the
// nav buttons. atStart/atEnd make the image resist at the first/last
// photo instead of moving freely.
function addSwipe(img, onPrev, onNext, atStart, atEnd) {
  const area = img.parentElement; // .viewer-body: image plus nav buttons
  img.draggable = false; // no native image drag-and-drop on mouse drag

  let drag = null; // {id, x, y, t, dx, horizontal}
  const down = new Set(); // pointers currently touching: 2+ means a pinch

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
    area.style.touchAction = zoomed() ? "auto" : normal;
  }
  updateTouchAction();
  if (window.visualViewport) window.visualViewport.addEventListener("resize", updateTouchAction);

  function settle(animate) {
    img.style.transition = animate ? "transform 0.2s ease-out" : "none";
    img.style.transform = "";
  }

  area.addEventListener("pointerdown", (e) => {
    down.add(e.pointerId);
    if (down.size > 1) {
      // A second finger: this is a pinch, not a swipe.
      if (drag) {
        drag = null;
        settle(true);
      }
      return;
    }
    if (e.button !== 0 || e.target.closest("button") || zoomed()) return;
    drag = { id: e.pointerId, x: e.clientX, y: e.clientY, t: e.timeStamp, dx: 0, horizontal: null };
  });

  area.addEventListener("pointermove", (e) => {
    if (!drag || e.pointerId !== drag.id) return;
    const dx = e.clientX - drag.x;
    const dy = e.clientY - drag.y;
    if (drag.horizontal === null) {
      if (Math.hypot(dx, dy) < SWIPE_SLOP) return;
      drag.horizontal = Math.abs(dx) > Math.abs(dy);
      if (!drag.horizontal) {
        drag = null; // vertical: not ours
        return;
      }
      area.setPointerCapture(e.pointerId); // keep getting moves off the image
    }
    // Follow the finger; resist (move a third as far) past the first/last.
    const blocked = (dx > 0 && atStart()) || (dx < 0 && atEnd());
    drag.dx = dx;
    img.style.transition = "none";
    img.style.transform = `translateX(${blocked ? dx / 3 : dx}px)`;
  });

  function end(e) {
    down.delete(e.pointerId);
    if (!drag || e.pointerId !== drag.id) return;
    const d = drag;
    drag = null;
    if (!d.horizontal || e.type === "pointercancel") {
      settle(true);
      return;
    }
    const dist = Math.abs(d.dx);
    const speed = dist / Math.max(1, e.timeStamp - d.t);
    const swiped = dist >= SWIPE_DISTANCE || (dist >= 20 && speed >= SWIPE_FLICK);
    if (swiped && d.dx < 0 && !atEnd()) {
      settle(false); // the next photo appears in place
      onNext();
    } else if (swiped && d.dx > 0 && !atStart()) {
      settle(false);
      onPrev();
    } else {
      settle(true); // spring back
    }
  }
  area.addEventListener("pointerup", end);
  area.addEventListener("pointercancel", end);
  // A finger can also lift outside the area (no capture after a vertical
  // drag); forget it there too, or the next swipe would look like a pinch.
  window.addEventListener("pointerup", (e) => down.delete(e.pointerId));
  window.addEventListener("pointercancel", (e) => down.delete(e.pointerId));

  // Trackpads report two-finger swipes as horizontal wheel events, which
  // keep coming for a while after the fingers lift (momentum). Change photo
  // once per gesture: lock after a change until the events stop.
  let acc = 0;
  let locked = false;
  let idle = null;
  area.addEventListener("wheel", (e) => {
    if (Math.abs(e.deltaX) <= Math.abs(e.deltaY)) return; // vertical scroll
    if (zoomed()) return; // panning around a zoomed-in photo
    e.preventDefault(); // otherwise the browser's back/forward gesture
    clearTimeout(idle);
    idle = setTimeout(() => { acc = 0; locked = false; }, 250);
    if (locked) return;
    acc += e.deltaX;
    if (acc >= WHEEL_DISTANCE && !atEnd()) {
      locked = true;
      onNext();
    } else if (acc <= -WHEEL_DISTANCE && !atStart()) {
      locked = true;
      onPrev();
    }
  }, { passive: false });
}

// makeViewer wires up the page's <dialog id="viewer"> markup. caption(photo,
// index) returns the text shown (and used as alt text) for a photo; photos
// are objects with images.preview. onShow(photo, index), if given, runs
// after a photo is shown (e.g. to render its feedback controls).
function makeViewer(caption, onShow) {
  const dialog = document.getElementById("viewer");
  const img = document.getElementById("viewer-img");
  const name = document.getElementById("viewer-name");
  const pos = document.getElementById("viewer-pos");
  const prev = document.getElementById("viewer-prev");
  const next = document.getElementById("viewer-next");
  const close = document.getElementById("viewer-close");
  let photos = [];
  let index = 0;

  function show(i) {
    index = i;
    const p = photos[i];
    const text = caption(p, i);
    img.src = p.images.preview;
    img.alt = text;
    name.textContent = text;
    pos.textContent = `${i + 1} / ${photos.length}`;
    prev.disabled = i === 0;
    next.disabled = i === photos.length - 1;
    if (onShow) onShow(p, i);
  }

  prev.addEventListener("click", () => index > 0 && show(index - 1));
  next.addEventListener("click", () => index < photos.length - 1 && show(index + 1));
  close.addEventListener("click", () => dialog.close());
  dialog.addEventListener("keydown", (e) => {
    // Arrow keys move the text cursor while typing, not the photo.
    const typing = e.target.matches("input, textarea, select");
    if (typing || e.isComposing || isComposing) return;
    if (e.key === "ArrowLeft") prev.click();
    if (e.key === "ArrowRight") next.click();
  });
  // Escape closes the dialog, except while an IME is composing: there it
  // cancels the conversion.
  dialog.addEventListener("cancel", (e) => {
    if (isComposing) e.preventDefault();
  });

  addSwipe(img, () => prev.click(), () => next.click(),
    () => index === 0, () => index === photos.length - 1);
  dialog.addEventListener("close", () => img.removeAttribute("src"));

  return {
    open(list, i) {
      photos = list;
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
