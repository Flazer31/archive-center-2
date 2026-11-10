package store

import "archivecenter2/goservice/internal/memory"

// chatMemoryCounts counts exactly what the management list reads. Load only
// this chat, without warming the pipeline cache; only its counts survive.
func (d *DB) chatMemoryCounts(key string) (map[string]int, error) {
	counts := map[string]int{}
	s, found, err := d.LoadSession(key)
	if err != nil || !found {
		return counts, err
	}
	listed, err := d.ChatMemories(key, memory.Own(s))
	if err != nil {
		return nil, err
	}
	for _, m := range listed {
		counts[m.Kind]++
	}
	return counts, nil
}
