# stratus-backend

The Stratus server: one Go binary, one container, no sidecars.

Stratus is a self-hosted personal cloud for photos, calendar, music and video.
Instead of shipping its own API and a client app per platform, it speaks
protocols your existing apps already understand.

## Protocols

| Protocol | Use | Works with |
|---|---|---|
| WebDAV | files, photo backup, sync | rclone, Finder, Nautilus, DAVx5, FolderSync |
| CalDAV | calendar | DAVx5, Thunderbird, iOS/macOS |
| OpenSubsonic | music | Symfonium, Substreamer, DSub, Feishin |
| HTTP range | audio/video streaming | browsers, VLC, mpv |
| CardDAV | contacts | *planned* |
| DLNA / UPnP-AV | TVs, set-top players | *planned* |

## Pluggable backends

Two seams, and only two:

- **Blob storage** — `disk` and `s3` at launch; ftp and others later.
- **Metadata database** — `sqlite` at launch (pure Go, CGO-free); postgres and
  mysql later. No driver-specific SQL leaves its driver package.

## Build

```sh
go build ./cmd/stratus
```

## Status

Greenfield — the skeleton above is all there is so far. Work is tracked on the
[Stratus project board](https://github.com/users/C0piIot/projects/2).
