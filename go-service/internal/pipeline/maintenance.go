package pipeline

import (
	"archivecenter2/goservice/internal/ingest"
	"archivecenter2/goservice/internal/lifecycle"
)

// Reset clears observation-derived memory only after the database reset
// succeeds. The existing text-loader closure still refers to this pipeline.
func (p *Pipeline) Reset(reset func() error) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := reset(); err != nil {
		return err
	}
	p.sessions = map[string]*lifecycle.Session{}
	p.labels = nil
	load := p.texts.Load
	p.texts = ingest.NewTextCache()
	p.texts.Load = load
	p.applied, p.resent, p.failures, p.lastError = 0, 0, 0, ""
	return nil
}
