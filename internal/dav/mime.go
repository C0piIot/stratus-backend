package dav

import "mime"

// typeByExtension is mime.TypeByExtension with the extensions this project
// cares about pinned, because the stdlib table is small and, on Linux, is
// extended from /etc/mime.types -- which a distroless image does not have. A
// photo served as application/octet-stream is a photo a browser will not show.
var pinned = map[string]string{
	".heic": "image/heic",
	".heif": "image/heif",
	".dng":  "image/x-adobe-dng",
	".cr2":  "image/x-canon-cr2",
	".nef":  "image/x-nikon-nef",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".png":  "image/png",
	".gif":  "image/gif",
	".webp": "image/webp",
	".mp4":  "video/mp4",
	".mov":  "video/quicktime",
	".mkv":  "video/x-matroska",
	".webm": "video/webm",
	".flac": "audio/flac",
	".mp3":  "audio/mpeg",
	".m4a":  "audio/mp4",
	".ogg":  "audio/ogg",
	".opus": "audio/opus",
	".ics":  "text/calendar",
	".txt":  "text/plain; charset=utf-8",
	".pdf":  "application/pdf",
}

func typeByExtension(ext string) string {
	if t, ok := pinned[lower(ext)]; ok {
		return t
	}
	return mime.TypeByExtension(ext)
}

func lower(s string) string {
	out := []byte(s)
	for i, c := range out {
		if c >= 'A' && c <= 'Z' {
			out[i] = c + ('a' - 'A')
		}
	}
	return string(out)
}
