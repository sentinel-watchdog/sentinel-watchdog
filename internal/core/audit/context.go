package audit

import "context"

// correlationKey is the context key of a request's correlation id.
type correlationKey struct{}

// WithCorrelation returns ctx carrying id, the correlation id of the
// request whose intent was recorded: an action that records its own
// records on behalf of that request links them to it.
func WithCorrelation(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, correlationKey{}, id)
}

// CorrelationFrom returns the correlation id WithCorrelation put in ctx,
// or "".
func CorrelationFrom(ctx context.Context) string {
	id, _ := ctx.Value(correlationKey{}).(string)
	return id
}
