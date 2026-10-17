package idgen

import (
	"fmt"
	"sync"
	"time"
)

const (
	// epoch is a custom starting point (2024-01-01 00:00:00 UTC in ms).
	epoch = int64(1704067200000)

	nodeBits     = 10
	sequenceBits = 12

	maxNodeID   = -1 ^ (-1 << nodeBits)   // 1023
	maxSequence = -1 ^ (-1 << sequenceBits) // 4095

	timeShift = nodeBits + sequenceBits // 22
	nodeShift = sequenceBits            // 12
)

// Generator produces unique int64 Snowflake IDs.
// Safe for concurrent use.
type Generator struct {
	mu       sync.Mutex
	nodeID   int64
	sequence int64
	lastMS   int64
}

// NewGenerator creates a Generator for the given nodeID (0–1023).
func NewGenerator(nodeID int64) (*Generator, error) {
	if nodeID < 0 || nodeID > maxNodeID {
		return nil, fmt.Errorf("idgen: nodeID must be 0–%d, got %d", maxNodeID, nodeID)
	}
	return &Generator{nodeID: nodeID}, nil
}

// MinIDAt returns the smallest ID that NextID can return at or after t, on any
// node. Because IDs are time-ordered, "id >= MinIDAt(t)" selects rows created
// at or after t.
func MinIDAt(t time.Time) int64 {
	return (t.UnixMilli() - epoch) << timeShift
}

// NextID returns a unique, time-ordered int64 ID.
func (g *Generator) NextID() int64 {
	g.mu.Lock()
	defer g.mu.Unlock()

	now := time.Now().UnixMilli() - epoch

	if now == g.lastMS {
		g.sequence = (g.sequence + 1) & maxSequence
		if g.sequence == 0 {
			// sequence exhausted — spin until next millisecond
			for now <= g.lastMS {
				now = time.Now().UnixMilli() - epoch
			}
		}
	} else {
		g.sequence = 0
	}

	g.lastMS = now
	return (now << timeShift) | (g.nodeID << nodeShift) | g.sequence
}
