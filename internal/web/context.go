package web

import (
	"context"

	"github.com/C0piIot/stratus-backend/internal/auth"
)

// shareKey is unexported for the reason auth.User's is: only the gate that
// verified a link may say a request arrived through one.
type shareKey struct{}

// withShare marks a request as having arrived on a signed link.
func withShare(ctx context.Context, share auth.Share) context.Context {
	return context.WithValue(ctx, shareKey{}, share)
}

// shareOf returns the link a request came in on, if it came in on one.
//
// It exists for one thing: a page rendered from a link has to know where the
// link starts, so its breadcrumbs can walk back to the share and no further.
func shareOf(ctx context.Context) (auth.Share, bool) {
	share, ok := ctx.Value(shareKey{}).(auth.Share)
	return share, ok
}
