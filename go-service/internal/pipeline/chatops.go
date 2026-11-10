package pipeline

import "strings"

// ForgetLocked invalidates a stored session after a chat operation commits.
// Call only inside WithSession (the observation lock is already held).
func (p *Pipeline) ForgetLocked(key string) {
	delete(p.sessions, key)
	delete(p.labels, key)
	p.texts.Forget(strings.TrimPrefix(key, "chat:"))
}
