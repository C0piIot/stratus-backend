package lookup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// The services, as the README names them.
const (
	musicBrainzURL = "https://musicbrainz.org"
	coverArtURL    = "https://coverartarchive.org"
	lrclibURL      = "https://lrclib.net"
)

// requestTimeout bounds one request. A cover from the archive is a redirect
// to archive.org and a few hundred kilobytes; nothing here should take longer.
const requestTimeout = 30 * time.Second

// userAgent is what MusicBrainz asks every client to send: a name, a version
// and a way to reach whoever runs it. The project's page is the contact,
// since that is where an operator's complaint would be filed.
func userAgent(version string) string {
	return "Stratus/" + version + " ( https://github.com/C0piIot/stratus-backend )"
}

// What a request can come back with, other than what was asked for.
var (
	// errNothing is a source saying it has nothing for this: a verdict, asked
	// about again in a month.
	errNothing = errors.New("nothing there")
	// errRefused is a source turning requests away -- 503 from MusicBrainz is
	// its rate limit, 429 is anybody's -- and the pass stops on it.
	errRefused = errors.New("refused")
	// errUnreachable is anything else that says nothing about the album: the
	// network, a timeout, a server error.
	errUnreachable = errors.New("could not be reached")
)

// get fetches one URL from one service, paced, and answers the body of a 2xx.
//
// Every failure is classified here, once, because the three callers would
// otherwise each decide what a 503 means. limit bounds the body, and a body
// past it is refused as not what was asked for.
func (s *Service) get(ctx context.Context, service, rawURL string, limit int64, accept string) ([]byte, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errUnreachable, err)
	}
	if werr := s.pace.wait(ctx, service); werr != nil {
		return nil, werr
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errUnreachable, err)
	}
	req.Header.Set("User-Agent", s.agent)
	if accept != "" {
		req.Header.Set("Accept", accept)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("%w: %s: %w", errUnreachable, service, err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, fmt.Errorf("%w: %s answered 404", errNothing, service)
	case resp.StatusCode == http.StatusServiceUnavailable, resp.StatusCode == http.StatusTooManyRequests:
		return nil, fmt.Errorf("%w: %s answered %d", errRefused, service, resp.StatusCode)
	case resp.StatusCode >= 500:
		return nil, fmt.Errorf("%w: %s answered %d", errUnreachable, service, resp.StatusCode)
	case resp.StatusCode >= 400:
		// A 400 is this server's own request being wrong -- a query the
		// service could not parse -- and asking again in an hour would be
		// the same question. Treated as nothing there, with the status in
		// the row for whoever reads it.
		return nil, fmt.Errorf("%w: %s answered %d", errNothing, service, resp.StatusCode)
	case resp.StatusCode < 200 || resp.StatusCode >= 300:
		return nil, fmt.Errorf("%w: %s answered %d", errUnreachable, service, resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("%w: reading from %s: %w", errUnreachable, service, err)
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("%w: %s answered more than %d bytes", errNothing, service, limit)
	}
	return body, nil
}
