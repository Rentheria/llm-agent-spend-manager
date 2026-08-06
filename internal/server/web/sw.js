// Service worker for the llm-agent-spend-manager dashboard (T18 — installable PWA).
//
// Strategy is split on purpose:
//   - App shell (/, icons, manifest): cache-first, so the dashboard opens and
//     is installable even offline / before the server responds.
//   - Data (/api/*): network-only, never cached. Spend/activity numbers must be
//     fresh; a stale cached total would be worse than a load error.
const CACHE = "asw-shell-v1";
const SHELL = [
  "/",
  "/index.html",
  "/manifest.webmanifest",
  "/icons/icon-192.png",
  "/icons/icon-512.png",
];

self.addEventListener("install", (event) => {
  event.waitUntil(
    caches.open(CACHE).then((c) => c.addAll(SHELL)).then(() => self.skipWaiting())
  );
});

self.addEventListener("activate", (event) => {
  event.waitUntil(
    caches.keys()
      .then((keys) => Promise.all(keys.filter((k) => k !== CACHE).map((k) => caches.delete(k))))
      .then(() => self.clients.claim())
  );
});

self.addEventListener("fetch", (event) => {
  const req = event.request;
  if (req.method !== "GET") return;
  const url = new URL(req.url);

  // Never cache the API — always go to the network for live figures.
  if (url.pathname.startsWith("/api/")) return;

  // Cache-first for the shell, with a background refresh of the cached copy.
  event.respondWith(
    caches.match(req).then((hit) => {
      const fetched = fetch(req)
        .then((res) => {
          if (res && res.ok) {
            const copy = res.clone();
            caches.open(CACHE).then((c) => c.put(req, copy));
          }
          return res;
        })
        .catch(() => hit);
      return hit || fetched;
    })
  );
});
