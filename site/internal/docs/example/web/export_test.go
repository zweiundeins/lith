package web

import "github.com/zweiundeins/lith/edge"

// WithLimit gives s another limiter for the actions, before it serves.
func WithLimit(s *Server, limit *edge.Limiter) *Server {
	s.limit = limit
	return s
}
