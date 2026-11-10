// Reviewer page, opened from a link like https://…/r#<token>.
//
// 1. The token is in the URL fragment, which browsers never send to the
//    server (so it can't land in logs or Referer headers). On every load
//    this script posts it to /api/review/session, which sets an HttpOnly
//    cookie. The token deliberately stays in the address bar: apps like
//    Instagram and LINE open links in an in-app browser with its own
//    cookies, and moving to the real browser carries over only the URL. If
//    the token were removed, the real browser would have neither the token
//    nor the cookie. (The link is already in the reviewer's chat app on the
//    same device, so keeping it in history exposes nothing new.)
// 2. Everything else uses that cookie: GET /api/review returns the session,
//    its photos and this reviewer's feedback; PATCH/DELETE
//    /api/review/photos/{id}/feedback save it.
//
// Feedback autosaves: verdict and stars on click, the comment shortly after
// typing stops (never mid-IME-composition). Changes show immediately;
// saves for a photo are sent one at a time in order, and failures can be
// retried.
//
// Language: English or Japanese, from the browser's preferred languages
// unless the reviewer picked one with the switch (remembered in
// localStorage). <html lang> follows, so kanji get Japanese glyph forms.
//
// request, h, makeViewer and isComposing come from common.js.
"use strict";

const STRINGS = {
  en: {
    brand: "Photo review",
    loading: "Loading…",
    reviewingAs: "Reviewing as {name}",
    validUntil: "This link is valid until {date}.",
    photoCount: (n) => (n === 1 ? "1 photo" : `${n} photos`),
    hint: "Tap Accept or Reject. Open a photo to add stars or a comment.",
    noPhotos: "There are no photos to review yet.",
    invalidLink: "This review link is no longer valid. Please ask the photographer for a new one.",
    noSession: "Please open the review link you were sent.",
    error: "Something went wrong. Please reload the page.",
    photo: "Photo {n}",
    viewPhoto: "View photo {n}",
    accept: "Accept",
    reject: "Reject",
    rating: "Rating",
    rateN: "{n} out of 5",
    comment: "Comment",
    commentPlaceholder: "Comment (optional)",
    saving: "Saving…",
    saved: "All changes saved",
    saveFailed: "Some changes weren't saved.",
    retry: "Retry",
    language: "Language",
    close: "Close",
    prev: "Previous photo",
    next: "Next photo",
    dialog: "Photo preview",
  },
  ja: {
    brand: "写真レビュー",
    loading: "読み込み中…",
    reviewingAs: "レビュー担当: {name}",
    validUntil: "このリンクの有効期限: {date}",
    photoCount: (n) => `写真 ${n} 枚`,
    hint: "「採用」か「不採用」を選んでください。写真を開くと、評価やコメントを追加できます。",
    noPhotos: "レビューする写真はまだありません。",
    invalidLink: "このレビューリンクは無効になりました。撮影者に新しいリンクを依頼してください。",
    noSession: "送られたレビュー用リンクを開いてください。",
    error: "エラーが発生しました。ページを再読み込みしてください。",
    photo: "写真 {n}",
    viewPhoto: "写真 {n} を表示",
    accept: "採用",
    reject: "不採用",
    rating: "評価",
    rateN: "5段階中 {n}",
    comment: "コメント",
    commentPlaceholder: "コメント（任意）",
    saving: "保存中…",
    saved: "すべて保存しました",
    saveFailed: "一部の変更を保存できませんでした。",
    retry: "再試行",
    language: "言語",
    close: "閉じる",
    prev: "前の写真",
    next: "次の写真",
    dialog: "写真のプレビュー",
  },
};

const LANG_KEY = "photosync.lang";
const COMMENT_SAVE_DELAY_MS = 800;
const MAX_COMMENT = 2000;

// --- Language ---

function detectLang() {
  try {
    const saved = localStorage.getItem(LANG_KEY);
    if (saved in STRINGS) return saved;
  } catch {
    // Storage unavailable (private mode, blocked): fall through.
  }
  // The first of the browser's preferred languages that we support wins.
  for (const l of navigator.languages || [navigator.language || ""]) {
    const base = l.toLowerCase().split("-")[0];
    if (base in STRINGS) return base;
  }
  return "en";
}

let lang = detectLang();

