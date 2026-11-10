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
// never innerHTML. request, h and makeViewer come from common.js.
"use strict";

// Image URLs expire after about an hour; refetch a bit before that.
const IMAGE_URL_MAX_AGE_MS = 50 * 60 * 1000;

const app = document.getElementById("app");
const signoutBtn = document.getElementById("signout");

// Whether an API call has succeeded since the page loaded, to tell "not
// signed in yet" from "session expired" when a 401 arrives.
let signedIn = false;

const api = (path) => request("GET", path);

function render(...nodes) {
  app.replaceChildren(...nodes.filter((n) => n != null)); // null would render as "null"
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
  const [{ sessions }, { photos }, { reviewers }] = await Promise.all([
    api("/api/sessions"),
    api(`/api/sessions/${enc}/photos`),
    api(`/api/sessions/${enc}/reviewers`),
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
    reviewersPanel(sid, reviewers),
    photos.length === 0
      ? h("p", { class: "muted" }, "No photos in this session yet.")
      : h("div", { class: "grid" }, ...photos.map((p) => tile(p, ready, onImageError))),
  );
}

// --- Reviewers panel ---

const DAY_MS = 24 * 60 * 60 * 1000;
const MAX_LINK_DAYS = 90; // server limit (review.MaxReviewerTTL)

function formatDate(iso) {
  return new Date(iso).toLocaleString(undefined, { dateStyle: "medium", timeStyle: "short" });
}

// reviewersPanel lists a session's reviewer links and lets the owner
// create, extend and revoke them. It re-renders itself in place after each
// change so the rest of the page (and a freshly created link) stays put.
function reviewersPanel(sid, initial) {
  const base = `/api/sessions/${encodeURIComponent(sid)}/reviewers`;
  const panel = h("section", { class: "panel", "aria-labelledby": "reviewers-heading" });
  let reviewers = initial;
  let newLink = null; // {name, url}: shown until the page is left
  let error = "";

  async function act(fn) {
    error = "";
    try {
      await fn();
      ({ reviewers } = await api(base));
    } catch (err) {
      if (err instanceof Unauthorized) return route();
      error = err.message;
    }
    draw();
  }

  function create(name, days) {
    return act(async () => {
      const expiresAt = new Date(Date.now() + days * DAY_MS).toISOString();
      const res = await request("POST", base, { name, expiresAt });
      newLink = { name: res.reviewer.name, url: location.origin + res.linkPath };
    });
  }

  function extend(r) {
    // A week past whichever is later, now or the current expiry, within
    // the server's limit.
    const from = Math.max(Date.now(), Date.parse(r.expiresAt));
    const until = Math.min(from + 7 * DAY_MS, Date.now() + MAX_LINK_DAYS * DAY_MS - 60 * 1000);
    return act(() => request("PATCH", `${base}/${encodeURIComponent(r.id)}`, { expiresAt: new Date(until).toISOString() }));
  }

  function revoke(r) {
    if (!confirm(`Revoke ${r.name}'s link? It stops working immediately and can't be re-enabled.`)) return;
    return act(() => request("DELETE", `${base}/${encodeURIComponent(r.id)}`));
  }

  function status(r) {
    if (r.revoked) return h("span", { class: "badge revoked" }, "Revoked");
    if (!r.active) return h("span", { class: "badge expired" }, "Expired " + formatDate(r.expiresAt));
    return h("span", { class: "badge active" }, "Active until " + formatDate(r.expiresAt));
  }

  function row(r) {
    return h("li", { class: "reviewer" },
      h("span", { class: "reviewer-name" }, r.name),
      status(r),
      h("span", { class: "reviewer-actions" },
        r.revoked ? null : h("button", { type: "button", onclick: () => extend(r) }, "Extend 7 days"),
        r.revoked ? null : h("button", { type: "button", class: "danger", onclick: () => revoke(r) }, "Revoke"),
      ),
    );
  }

  function linkBox() {
    const field = h("input", { type: "text", readonly: true, value: newLink.url, "aria-label": "Review link" });
    const copy = h("button", { type: "button" }, "Copy");
    copy.addEventListener("click", async () => {
      try {
        await navigator.clipboard.writeText(newLink.url);
        copy.textContent = "Copied";
      } catch {
        field.select(); // clipboard blocked: let the owner copy by hand
      }
    });
    return h("div", { class: "link-box", role: "status" },
      h("p", {}, `Link for ${newLink.name}. Copy it now: it can't be shown again.`),
      h("div", { class: "link-row" }, field, copy),
    );
  }

  function form() {
    const name = h("input", { type: "text", name: "name", required: true, maxlength: "100", placeholder: "Reviewer name", "aria-label": "Reviewer name" });
    const days = h("select", { name: "days", "aria-label": "Link valid for" },
      ...[3, 7, 14, 30].map((d) => h("option", { value: String(d), selected: d === 7 }, `${d} days`)));
    const f = h("form", { class: "reviewer-form" }, name, days, h("button", { type: "submit" }, "Create link"));
    f.addEventListener("submit", (e) => {
      e.preventDefault();
      const n = name.value.trim();
      if (n) create(n, Number(days.value));
    });
    return f;
  }

  function draw() {
    // replaceChildren (unlike h) would render null as the text "null",
    // so optional parts are filtered out first.
    panel.replaceChildren(...[
      h("h2", { id: "reviewers-heading" }, "Reviewers"),
      reviewers.length === 0
        ? h("p", { class: "muted" }, "No review links yet.")
        : h("ul", { class: "reviewers" }, ...reviewers.map(row)),
      newLink ? linkBox() : null,
      error ? h("p", { class: "error", role: "alert" }, error) : null,
      form(),
    ].filter((n) => n != null));
  }

  draw();
  return panel;
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

const viewer = makeViewer((p) => p.filename);

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
