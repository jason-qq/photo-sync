// Reviewer page, opened from a link like https://…/r#<token>.
//
// 1. The token is in the URL fragment, which browsers never send to the
//    server (so it can't land in logs or Referer headers). This script
//    removes it from the address bar and history straight away, then posts
//    it once to /api/review/session, which sets an HttpOnly cookie.
// 2. Everything else uses that cookie: GET /api/review returns the session
//    and its photos. Reopening /r later (without the fragment) works as
//    long as the cookie and the link are valid.
//
// Language: English or Japanese, from the browser's preferred languages
// unless the reviewer picked one with the switch (remembered in
// localStorage). <html lang> follows, so kanji get Japanese glyph forms.
//
// request, h and makeViewer come from common.js.
"use strict";

const STRINGS = {
  en: {
    brand: "Photo review",
    loading: "Loading…",
    reviewingAs: "Reviewing as {name}",
    validUntil: "This link is valid until {date}.",
    photoCount: (n) => (n === 1 ? "1 photo" : `${n} photos`),
    noPhotos: "There are no photos to review yet.",
    invalidLink: "This review link is no longer valid. Please ask the photographer for a new one.",
    noSession: "Please open the review link you were sent.",
    error: "Something went wrong. Please reload the page.",
    photo: "Photo {n}",
    viewPhoto: "View photo {n}",
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
    noPhotos: "レビューする写真はまだありません。",
    invalidLink: "このレビューリンクは無効になりました。撮影者に新しいリンクを依頼してください。",
    noSession: "送られたレビュー用リンクを開いてください。",
    error: "エラーが発生しました。ページを再読み込みしてください。",
    photo: "写真 {n}",
    viewPhoto: "写真 {n} を表示",
    language: "言語",
    close: "閉じる",
    prev: "前の写真",
    next: "次の写真",
    dialog: "写真のプレビュー",
  },
};

const LANG_KEY = "photosync.lang";

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

const viewer = makeViewer((_, i) => t("photo", { n: i + 1 }));
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

// --- State and rendering ---

const app = document.getElementById("app");

// state is what's on screen: {kind: "loading" | "message" | "view", ...}.
let state = { kind: "loading" };

function show(next) {
  state = next;
  draw();
}

function draw() {
  let nodes;
  switch (state.kind) {
    case "loading":
      nodes = [h("p", { class: "message muted" }, t("loading"))];
      break;
    case "message":
      nodes = [h("p", { class: "message" }, t(state.key))];
      break;
    case "view":
      nodes = viewNodes(state.view);
      break;
  }
  app.replaceChildren(...nodes);
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
      : h("div", { class: "grid" }, ...v.photos.map((p, i) => tile(p, i, v.photos))),
  ];
}

function tile(photo, i, photos) {
  const label = t("photo", { n: i + 1 });
  const img = h("img", { src: photo.images.thumb, alt: label, loading: "lazy" });
  img.addEventListener("error", onImageError);
  return h("figure", { class: "tile" },
    h("button", {
      type: "button",
      "aria-label": t("viewPhoto", { n: i + 1 }),
      onclick: () => viewer.open(photos, i),
    }, img),
    h("figcaption", {}, String(i + 1)),
  );
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

// --- Data ---

function messageFor(err) {
  if (err.status === 401) return "noSession";
  if (err.status === 403) return "invalidLink";
  return "error";
}

async function load() {
  try {
    const view = await request("GET", "/api/review");
    viewer.close();
    show({ kind: "view", view });
  } catch (err) {
    show({ kind: "message", key: messageFor(err) });
  }
}

async function start() {
  applyLang();
  draw();

  const token = location.hash.slice(1);
  if (token) {
    // Remove the token from the address bar and history before anything
    // else, so it isn't left behind on this device.
    history.replaceState(null, "", location.pathname + location.search);
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

start();