// t looks up a string in the current language and fills {placeholders}.
function t(key, vars = {}) {
  const s = STRINGS[lang][key];
  return s.replace(/\{(\w+)\}/g, (_, k) => String(vars[k]));
}

function formatDate(d, opts) {
  return new Intl.DateTimeFormat(lang, opts).format(d);
}

// Session dates are plain YYYY-MM-DD; show them as local calendar dates.
function formatDay(ymd) {
  const [y, m, d] = ymd.split("-").map(Number);
  return formatDate(new Date(y, m - 1, d), { dateStyle: "long" });
}

const langButtons = document.querySelectorAll(".lang-switch button");

function applyLang() {
  document.documentElement.lang = lang;
  document.title = t("brand");
  document.getElementById("brand").textContent = t("brand");
  document.querySelector(".lang-switch").setAttribute("aria-label", t("language"));
  for (const b of langButtons) b.setAttribute("aria-pressed", String(b.dataset.lang === lang));
  viewer.setLabels({ close: t("close"), prev: t("prev"), next: t("next"), dialog: t("dialog") });
}

for (const b of langButtons) {
  b.addEventListener("click", () => {
    flushComment(); // don't lose a pending comment to the re-render
    lang = b.dataset.lang;
    try {
      localStorage.setItem(LANG_KEY, lang);
    } catch {
      // Not remembered, but still switched for this visit.
    }
    applyLang();
    draw(); // re-render the current state in the new language
  });
}

// --- Feedback state and saving ---

// feedback holds this reviewer's feedback by photo ID as shown on screen:
// {verdict, stars, comment}, each null when unset. It's updated as soon as
// the reviewer acts (optimistically), then from the server's reply.
const feedback = new Map();
const EMPTY = Object.freeze({ verdict: null, stars: null, comment: null });

function fbOf(id) {
  return feedback.get(id) || EMPTY;
}

// Saves for one photo go out one after another (queue), so the server
// sees them in the order the reviewer made them.
const queues = new Map(); // photo ID -> Promise of the last save
const failed = new Map(); // photo ID -> merged patch that failed to save
let inFlight = 0;

function save(id, change) {
  // Resend anything that failed earlier for this photo along with this
  // change, so a later edit doesn't silently drop an earlier one.
  const patch = { ...(failed.get(id) || {}), ...change };
  failed.delete(id);
  feedback.set(id, { ...fbOf(id), ...change });
  refresh(id);

  inFlight++;
  updateStatus();
  const previous = queues.get(id) || Promise.resolve();
  const p = previous
    .catch(() => {}) // an earlier failure doesn't block later saves
    // keepalive lets a save started as the page closes still complete.
    .then(() => request("PATCH", `/api/review/photos/${encodeURIComponent(id)}/feedback`, patch, { keepalive: true }))
    .then((saved) => {
      // Only the latest save's reply reflects what's on screen.
      if (queues.get(id) === p) {
        feedback.set(id, { verdict: saved.verdict, stars: saved.stars, comment: saved.comment });
        refresh(id);
      }
    })
    .catch((err) => {
      if (err.status === 401 || err.status === 403) {
        show({ kind: "message", key: messageFor(err) });
        return;
      }
      failed.set(id, { ...(failed.get(id) || {}), ...patch });
      throw err;
    })
    .finally(() => {
      inFlight--;
      updateStatus();
    });
  queues.set(id, p);
  p.catch(() => {}); // failures are shown via the status line
}

function retryFailed() {
  const pending = [...failed];
  failed.clear();
  for (const [id, patch] of pending) save(id, patch);
}

// --- Status line (page header and viewer) ---

const headerStatus = document.querySelector(".save-status");
let viewerStatus = null; // set while the viewer panel is rendered

function statusNodes() {
  if (inFlight > 0) return { cls: "", nodes: [t("saving")] };
  if (failed.size > 0) {
    return {
      cls: "error",
      nodes: [t("saveFailed"), h("button", { type: "button", onclick: retryFailed }, t("retry"))],
    };
  }
  return { cls: "", nodes: queues.size > 0 ? [t("saved")] : [] };
}

function updateStatus() {
  for (const el of [headerStatus, viewerStatus]) {
    if (!el) continue;
    const { cls, nodes } = statusNodes();
    el.className = "save-status " + cls;
    fill(el, ...nodes);
  }
}

