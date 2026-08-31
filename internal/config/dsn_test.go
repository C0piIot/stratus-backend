package config_test

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/C0piIot/stratus-backend/internal/config"
)

// The credentials used throughout, so every leak assertion looks for the same
// two distinctive strings.
const (
	accessKey = "AKIAIOSFODNN7EXAMPLE"
	secretKey = "wJalrXUtnFEMI-K7MDENG-bPxRfiCYEXAMPLEKEY"
)

func s3DSN(suffix string) string {
	return "s3://" + accessKey + ":" + secretKey + "@s3.eu-west-1.amazonaws.com/photos" + suffix
}

func TestParseFileDSN(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{name: "absolute path", raw: "file:///data/blobs", want: "/data/blobs"},
		{name: "root", raw: "file:///", want: "/"},
		{name: "trailing slash is kept as given", raw: "file:///srv/stratus/", want: "/srv/stratus/"},
		{name: "percent escapes are decoded", raw: "file:///srv/my%20blobs", want: "/srv/my blobs"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := config.ParseStorageDSN(tt.raw)
			if err != nil {
				t.Fatalf("ParseStorageDSN: %v", err)
			}
			if got.Scheme != config.SchemeFile {
				t.Errorf("Scheme = %q, want %q", got.Scheme, config.SchemeFile)
			}
			if got.Dir != tt.want {
				t.Errorf("Dir = %q, want %q", got.Dir, tt.want)
			}
		})
	}
}

func TestParseS3DSN(t *testing.T) {
	t.Parallel()

	got, err := config.ParseStorageDSN(s3DSN("?region=eu-west-1"))
	if err != nil {
		t.Fatalf("ParseStorageDSN: %v", err)
	}
	if got.Scheme != config.SchemeS3 {
		t.Errorf("Scheme = %q, want %q", got.Scheme, config.SchemeS3)
	}
	if got.Endpoint != "s3.eu-west-1.amazonaws.com" {
		t.Errorf("Endpoint = %q", got.Endpoint)
	}
	if got.Bucket != "photos" {
		t.Errorf("Bucket = %q, want photos", got.Bucket)
	}
	if got.AccessKey != accessKey {
		t.Errorf("AccessKey = %q", got.AccessKey)
	}
	if got.SecretKey.Reveal() != secretKey {
		t.Error("SecretKey does not round trip")
	}
	if got.Region != "eu-west-1" {
		t.Errorf("Region = %q, want eu-west-1", got.Region)
	}
	// TLS is the default, since the DSN in the documentation is an AWS one.
	if !got.UseTLS {
		t.Error("UseTLS = false, want true by default")
	}
}

func TestParseS3DSNOptions(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		raw     string
		wantTLS bool
	}{
		{name: "tls off for a local MinIO", raw: s3DSN("?tls=false"), wantTLS: false},
		{name: "tls on explicitly", raw: s3DSN("?tls=true"), wantTLS: true},
		{name: "tls accepts 0", raw: s3DSN("?tls=0"), wantTLS: false},
		{name: "no parameters at all", raw: s3DSN(""), wantTLS: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := config.ParseStorageDSN(tt.raw)
			if err != nil {
				t.Fatalf("ParseStorageDSN: %v", err)
			}
			if got.UseTLS != tt.wantTLS {
				t.Errorf("UseTLS = %v, want %v", got.UseTLS, tt.wantTLS)
			}
		})
	}
}

func TestParseS3DSNEscapedSecret(t *testing.T) {
	t.Parallel()
	// A secret key containing / and + is ordinary for S3, and both have to
	// survive being percent-encoded in a URL.
	const awkward = "a/b+c=d"
	got, err := config.ParseStorageDSN("s3://key:a%2Fb%2Bc%3Dd@localhost:9000/bucket")
	if err != nil {
		t.Fatalf("ParseStorageDSN: %v", err)
	}
	if got.SecretKey.Reveal() != awkward {
		t.Errorf("SecretKey = %q, want %q", got.SecretKey.Reveal(), awkward)
	}
}

