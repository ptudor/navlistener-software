package state

import "sync/atomic"

// Generation changes whenever the whole materialized view is invalidated.
// Feed builders compare it before/after a render so a concurrent reset cannot
// produce a body assembled partly from pre-withdrawal and partly from new state.
func (s *Store) Generation() uint64 { return s.generation.Load() }

// Reset conservatively drops the complete audience materialization. The state
// is merged across sources, so exact subtraction of one withdrawn observer is
// not sound: its ephemeris may have won freshest-value selection and influenced
// confidence, detector inputs, RF baselines, and almanac state. Rebuilding from
// post-change receipts is the fail-closed operation.
func (s *Store) Reset() {
	for _, shard := range s.shards {
		shard.mu.Lock()
	}
	s.sbasMu.Lock()
	s.gloAlmMu.Lock()
	s.almMu.Lock()
	s.rfMu.Lock()
	s.capMu.Lock()
	s.monitoringMu.Lock()

	for _, shard := range s.shards {
		shard.m = make(map[Key]*svState)
	}
	s.sbas = make(map[int]*sbasState)
	s.gloAlmanac = make(map[int]gloAlmSlot)
	s.gloNA = 0
	s.almanacs = make(map[almanacKey]keplerAlmanac)
	s.almanacRecords = make(map[almanacRecordKey]almanacSourceRecord)
	s.rf = make(map[string]*rfStation)
	s.boards = make(map[string]*boardStation)
	// Integrity checks hold the same withdrawn receipts in their histories; the
	// configuration (integrityCfg) is not authorization evidence and stays.
	s.integrity = make(map[string]*integrityStation)
	s.agcRestore = make(map[string]map[int]agcBaseline)
	s.caps = make(map[string]*capStation)
	// Declarations are authorization evidence too. They are relearned from
	// post-change trusted receipts; retaining them could expose a withdrawn
	// station through capability reports/events even with no new nav frames.
	s.declared = nil
	clear(s.monitoringRoster)
	s.generation.Add(1)

	s.monitoringMu.Unlock()
	s.capMu.Unlock()
	s.rfMu.Unlock()
	s.almMu.Unlock()
	s.gloAlmMu.Unlock()
	s.sbasMu.Unlock()
	for i := len(s.shards) - 1; i >= 0; i-- {
		s.shards[i].mu.Unlock()
	}
}

// Compile-time assertion that Store keeps an atomic generation counter. The
// named type is used only to make lifecycle.go's ownership explicit.
type storeGeneration = atomic.Uint64
