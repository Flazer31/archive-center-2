package ingest

// Forget removes a chat's acknowledged texts and its lazy-load marker.
// The pipeline owns synchronization, just as for Warm and resolve.
func (c *TextCache) Forget(chat string) {
	for key := range c.texts {
		if key.Chat == chat {
			delete(c.texts, key)
		}
	}
	delete(c.loaded, chat)
	delete(c.dropped, chat)
}
