// The service worker. VERSION, OFFLINE and SHELL are written in front of this
// file by the handler that serves it, so the addresses it caches have one
// source and it is the Go beside the pages that link them: see sw.go.
//
// It writes to the cache while it installs and nowhere else. A page cached at
// runtime would outlive a sign-out and show one person's folder to the next,
// and this file is short enough that somebody can check that by reading it.

self.addEventListener("install", function (event) {
  event.waitUntil(
    caches
      .open(VERSION)
      .then(function (cache) {
        return cache.addAll(SHELL);
      })
      .then(function () {
        return self.skipWaiting();
      })
  );
});

self.addEventListener("activate", function (event) {
  // Every cache but this build's, because the addresses in SHELL carry the
  // build and a new one is a new set of them.
  event.waitUntil(
    caches
      .keys()
      .then(function (names) {
        return Promise.all(
          names
            .filter(function (name) {
              return name !== VERSION;
            })
            .map(function (name) {
              return caches.delete(name);
            })
        );
      })
      .then(function () {
        return self.clients.claim();
      })
  );
});

self.addEventListener("fetch", function (event) {
  var request = event.request;
  if (request.method !== "GET") {
    return;
  }
  // A page the network cannot answer is the offline one, and that is the whole
  // of what this worker promises: the shell, never the library.
  if (request.mode === "navigate") {
    event.respondWith(
      fetch(request).catch(function () {
        return caches.match(OFFLINE);
      })
    );
    return;
  }
  // Everything else is answered from what install put there and from nothing
  // else: a miss goes to the network and is never written back.
  event.respondWith(
    caches.match(request).then(function (hit) {
      return hit || fetch(request);
    })
  );
});
