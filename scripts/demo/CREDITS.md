# Demo media for the Stratus test instance

Everything here is freely licensed. Nothing in this bundle is Stratus's own
work, and none of it is in the repository: it is a release asset, fetched and
verified by `scripts/seed-demo.sh`.

## Photos — CC0 1.0 (public domain dedication), Wikimedia Commons

Resized to 1600 px and re-encoded at quality 82. The EXIF is the photographer's,
kept on purpose: it is what the media indexer reads.

| file | author | source |
|---|---|---|
| `Photos/wilderness.jpg` | David Marcu | https://commons.wikimedia.org/wiki/File:Alone_in_the_unspoilt_wilderness_(Unsplash).jpg |
| `Photos/bee-on-flower.jpg` | Dominik Scythe | https://commons.wikimedia.org/wiki/File:Bee_on_orange_flower_in_macro_(Unsplash).jpg |
| `Photos/akiapolaau.jpg` | Alan Schmierer | https://commons.wikimedia.org/wiki/File:Akiapola%27au_(8-30-2017)_Hakalau_Forest_National_Wildlife_Refuge,_Hawaii_co,_Hawaii_-18_male_(37025110631).jpg |
| `Photos/bell-peppers.jpg` | Judgefloro | https://commons.wikimedia.org/wiki/File:1296Green_bell_peppers_slices_Food_ingredients_15.jpg |
| `Photos/abay-facade.jpg` | Lijingys | https://commons.wikimedia.org/wiki/File:Abay_130_residential_complex_facade_at_Abay_Avenue_130k3_in_Almaty.jpg |

`Music/Kevin MacLeod/Stratus demo/cover.jpg` is a square crop of
`Photos/bee-on-flower.jpg`, same licence and same author.

## Music — CC BY 4.0

"Carefree" and "Wallpaper" by **Kevin MacLeod** (https://incompetech.com),
licensed under Creative Commons Attribution 4.0:
https://creativecommons.org/licenses/by/4.0/

Both are **excerpts**: the first minute, re-encoded at 128 kbps with a fade at
the end. The `album` tag ("Stratus demo") and the track numbers are ours, added
so that a Subsonic client has an album to browse; the music is his.

## Video — CC BY 3.0

Every file in `Video/` is fifteen seconds of the *Sintel* trailer,
© copyright Blender Foundation | https://www.sintel.org, licensed under
Creative Commons Attribution 3.0: https://creativecommons.org/licenses/by/3.0/

`Video/sintel-trailer.mp4` was cut from `sintel_trailer-480p.mp4` and
re-encoded to 854×480 h264/aac. The others are the same fifteen seconds
re-encoded into the shapes a server meets in a real library, each for what it
makes the server do:

| file | what it is | what it exercises |
|---|---|---|
| `h264-aac-moov-last.mp4` | the same streams, index at the end | a reader that has to find `moov` behind the film |
| `hevc-main10-1080p.mp4` | HEVC Main 10, 1080p, AAC — cut from `sintel_trailer-1080p.mp4` at 0:20 | a picture older players cannot take |
| `h264-ac3-5.1.mkv` | H.264, AC-3 5.1 in Matroska | a picture that plays and a sound that does not |
| `vp9-opus.webm` | VP9, Opus | a header that does not state a profile |
| `mpeg2-mp2.ts` | MPEG-2, MP2 in MPEG-TS | no duration in the header, and no thumbnail by name |
| `mpeg4-mp3.avi` | MPEG-4 Part 2, MP3 in AVI | an old container read through a local copy |
| `portrait.mov` | H.264 stored on its side with a 90° display matrix | a recording a phone made holding it upright |

The 5.1 in the Matroska file is an upmix of the stereo original, made for the
channel count and not for listening. `portrait.mov` is the centre of the frame,
cropped to portrait.