func TestParseStorageDSNRejects(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		raw  string
	}{
		{name: "empty", raw: ""},
		{name: "blank", raw: "   "},
		{name: "no scheme", raw: "/data/blobs"},
		{name: "unknown scheme", raw: "ftp://example.com/blobs"},
		{name: "postgres, which is the other seam", raw: "postgres://localhost/stratus"},
		{name: "file with two slashes", raw: "file://data/blobs"},
		{name: "file with a relative path", raw: "file:data/blobs"},
		{name: "file with credentials", raw: "file://user:pass@/data"},
		{name: "file with parameters", raw: "file:///data?sync=true"},
		{name: "s3 without a host", raw: "s3:///bucket"},
		{name: "s3 without a bucket", raw: "s3://key:secret@localhost:9000"},
		{name: "s3 with an empty bucket", raw: "s3://key:secret@localhost:9000/"},
		{name: "s3 with a path inside the bucket", raw: "s3://key:secret@localhost:9000/bucket/prefix"},
		{name: "s3 with no credentials", raw: "s3://localhost:9000/bucket"},
		{name: "s3 with no secret", raw: "s3://key@localhost:9000/bucket"},
		{name: "s3 with an empty secret", raw: "s3://key:@localhost:9000/bucket"},
		{name: "s3 with an unknown parameter", raw: s3DSN("?regoin=eu-west-1")},
		{name: "s3 with a non-boolean tls", raw: s3DSN("?tls=maybe")},
		{name: "s3 with a repeated parameter", raw: s3DSN("?region=a&region=b")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, err := config.ParseStorageDSN(tt.raw); err == nil {
				t.Errorf("ParseStorageDSN(%q) = nil, want an error", tt.raw)
			}
		})
	}
}

// TestParseErrorsNeverLeakTheSecret is the test issue #7 asks for by name. An
// error message is a log line waiting to happen, and a DSN carries credentials.
func TestParseErrorsNeverLeakTheSecret(t *testing.T) {
	t.Parallel()
	broken := []string{
		s3DSN("?regoin=eu-west-1"),
		s3DSN("?tls=maybe"),
		s3DSN("/prefix/too/deep"),
		"s3://" + accessKey + ":" + secretKey + "@/no-host",
		"s3://" + accessKey + ":" + secretKey + "@host:9000",
		// A URL that url.Parse itself rejects: its error embeds the input.
		"s3://" + accessKey + ":" + secretKey + "@host:9000/bucket?x=%zz",
		"://" + accessKey + ":" + secretKey + "@host/bucket",
	}
	for _, raw := range broken {
		t.Run(raw[:min(len(raw), 24)], func(t *testing.T) {
			t.Parallel()
			_, err := config.ParseStorageDSN(raw)
			if err == nil {
				t.Fatalf("ParseStorageDSN(%q) = nil, want an error", raw)
			}
			if strings.Contains(err.Error(), secretKey) {
				t.Errorf("error leaks the secret key: %v", err)
			}
			if strings.Contains(err.Error(), accessKey) {
				t.Errorf("error leaks the access key: %v", err)
			}
		})
	}
}

// TestDSNNeverPrintsItsSecret covers every way a value normally reaches a log:
// the fmt verbs, and slog reflecting over a struct.
func TestDSNNeverPrintsItsSecret(t *testing.T) {
	t.Parallel()
	dsn, err := config.ParseStorageDSN(s3DSN("?region=eu-west-1"))
	if err != nil {
		t.Fatal(err)
	}

	for _, format := range []string{"%s", "%v", "%q", "%#v", "%+v"} {
		if got := fmt.Sprintf(format, dsn); strings.Contains(got, secretKey) {
			t.Errorf("%s leaks the secret: %s", format, got)
		}
	}
	// The access key is not a secret and stays legible, or the redacted form
	// would be useless for telling two configurations apart.
	if got := dsn.String(); !strings.Contains(got, accessKey) {
		t.Errorf("String() = %q, want it to keep the access key", got)
	}

	var buf bytes.Buffer
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("storage ready", "dsn", dsn, "secret", dsn.SecretKey)
	if strings.Contains(buf.String(), secretKey) {
		t.Errorf("slog leaks the secret: %s", buf.String())
	}
}

// TestConfigNeverPrintsItsSecrets is the realistic version: somebody logs the
// whole configuration struct.
func TestConfigNeverPrintsItsSecrets(t *testing.T) {
	t.Parallel()
	cfg, err := config.Load(env(map[string]string{
		"STRATUS_STORAGE_DSN":   s3DSN(""),
		"STRATUS_DB_DSN":        pgDSN(""),
		"STRATUS_USERNAME":      "edu",
		"STRATUS_PASSWORD_HASH": "$2a$10$abcdefghijklmnopqrstuv",
	}))
	if err != nil {
		t.Fatal(err)
	}

	for _, format := range []string{"%v", "%+v", "%#v"} {
		got := fmt.Sprintf(format, cfg)
		if strings.Contains(got, secretKey) {
			t.Errorf("%s leaks the storage secret: %s", format, got)
		}
		if strings.Contains(got, "abcdefghijklmnopqrstuv") {
			t.Errorf("%s leaks the password hash: %s", format, got)
		}
		if strings.Contains(got, dbPassword) {
			t.Errorf("%s leaks the database password: %s", format, got)
		}
	}
}

