# Coolify

Coolify runs on your own server, so there is nothing to pay anybody and no
button: a resource pointed at this repository and one file in it.

1. **New Resource → Public Repository**, URL
   `https://github.com/C0piIot/stratus-backend`, branch `main`.
2. **Build Pack: Docker Compose**, Base Directory `/`, Docker Compose Location
   `/deploy/coolify/compose.yaml`.
3. Fill in `STRATUS_USERNAME` under Environment Variables, give the service a
   domain, and deploy.

[`deploy/coolify/compose.yaml`](../../deploy/coolify/compose.yaml) runs the
published image on a named volume, which holds the blobs and the SQLite
database both. It is not the `compose.yaml` at the root, which builds the image
and binds `./data` for development.

## Credentials

`STRATUS_USERNAME` is required and Coolify's form marks it so. The password is
`SERVICE_PASSWORD_STRATUS`, which Coolify generates on the first deploy and
keeps across the next ones; it is on the same Environment Variables page, and
changing it there signs every client and browser out, as a password change
does anywhere.

**A deploy through Coolify's API does not stop for the missing username**, only
the form does: the server then refuses to start, saying a password is set and a
username is not.

## Things worth knowing

- **The volume is writable because of the image, not the platform.** A fresh
  named volume takes the ownership of the directory it is mounted over, and the
  image carries an empty `/data` owned by the user it runs as, 65532. An image
  older than that refuses to start with `/data is not writable` in its log.
- **Upgrading is Redeploy**: `:main` is a moving tag, and Coolify pulls it on
  every deploy. The volume is kept.
- **TLS is Coolify's proxy**, which forwards the scheme, so the session cookie
  comes back `Secure` behind a domain with a certificate.

## What a real deploy answered

October 2026, on Coolify 4 with no proxy, from this file (#177): the username
was required and the password generated, the volume came up owned by 65532, a
WebDAV `PUT` landed and an unauthenticated `PROPFIND` was refused, and the file
was still there after the container restarted. The published image of the time
refused to start on the same volume, which is what put `/data` in the image.
Through the proxy and a certificate is the part not yet tried.
