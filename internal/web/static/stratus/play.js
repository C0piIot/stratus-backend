// The player's one job beyond what the markup already does: hand a film that
// needs HLS its playlist.
//
// The element's sources are the playlist and the file itself, and a browser
// that plays either one has already chosen by the time this runs -- Safari
// takes the playlist, and nothing here must touch it, or hls.js would attach
// a MediaSource over a film that is already playing. What is left is the
// browser that skipped both: it gets the playlist through hls.js, with its
// worker off, since the page's policy has no worker-src to give it.
document.querySelectorAll("video[data-hls]").forEach(function (video) {
  if (video.canPlayType("application/vnd.apple.mpegurl")) {
    return;
  }
  if (window.Hls && window.Hls.isSupported()) {
    var hls = new window.Hls({ enableWorker: false });
    hls.loadSource(video.getAttribute("data-hls"));
    hls.attachMedia(video);
  }
});
