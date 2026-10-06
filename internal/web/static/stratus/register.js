// Registering the worker, which is the only thing this file does: a manifest
// alone does not make a page installable, and no browser offers to install one
// without a worker that has a fetch handler (#129).
//
// It degrades by construction like the other three scripts here: a browser
// with no JavaScript registers nothing and gets the server-rendered site it
// had before, which is every page of this UI.
if ("serviceWorker" in navigator) {
  window.addEventListener("load", function () {
    navigator.serviceWorker.register("/sw.js");
  });
}
