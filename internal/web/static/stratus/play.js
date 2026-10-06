// The player's one job beyond what the markup already does: hand a film that
// needs HLS its playlist.
//
// The element's sources are the playlist and the file itself, and a browser
// that plays either one has already chosen by the time this runs -- Safari
// takes the playlist, and nothing here must touch it, or hls.js would attach
// a MediaSource over a film that is already playing.
document.querySelectorAll("video[data-hls]").forEach(function (video) {
  if (video.canPlayType("application/vnd.apple.mpegurl")) {
    return;
  }
  // What is left is a browser that took neither, and for it this element will
  // never hold an address a television could fetch: the file it fell back to
  // is the one the server remuxes precisely because a receiver will not play
  // it, and the MediaSource below is not an address at all. So the cast
  // button the browser offers for it leads nowhere, and this is how it stops
  // being offered -- casting from here is still a decision (#305).
  video.disableRemotePlayback = true;
  if (window.Hls && window.Hls.isSupported()) {
    var hls = new window.Hls({ enableWorker: false });
    hls.loadSource(video.getAttribute("data-hls"));
    hls.attachMedia(video);
  }
});
