package runtime

import "testing"

func TestLedgerIdempotencyQuotaAndSequence(t *testing.T) {
	l := Ledger{HardLimit: 10}
	if err := l.Apply(1, 1, 6); err != nil {
		t.Fatal(err)
	}
	if err := l.Apply(1, 1, 6); err != nil {
		t.Fatal("replay: ", err)
	}
	if err := l.Apply(2, 1, 1); err != ErrUsageSequence {
		t.Fatalf("sequence: %v", err)
	}
	if err := l.Apply(2, 2, 5); err != ErrQuota {
		t.Fatalf("quota: %v", err)
	}
	if l.Used != 6 {
		t.Fatalf("used=%d", l.Used)
	}
}
