package runtime

import (
	"errors"
	"math"
	"sync"
)

var ErrUsageSequence = errors.New("usage sequence is not increasing")
var ErrQuota = errors.New("token quota exceeded")

type Ledger struct {
	mu                               sync.Mutex
	HardLimit, Used, LastID, LastSeq uint64
}

func (l *Ledger) Apply(id, seq, amount uint64) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if id == l.LastID && id != 0 {
		return nil
	}
	if seq <= l.LastSeq {
		return ErrUsageSequence
	}
	if l.HardLimit != 0 && (l.Used > l.HardLimit || amount > l.HardLimit-l.Used) {
		return ErrQuota
	}
	if math.MaxUint64-l.Used < amount {
		l.Used = math.MaxUint64
	} else {
		l.Used += amount
	}
	l.LastID, l.LastSeq = id, seq
	return nil
}

func saturatingAdd(a, b uint64) uint64 {
	if math.MaxUint64-a < b {
		return math.MaxUint64
	}
	return a + b
}
