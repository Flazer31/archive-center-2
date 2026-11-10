package lifecycle

// Restore rebuilds a Session from stored turns, so a restart continues the
// same Go-issued IDs and links instead of treating stored rows as new input.
func Restore(key string, turns []*Turn, progress map[string]int64) *Session {
	s := NewSession(key)
	for b, q := range progress {
		s.Progress[b] = q
	}
	s.Turns = turns

	for _, t := range turns {
		if t.ID >= s.nextTurnID {
			s.nextTurnID = t.ID + 1
		}
		for _, m := range t.Members {
			s.memberTurn[m.Key] = t
			s.member[m.Key] = m
		}
		for _, r := range t.Responses {
			s.respTurn[r.Key] = t
			s.respRow[r.Key] = r
			for _, rev := range r.Revisions {
				if rev.Seq > s.revSeq {
					s.revSeq = rev.Seq
				}
			}
		}
	}
	return s
}
