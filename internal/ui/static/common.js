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
async function request(method, path, body) {
  const init = { method, cache: "no-store" };
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

// --- Full-screen viewer ---

// makeViewer wires up the page's <dialog id="viewer"> markup. caption(photo,
// index) returns the text shown (and used as alt text) for a photo; photos
// are objects with images.preview.
function makeViewer(caption) {
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
  }

  prev.addEventListener("click", () => index > 0 && show(index - 1));
  next.addEventListener("click", () => index < photos.length - 1 && show(index + 1));
  close.addEventListener("click", () => dialog.close());
  dialog.addEventListener("keydown", (e) => {
    if (e.key === "ArrowLeft") prev.click();
    if (e.key === "ArrowRight") next.click();
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