// --- Viewer feedback panel ---

const viewer = makeViewer((_, i) => t("photo", { n: i + 1 }), renderPanel);
const panel = document.getElementById("viewer-feedback");
document.getElementById("viewer").addEventListener("close", () => {
  flushComment();
  viewerStatus = null;
  fill(panel);
});

function verdictButtons(id, small) {
  const f = fbOf(id);
  const btn = (value) => h("button", {
    type: "button",
    class: `verdict ${value}`,
    "aria-pressed": String(f.verdict === value),
    // Tapping the active verdict again clears it.
    onclick: () => save(id, { verdict: f.verdict === value ? null : value }),
  }, t(value));
  return small ? [btn("accept"), btn("reject")] : h("div", { class: "row" }, btn("accept"), btn("reject"));
}

function starButtons(id) {
  const f = fbOf(id);
  return h("div", { class: "stars", role: "group", "aria-label": t("rating") },
    ...[1, 2, 3, 4, 5].map((n) => h("button", {
      type: "button",
      class: f.stars != null && n <= f.stars ? "on" : "",
      "aria-label": t("rateN", { n }),
      "aria-pressed": String(f.stars === n),
      // Tapping the current rating again clears it.
      onclick: () => save(id, { stars: f.stars === n ? null : n }),
    }, "★")),
  );
}

// The comment box being edited, if any, and its pending save timer.
let comment = null; // {id, el, timer}

function flushComment() {
  if (!comment) return;
  clearTimeout(comment.timer);
  comment.timer = null;
  if (isComposing) return; // finishes on compositionend
  const value = comment.el.value;
  const current = fbOf(comment.id).comment || "";
  if (value !== current) save(comment.id, { comment: value === "" ? null : value });
}

function scheduleComment() {
  clearTimeout(comment.timer);
  comment.timer = setTimeout(flushComment, COMMENT_SAVE_DELAY_MS);
}

function renderPanel(photo) {
  flushComment(); // save the previous photo's comment before switching
  const id = photo.id;
  const box = h("textarea", {
    "aria-label": t("comment"),
    placeholder: t("commentPlaceholder"),
    maxlength: String(MAX_COMMENT),
    rows: "2",
  });
  box.value = fbOf(id).comment || "";
  comment = { id, el: box, timer: null };

  // Typing schedules a save; IME composition (kana → kanji) doesn't, and
  // Enter that confirms a conversion is part of composing, not a submit.
  box.addEventListener("input", (e) => {
    if (!e.isComposing && !isComposing) scheduleComment();
  });
  box.addEventListener("compositionend", scheduleComment);
  box.addEventListener("blur", flushComment);

  viewerStatus = h("span", { class: "save-status", role: "status", "aria-live": "polite" });
  fill(panel, verdictButtons(id, false), starButtons(id), box, viewerStatus);
  updateStatus();
}

// refresh updates everything showing photo id's feedback, in place.
function refresh(id) {
  const tile = tiles.get(id);
  if (tile) tile.update();
  const cur = viewer.current();
  if (cur && cur.id === id) {
    // Rebuild buttons and stars but keep the textarea (and its cursor and
    // IME state) as it is.
    const [verdicts, stars] = panel.children;
    verdicts.replaceWith(verdictButtons(id, false));
    stars.replaceWith(starButtons(id));
  }
}

// --- Page rendering ---

const app = document.getElementById("app");

// state is what's on screen: {kind: "loading" | "message" | "view", ...}.
let state = { kind: "loading" };
const tiles = new Map(); // photo ID -> {el, update()}

function show(next) {
  state = next;
  draw();
}

function draw() {
  tiles.clear();
  let nodes;
  switch (state.kind) {
    case "loading":
      nodes = [h("p", { class: "message muted" }, t("loading"))];
      break;
    case "message":
      viewer.close();
      nodes = [h("p", { class: "message" }, t(state.key))];
      break;
    case "view":
      nodes = viewNodes(state.view);
      break;
  }
  fill(app, ...nodes);
  updateStatus();
  // Re-label an open viewer's panel in the new language.
  const cur = viewer.current();
  if (cur) renderPanel(cur);
}

