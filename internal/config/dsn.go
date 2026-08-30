package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strconv"
	"strings"
)

// Schemes understood by STRATUS_STORAGE_DSN. The scheme selects the backend,
// which is what makes "pluggable at exactly two seams" a property of the
// configuration rather than a convention.
const (
	SchemeFile = "file"
	SchemeS3   = "s3"
)

const redacted = "REDACTED"

// Secret is a string that refuses to print itself. Every way of rendering a
// value that fmt and slog reach for goes through one of these methods, so a
// secret can only escape into a log through an explicit Reveal.
type Secret string

// String implements fmt.Stringer, covering %s, %q and %v.
func (s Secret) String() string { return redacted }

// GoString implements fmt.GoStringer, covering %#v.
func (s Secret) GoString() string { return redacted }

// LogValue implements slog.LogValuer, covering every structured log call.
func (s Secret) LogValue() slog.Value { return slog.StringValue(redacted) }

// Reveal returns the secret itself. It is a method rather than a conversion so
// that every place a secret leaves this package is one grep away.
func (s Secret) Reveal() string { return string(s) }

// StorageDSN is a parsed STRATUS_STORAGE_DSN. Backends receive this, never the
// string it came from.
type StorageDSN struct {
	// Scheme is SchemeFile or SchemeS3.
	Scheme string

	// Dir is the blob directory, for file.
	Dir string

	// Endpoint through Region describe an S3 bucket.
	Endpoint  string
	Bucket    string
	AccessKey string
	SecretKey Secret
	Region    string
	UseTLS    bool

	// safe is the DSN with its secrets replaced, built at parse time so that
	// the only form anything else can hold is already redacted.
	safe string
}

// String returns the DSN with its secrets removed.
func (d StorageDSN) String() string { return d.safe }

// LogValue keeps a structured log line from reflecting over the fields.
func (d StorageDSN) LogValue() slog.Value { return slog.StringValue(d.safe) }

// ParseStorageDSN parses a storage DSN.
//
// No error it returns includes the DSN, not even the part that failed to parse.
// A DSN carries credentials, errors end up in logs, and "which character was
// wrong" is not worth a leaked secret key. That includes the error from
// url.Parse, which embeds its input.
func ParseStorageDSN(raw string) (StorageDSN, error) {
	if strings.TrimSpace(raw) == "" {
		return StorageDSN{}, errors.New("storage DSN is empty")
	}

	u, err := url.Parse(raw)
	if err != nil {
		return StorageDSN{}, errors.New("storage DSN is not a valid URL")
	}

	switch u.Scheme {
	case SchemeFile:
		return parseFileDSN(u)
	case SchemeS3:
		return parseS3DSN(u)
	case "":
		return StorageDSN{}, fmt.Errorf("storage DSN has no scheme; use %s:// or %s://", SchemeFile, SchemeS3)
	default:
		return StorageDSN{}, fmt.Errorf("unsupported storage scheme %q; use %s or %s", u.Scheme, SchemeFile, SchemeS3)
	}
}

func parseFileDSN(u *url.URL) (StorageDSN, error) {
	switch {
	case u.Host != "":
		// file://data/blobs is the classic two-slash mistake: everything up to
		// the next slash is a host, so the path silently loses a segment.
		return StorageDSN{}, errors.New("file DSN needs three slashes: file:///path, not file://path")
	case u.User != nil:
		return StorageDSN{}, errors.New("file DSN takes no credentials")
	case u.RawQuery != "":
		return StorageDSN{}, errors.New("file DSN takes no parameters")
	case !strings.HasPrefix(u.Path, "/"):
		return StorageDSN{}, errors.New("file DSN needs an absolute path")
	}
	return StorageDSN{Scheme: SchemeFile, Dir: u.Path, safe: SchemeFile + "://" + u.Path}, nil
}

func parseS3DSN(u *url.URL) (StorageDSN, error) {
	if u.Host == "" {
		return StorageDSN{}, errors.New("s3 DSN needs an endpoint host")
	}

	bucket := strings.Trim(u.Path, "/")
	if bucket == "" || strings.Contains(bucket, "/") {
		return StorageDSN{}, errors.New("s3 DSN needs exactly one path segment, the bucket")
	}

	if u.User == nil {
		return StorageDSN{}, errors.New("s3 DSN needs an access key and a secret key")
	}
	access := u.User.Username()
	secret, ok := u.User.Password()
	if access == "" || !ok || secret == "" {
		return StorageDSN{}, errors.New("s3 DSN needs an access key and a secret key")
	}

	dsn := StorageDSN{
		Scheme:    SchemeS3,
		Endpoint:  u.Host,
		Bucket:    bucket,
		AccessKey: access,
		SecretKey: Secret(secret),
		UseTLS:    true,
	}

	// url.URL.Query() silently drops whatever it cannot parse, which would turn
	// a malformed parameter into no parameter at all -- the same failure the
	// strictness below exists to prevent.
	query, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		// Not wrapped: ParseQuery quotes the offending fragment back.
		return StorageDSN{}, errors.New("s3 DSN has a malformed query string")
	}

	// Unknown parameters are an error rather than a shrug: a typo in "region"
	// would otherwise mean the wrong region, discovered as a redirect much
	// later.
	for key, values := range query {
		if len(values) > 1 {
			return StorageDSN{}, fmt.Errorf("s3 DSN repeats the %q parameter", key)
		}
		switch key {
		case "region":
			dsn.Region = values[0]
		case "tls":
			tls, err := strconv.ParseBool(values[0])
			if err != nil {
				return StorageDSN{}, errors.New(`s3 DSN parameter "tls" must be true or false`)
			}
			dsn.UseTLS = tls
		default:
			return StorageDSN{}, fmt.Errorf("unknown s3 DSN parameter %q", key)
		}
	}

	// Rebuilt rather than string-replaced: the redacted form has to be right
	// even when the secret is the empty-looking result of some escaping.
	safe := *u
	safe.User = url.UserPassword(access, redacted)
	dsn.safe = safe.String()
	return dsn, nil
}
