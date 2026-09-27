// The player's one job beyond what the markup already does: hand a film that
// needs HLS its playlist.
//
// The video element's src is the file itself, which is what plays with no
// script. When the server set data-hls, the film is one the browser will not
// take as it is, and the playlist is the remux it will: Safari plays that
// natively, and anything else gets it through hls.js with its worker off,
// since the page's policy has no worker-src to give it.
document.querySelectorAll("video[data-hls]").forEach(function (video) {
  var playlist = video.getAttribute("data-hls");
  if (video.canPlayType("application/vnd.apple.mpegurl")) {
    video.src = playlist;
    return;
  }
  if (window.Hls && window.Hls.isSupported()) {
    var hls = new window.Hls({ enableWorker: false });
    hls.loadSource(playlist);
    hls.attachMedia(video);
  }
});
