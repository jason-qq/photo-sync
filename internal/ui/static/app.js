// Owner UI: sign in with the API key, browse sessions, view photos.
//
// Sign-in posts the key to /api/login, which sets an HttpOnly session
// cookie; this script never stores the key and can't read the cookie. The
// browser sends the cookie with every same-origin request.
//
// Routes (URL hash, so a refresh keeps your place):
//   #/            session list
//   #/s/{sid}     one session's photos
//
// All user-supplied text (titles, filenames) is inserted with textContent,
// never innerHTML.
"use strict";

// Image URLs expire after about an hour; refetch a bit before that.
const IMAGE_URL_MAX_AGE_MS = 50 * 60 * 1000;

const app = document.getElementById("app");
const signoutBtn = document.getElementById("signout");

// Whether an API call has succeeded since the page loaded, to tell "not
// signed in yet" from "session expired" when a 401 arrives.
let signedIn = false;

// --- API ---

class Unauthorized extends Error {}

// request sends a same-origin request (the session cookie goes along
// automatically) and returns the decoded JSON body, or null for 204.
async function request(method, path, body) {
  const init = { method, cache: "no-store" };
  if (body !== undefined) {
    init.headers = { "Content-Type": "application/json" };
    init.body = JSON.stringify(body);
  }
  const resp = await fetch(path, init);
  if (resp.status === 401) throw new Unauthorized("unauthorized");
  if (resp.status === 204) return null;
  const data = await resp.json().catch(() => ({}));
  if (!resp.ok) throw new Error(data.error || `HTTP ${resp.status}`);
  return data;
}

const api = (path) => request("GET", path);

// --- DOM helper ---

// h("a", {href: "#"}, "text", childNode) builds an element. Strings become
// text nodes; attributes starting with "on" become event listeners.
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

function render(...nodes) {
  app.replaceChildren(...nodes);
}

function plural(n, word) {
  return `${n} ${word}${n === 1 ? "" : "s"}`;
}

// --- Views ---

function showSignIn(message) {
  signedIn = false;
  signoutBtn.hidden = true;
  const input = h("input", {
    type: "password", id: "key", autocomplete: "current-password", required: true,
  });
  const status = h("p", { class: "error", role: "alert" }, message || "");
  const form = h("form", { class: "signin" },
    h("h1", {}, "Sign in"),
    h("label", { for: "key" }, "API key"),
    input,
    h("button", { type: "submit" }, "Sign in"),
    status,
  );
  form.addEventListener("submit", async (e) => {
    e.preventDefault();
    status.textContent = "";
    try {
      await request("POST", "/api/login", { apiKey: input.value.trim() });
      input.value = "";
      route();
    } catch (err) {
      status.textContent = err instanceof Unauthorized ? "That key was not accepted." : err.message;
    }
  });
  render(form);
  input.focus();
}

async function showSessions() {
  const { sessions } = await api("/api/sessions");
  if (sessions.length === 0) {
    render(
      h("h1", {}, "Sessions"),
      h("p", { class: "muted" }, "No sessions yet. Run the uploader on a folder of raws to create one."),
    );
    return;
  }
  render(
    h("h1", {}, "Sessions"),
    h("p", { class: "subhead muted" }, plural(sessions.length, "session")),
    h("ul", { class: "sessions" }, ...sessions.map((s) =>
      h("li", {}, h("a", { href: "#/s/" + encodeURIComponent(s.id) },
        h("span", {}, s.title),
        h("span", { class: "muted" }, s.date),
      )),
    )),
  );
}

async function showSession(sid) {
  const enc = encodeURIComponent(sid);
  const [{ sessions }, { photos }] = await Promise.all([
    api("/api/sessions"),
    api(`/api/sessions/${enc}/photos`),
  ]);
  const session = sessions.find((s) => s.id === sid);
  const loadedAt = Date.now();
  const ready = photos.filter((p) => p.images);
  const pending = photos.length - ready.length;

  // Expired image URLs fail to load; reload the list to get fresh ones.
  // Only once per load, so a genuinely missing image can't loop.
  let reloaded = false;
  const onImageError = () => {
    if (!reloaded && Date.now() - loadedAt > IMAGE_URL_MAX_AGE_MS) {
      reloaded = true;
      route();
    }
  };

  const summary = [plural(photos.length, "photo")];
  if (pending > 0) summary.push(`${pending} still uploading`);

  render(
    h("p", {}, h("a", { href: "#/" }, "← Sessions")),
    h("h1", {}, session ? session.title : "Session"),
    h("p", { class: "subhead muted" }, [session?.date, ...summary].filter(Boolean).join(" · ")),
    photos.length === 0
      ? h("p", { class: "muted" }, "No photos in this session yet.")
      : h("div", { class: "grid" }, ...photos.map((p) => tile(p, ready, onImageError))),
  );
}

function tile(photo, ready, onImageError) {
  if (!photo.images) {
    return h("figure", { class: "tile" },
      h("div", { class: "placeholder" }, "Uploading…"),
      h("figcaption", { title: photo.filename }, photo.filename),
    );
  }
  const img = h("img", { src: photo.images.thumb, alt: photo.filename, loading: "lazy" });
  img.addEventListener("error", onImageError);
  return h("figure", { class: "tile" },
    h("button", {
      type: "button",
      "aria-label": "View " + photo.filename,
      onclick: () => viewer.open(ready, ready.indexOf(photo)),
    }, img),
    h("figcaption", { title: photo.filename }, photo.filename),
  );
}

function showError(err) {
  render(
    h("h1", {}, "Something went wrong"),
    h("p", { class: "error" }, err.message),
    h("p", {}, h("a", { href: "#/" }, "Back to sessions")),
  );
}

// --- Full-screen viewer ---

const viewer = (() => {
  const dialog = document.getElementById("viewer");
  const img = document.getElementById("viewer-img");
  const name = document.getElementById("viewer-name");
  const pos = document.getElementById("viewer-pos");
  const prev = document.getElementById("viewer-prev");
  const next = document.getElementById("viewer-next");
  let photos = [];
  let index = 0;

  function show(i) {
    index = i;
    const p = photos[i];
    img.src = p.images.preview;
    img.alt = p.filename;
    name.textContent = p.filename;
    pos.textContent = `${i + 1} / ${photos.length}`;
    prev.disabled = i === 0;
    next.disabled = i === photos.length - 1;
  }

  prev.addEventListener("click", () => index > 0 && show(index - 1));
  next.addEventListener("click", () => index < photos.length - 1 && show(index + 1));
  document.getElementById("viewer-close").addEventListener("click", () => dialog.close());
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
  };
})();

// --- Router ---

async function route() {
  viewer.close();
  const m = location.hash.match(/^#\/s\/([^/]+)$/);
  try {
    if (m) await showSession(decodeURIComponent(m[1]));
    else await showSessions();
    signedIn = true;
    signoutBtn.hidden = false;
  } catch (err) {
    if (err instanceof Unauthorized) {
      showSignIn(signedIn ? "Your session has ended. Sign in again." : "");
    } else {
      showError(err);
    }
  }
}

signoutBtn.addEventListener("click", async () => {
  try {
    await request("POST", "/api/logout");
  } catch {
    // The cookie may already be gone; show the sign-in page regardless.
  }
  location.hash = "#/";
  showSignIn();
});
window.addEventListener("hashchange", route);
route();
