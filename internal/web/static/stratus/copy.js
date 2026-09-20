// The only JavaScript this project has written, and what it does is one thing.
//
// A share page hands back a URL to send somebody, and copying it by hand from a
// text field is the sort of small misery that makes a feature feel unfinished.
// There is no way to do it without script: the content security policy is
// script-src 'self' with no 'unsafe-inline', so not even an onclick attribute
// would run -- which is why this is a file rather than three characters in the
// markup.
//
// It degrades by construction. The button ships hidden and this is what shows
// it, so a browser with no script shows the field alone, which is what the page
// was before.
document.querySelectorAll("[data-copies]").forEach(function (button) {
  var field = document.getElementById(button.getAttribute("data-copies"));
  if (!field || !navigator.clipboard) {
    return;
  }
  button.classList.remove("d-none");
  button.addEventListener("click", function () {
    navigator.clipboard.writeText(field.value).then(function () {
      var was = button.textContent;
      button.textContent = "Copied";
      setTimeout(function () {
        button.textContent = was;
      }, 1500);
    });
  });
});
