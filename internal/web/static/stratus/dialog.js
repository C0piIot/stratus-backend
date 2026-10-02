// The third script this project has written, and the second that exists only
// because of the content security policy.
//
// A file's details are a modal, and there is no way to open one declaratively:
// <dialog> opens as a modal from showModal() and from nothing else. Bootstrap's
// own modal is not an option either -- it is driven by inline styles on the
// element and on <body>, which style-src 'self' drops, so it would open a box
// the browser never displays.
//
// It degrades by construction, like the other two. The button is a link to a
// page that renders the same facts, and htmx is what turns that into a fetch;
// with no JavaScript neither this nor htmx runs and the link is followed.
document.addEventListener("htmx:afterSwap", function (event) {
  var target = event.target;
  if (!target || !target.querySelector) {
    return;
  }
  var dialog = target.querySelector("dialog");
  if (dialog && typeof dialog.showModal === "function" && !dialog.open) {
    dialog.showModal();
  }
});
