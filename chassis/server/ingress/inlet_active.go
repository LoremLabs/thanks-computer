package ingress

// InletActive reports whether tenant has an active nested inlet stack
// named stack (`<stack>/_<inlet>`, with ops), the way a hostname-routed
// TCP connection is admitted to `<stack>/_tcp`. The web head's markdown
// negotiation asks it about `<stack>/_markdown`. Route cache first; err is
// a transient lookup failure, never a miss.
func (r *DBResolver) InletActive(tenant, stack string) (bool, error) {
	return r.inletActive(tenant, stack)
}