func TestSecret(t *testing.T) {
	t.Parallel()
	const value = "not in a log"
	secret := config.Secret(value)

	if secret.Reveal() != value {
		t.Errorf("Reveal = %q, want %q", secret.Reveal(), value)
	}
	for _, format := range []string{"%s", "%v", "%q", "%#v"} {
		if got := fmt.Sprintf(format, secret); strings.Contains(got, value) {
			t.Errorf("%s leaks the secret: %s", format, got)
		}
	}
	// An empty secret must still compare as empty, which is how the credential
	// check tells "unset" from "set".
	if config.Secret("") != "" {
		t.Error("an empty Secret does not compare equal to the empty string")
	}
}

func TestLoadStorageDSN(t *testing.T) {
	t.Parallel()

	t.Run("defaults to blobs under the data dir", func(t *testing.T) {
		t.Parallel()
		cfg := load(t, map[string]string{"STRATUS_DATA_DIR": "/srv/stratus"})
		if cfg.Storage.Scheme != config.SchemeFile {
			t.Errorf("Scheme = %q", cfg.Storage.Scheme)
		}
		if cfg.Storage.Dir != "/srv/stratus/"+config.DefaultBlobDir {
			t.Errorf("Dir = %q", cfg.Storage.Dir)
		}
	})

	t.Run("a data dir needing escapes still parses back", func(t *testing.T) {
		t.Parallel()
		cfg := load(t, map[string]string{"STRATUS_DATA_DIR": "/srv/my data"})
		if cfg.Storage.Dir != "/srv/my data/"+config.DefaultBlobDir {
			t.Errorf("Dir = %q", cfg.Storage.Dir)
		}
	})

	t.Run("the DSN wins over the data dir", func(t *testing.T) {
		t.Parallel()
		cfg := load(t, map[string]string{
			"STRATUS_DATA_DIR":    "/srv/stratus",
			"STRATUS_STORAGE_DSN": "file:///mnt/photos",
		})
		if cfg.Storage.Dir != "/mnt/photos" {
			t.Errorf("Dir = %q, want the DSN to win", cfg.Storage.Dir)
		}
	})

	t.Run("a broken DSN names the variable but not the secret", func(t *testing.T) {
		t.Parallel()
		_, err := config.Load(env(map[string]string{"STRATUS_STORAGE_DSN": s3DSN("?tls=maybe")}))
		if err == nil {
			t.Fatal("Load = nil, want an error")
		}
		if !strings.Contains(err.Error(), "STRATUS_STORAGE_DSN") {
			t.Errorf("error should name the variable, got %v", err)
		}
		if strings.Contains(err.Error(), secretKey) {
			t.Errorf("error leaks the secret: %v", err)
		}
	})
}

func TestLoadCredentials(t *testing.T) {
	t.Parallel()
	cfg := load(t, map[string]string{
		"STRATUS_USERNAME":      "edu",
		"STRATUS_PASSWORD_HASH": "$2a$10$hash",
	})
	if cfg.Username != "edu" {
		t.Errorf("Username = %q", cfg.Username)
	}
	if cfg.PasswordHash.Reveal() != "$2a$10$hash" {
		t.Errorf("PasswordHash does not round trip")
	}
	if errors.Is(nil, nil) && cfg.PasswordHash.String() == "$2a$10$hash" {
		t.Error("the hash prints itself")
	}
}

const dbPassword = "hunter2-but-longer"

func pgDSN(suffix string) string {
	return "postgres://stratus:" + dbPassword + "@db.internal:5432/stratus" + suffix
}

func TestParseSQLiteDSN(t *testing.T) {
	t.Parallel()
	got, err := config.ParseDatabaseDSN("sqlite:///data/stratus.db")
	if err != nil {
		t.Fatalf("ParseDatabaseDSN: %v", err)
	}
	if got.Scheme != config.SchemeSQLite {
		t.Errorf("Scheme = %q", got.Scheme)
	}
	if got.Path != "/data/stratus.db" {
		t.Errorf("Path = %q", got.Path)
	}
}

func TestParsePostgresDSN(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		raw  string
	}{
		{name: "plain", raw: pgDSN("")},
		{name: "with parameters", raw: pgDSN("?sslmode=require&connect_timeout=5")},
		// Parameters are passed through, not allow-listed: pgx knows its own
		// set and rejects the rest when it connects.
		{name: "with an exotic parameter", raw: pgDSN("?application_name=stratus&search_path=public")},
		{name: "the postgresql alias", raw: strings.Replace(pgDSN(""), "postgres://", "postgresql://", 1)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := config.ParseDatabaseDSN(tt.raw)
			if err != nil {
				t.Fatalf("ParseDatabaseDSN: %v", err)
			}
			if got.Scheme != config.SchemePostgres {
				t.Errorf("Scheme = %q, want it normalised to %q", got.Scheme, config.SchemePostgres)
			}
			if !strings.Contains(got.ConnString.Reveal(), dbPassword) {
				t.Error("the connection string lost its password")
			}
			if !strings.HasPrefix(got.ConnString.Reveal(), "postgres://") {
				t.Errorf("the connection string is not a postgres URL: %q", got.ConnString.Reveal())
			}
		})
	}
}

