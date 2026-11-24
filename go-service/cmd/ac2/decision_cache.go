package main

import (
	"crypto/sha256"
	"encoding/binary"
	"slices"
	"sync"

	"archivecenter2/goservice/internal/provider"
)

// About 330 questions per scene in the recorded workload. Bound both
// the number of scenes and the total answers across unusually wide scenes.
const decisionCacheScenes = 8
const decisionCacheAnswers = 4096

type decisionDigest = [sha256.Size]byte

func decisionHash(parts ...string) decisionDigest {
	h := sha256.New()
	var length [8]byte
	for _, part := range parts {
		binary.BigEndian.PutUint64(length[:], uint64(len(part)))
		h.Write(length[:])
		h.Write([]byte(part))
	}
	var sum decisionDigest
	h.Sum(sum[:0])
	return sum
}

func decisionStateKey(dec *provider.Decider, state string) decisionDigest {
	service, endpoint, model := dec.Connection()
	return decisionHash(service, endpoint, model, state)
}

type sceneScores struct {
	key    decisionDigest
	scores map[decisionDigest]float64
}

type decisionCache struct {
	mu     sync.Mutex
	scenes []sceneScores // oldest first, ordered by first saved answer
	count  int
}

// scene is called with mu held. At most eight digests are compared.
func (c *decisionCache) scene(key decisionDigest) map[decisionDigest]float64 {
	for _, s := range c.scenes {
		if s.key == key {
			return s.scores
		}
	}
	return nil
}

func (c *decisionCache) lookup(scene, question decisionDigest) (float64, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	p, ok := c.scene(scene)[question]
	return p, ok
}

func (c *decisionCache) remember(scene, question decisionDigest, p float64) float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	scores := c.scene(scene)
	// Overlapping cold requests may both answer before either sees a hit.
	// Use the first saved probability for both deliveries.
	if saved, ok := scores[question]; ok {
		return saved
	}
	if c.count == decisionCacheAnswers || (scores == nil && len(c.scenes) == decisionCacheScenes) {
		c.count -= len(c.scenes[0].scores)
		if c.scenes[0].key == scene {
			scores = nil
		}
		c.scenes = slices.Delete(c.scenes, 0, 1)
	}
	if scores == nil {
		scores = make(map[decisionDigest]float64)
		c.scenes = append(c.scenes, sceneScores{scene, scores})
	}
	scores[question] = p
	c.count++
	return p
}

// prepFor constructs a new decider for every delivery. Keep saved answers
// across those connections and scope them by the effective endpoint and model.
var prepDecisionCache decisionCache
