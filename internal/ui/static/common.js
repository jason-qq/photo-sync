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