func TestParseDatabaseDSNRejects(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		raw  string
	}{
		{name: "empty", raw: ""},
		{name: "blank", raw: "   "},
		{name: "no scheme", raw: "/data/stratus.db"},
		{name: "unknown scheme", raw: "mongodb://localhost/stratus"},
		{name: "mysql, which is not implemented", raw: "mysql://user:pass@localhost/stratus"},
		{name: "file, which is the other seam", raw: "file:///data/blobs"},
		{name: "sqlite with two slashes", raw: "sqlite://data/stratus.db"},
		{name: "sqlite with a relative path", raw: "sqlite:data/stratus.db"},
		{name: "sqlite with credentials", raw: "sqlite://user:pass@/data/stratus.db"},
		// The pragmas are correctness, not preference, so there is nothing to
		// tune and a parameter here means somebody misunderstood.
		{name: "sqlite with parameters", raw: "sqlite:///data/stratus.db?_busy_timeout=5000"},
		{name: "postgres without a host", raw: "postgres:///stratus"},
		{name: "postgres without a database", raw: "postgres://user:pass@localhost:5432"},
		{name: "postgres with an empty database", raw: "postgres://user:pass@localhost:5432/"},
		{name: "postgres with a path too deep", raw: "postgres://user:pass@localhost:5432/stratus/public"},
		// url.Parse itself rejects this one, and its error embeds the input.
		{name: "not a URL at all", raw: "postgres://ho%zzst/stratus"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, err := config.ParseDatabaseDSN(tt.raw); err == nil {
				t.Errorf("ParseDatabaseDSN(%q) = nil, want an error", tt.raw)
			}
		})
	}
}

func TestDatabaseDSNNeverPrintsItsPassword(t *testing.T) {
	t.Parallel()
	dsn, err := config.ParseDatabaseDSN(pgDSN("?sslmode=require"))
	if err != nil {
		t.Fatal(err)
	}

	for _, format := range []string{"%s", "%v", "%q", "%#v", "%+v"} {
		if got := fmt.Sprintf(format, dsn); strings.Contains(got, dbPassword) {
			t.Errorf("%s leaks the password: %s", format, got)
		}
	}
	// The user and the host stay legible, or the redacted form would be
	// useless for telling two configurations apart in a log.
	if got := dsn.String(); !strings.Contains(got, "stratus:REDACTED@db.internal:5432") {
		t.Errorf("String() = %q, want it to keep everything but the password", got)
	}

	var buf bytes.Buffer
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("database ready", "dsn", dsn)
	if strings.Contains(buf.String(), dbPassword) {
		t.Errorf("slog leaks the password: %s", buf.String())
	}
}

func TestLoadDatabaseDSN(t *testing.T) {
	t.Parallel()

	t.Run("defaults to a file under the data dir", func(t *testing.T) {
		t.Parallel()
		cfg := load(t, map[string]string{"STRATUS_DATA_DIR": "/srv/stratus"})
		if cfg.Database.Scheme != config.SchemeSQLite {
			t.Errorf("Scheme = %q", cfg.Database.Scheme)
		}
		if cfg.Database.Path != "/srv/stratus/"+config.DefaultDBFile {
			t.Errorf("Path = %q", cfg.Database.Path)
		}
	})

	t.Run("the DSN wins over the data dir", func(t *testing.T) {
		t.Parallel()
		cfg := load(t, map[string]string{
			"STRATUS_DATA_DIR": "/srv/stratus",
			"STRATUS_DB_DSN":   "sqlite:///mnt/metadata.db",
		})
		if cfg.Database.Path != "/mnt/metadata.db" {
			t.Errorf("Path = %q", cfg.Database.Path)
		}
	})

	t.Run("a broken DSN names the variable but not the password", func(t *testing.T) {
		t.Parallel()
		_, err := config.Load(env(map[string]string{"STRATUS_DB_DSN": pgDSN("/too/deep")}))
		if err == nil {
			t.Fatal("Load = nil, want an error")
		}
		if !strings.Contains(err.Error(), "STRATUS_DB_DSN") {
			t.Errorf("error should name the variable, got %v", err)
		}
		if strings.Contains(err.Error(), dbPassword) {
			t.Errorf("error leaks the password: %v", err)
		}
	})
}
