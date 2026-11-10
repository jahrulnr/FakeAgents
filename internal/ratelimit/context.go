package ratelimit

import "context"

type decisionKey struct{}

// WithDecision stores the request-limit decision for handlers and logging.
func WithDecision(ctx context.Context, decision Decision) context.Context {
	return context.WithValue(ctx, decisionKey{}, decision)
}

// DecisionFrom retrieves a stored request-limit decision. The zero value is
// allowed with no limit.
func DecisionFrom(ctx context.Context) Decision {
	decision, _ := ctx.Value(decisionKey{}).(Decision)
	if decision.Limit == 0 {
		decision.Allowed = true
	}
	return decision
}
