package observe

// Reset serializes maintenance with the complete raw-write/apply operation.
// Token configuration is deliberately preserved.
func (s *Server) Reset(reset func() error) error {
	s.applyMu.Lock()
	defer s.applyMu.Unlock()
	if err := reset(); err != nil {
		return err
	}
	s.mu.Lock()
	s.seq, s.received, s.failures, s.lastError = 0, 0, 0, ""
	s.mu.Unlock()
	return nil
}