function viewNodes(v) {
  const expires = formatDate(new Date(v.reviewer.expiresAt), { dateStyle: "medium", timeStyle: "short" });
  return [
    h("h1", {}, v.session.title),
    h("p", { class: "reviewing-as" }, t("reviewingAs", { name: v.reviewer.name })),
    h("p", { class: "subhead muted" },
      [formatDay(v.session.date), STRINGS[lang].photoCount(v.photos.length), t("validUntil", { date: expires })].join(" · ")),
    v.photos.length === 0
      ? h("p", { class: "muted" }, t("noPhotos"))
      : h("div", {}, h("p", { class: "hint muted" }, t("hint")),
        h("div", { class: "grid" }, ...v.photos.map((p, i) => tile(p, i, v.photos)))),
  ];
}

function tile(photo, i, photos) {
  const label = t("photo", { n: i + 1 });
  const img = h("img", { src: photo.images.thumb, alt: label, loading: "lazy" });
  img.addEventListener("error", onImageError);
  const meta = h("div", { class: "tile-meta" });
  const actions = h("div", { class: "tile-actions" });
  const el = h("figure", { class: "tile" },
    h("button", {
      type: "button",
      "aria-label": t("viewPhoto", { n: i + 1 }),
      onclick: () => viewer.open(photos, i),
    }, img),
    meta,
    actions,
  );
  function update() {
    const f = fbOf(photo.id);
    el.classList.toggle("accepted", f.verdict === "accept");
    el.classList.toggle("rejected", f.verdict === "reject");
    // Row 1: number, stars, comment marker. Row 2: the verdict buttons,
    // on their own row so a long star rating can't push them off the tile.
    fill(meta,
      h("span", { class: "tile-num" }, String(i + 1)),
      f.stars ? h("span", { class: "tile-stars", "aria-label": t("rateN", { n: f.stars }) }, "★".repeat(f.stars)) : null,
      f.comment ? h("span", { class: "tile-comment", "aria-hidden": "true" }, "💬") : null,
    );
    fill(actions, ...verdictButtons(photo.id, true));
  }
  update();
  tiles.set(photo.id, { el, update });
  return el;
}

// Image URLs are signed and expire (on AWS possibly sooner than their
// nominal hour, since they're signed with short-lived credentials). On a
// broken image, refetch the view for fresh URLs, at most once a minute so
// a genuinely missing image can't cause a reload loop.
let lastImageReload = 0;
function onImageError() {
  if (Date.now() - lastImageReload < 60 * 1000) return;
  lastImageReload = Date.now();
  load();
}
document.getElementById("viewer-img").addEventListener("error", onImageError);

// --- Data ---

function messageFor(err) {
  if (err.status === 401) return "noSession";
  if (err.status === 403) return "invalidLink";
  return "error";
}

async function load() {
  try {
    const view = await request("GET", "/api/review");
    // Keep local edits that haven't been confirmed yet; take the server's
    // version of everything else.
    for (const p of view.photos) {
      if (queues.has(p.id) || failed.has(p.id)) continue;
      const f = p.feedback;
      feedback.set(p.id, f ? { verdict: f.verdict, stars: f.stars, comment: f.comment } : EMPTY);
    }
    // A reload (e.g. for fresh image URLs) shouldn't kick the reviewer out
    // of the photo they're looking at: reopen it from the new list.
    const open = viewer.current();
    viewer.close();
    show({ kind: "view", view });
    const i = open ? view.photos.findIndex((p) => p.id === open.id) : -1;
    if (i >= 0) viewer.open(view.photos, i);
  } catch (err) {
    show({ kind: "message", key: messageFor(err) });
  }
}

async function start() {
  applyLang();
  draw();

  // Exchanged on every load that has it, so whichever browser ends up with
  // the URL (in-app, Safari, Chrome…) signs itself in. Without it (e.g. a
  // bookmark of plain /r), the cookie from an earlier visit is used.
  const token = location.hash.slice(1);
  if (token) {
    try {
      await request("POST", "/api/review/session", { token });
    } catch (err) {
      // A malformed or unknown token is the same thing to the reviewer.
      show({ kind: "message", key: err.status === 400 ? "invalidLink" : messageFor(err) });
      return;
    }
  }
  await load();
}

// Last chance to save a comment typed just before leaving the page.
window.addEventListener("pagehide", flushComment);

start();
