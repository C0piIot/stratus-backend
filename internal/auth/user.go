package auth

import "context"

// userKey is unexported so that nothing outside this package can put a user in
// a context. Authentication is the only thing entitled to say who is calling.
type userKey struct{}

// WithUser marks a context as belonging to an authenticated user.
func WithUser(ctx context.Context, username string) context.Context {
	return context.WithValue(ctx, userKey{}, username)
}

// User returns who the request was authenticated as.
//
// This is how an adapter learns whose files it is serving. Taking it from
// configuration instead would work exactly once: the day there is a second user
// the answer has to come from the request, and every handler that read it from
// somewhere else would have to be rewritten.
func User(ctx context.Context) (string, bool) {
	username, ok := ctx.Value(userKey{}).(string)
	return username, ok && username != ""
}
