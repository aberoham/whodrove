package synthsess

import (
	"testing"
	"time"
)

func mk(principal string, t time.Time, count int64) MinuteFeature {
	return MinuteFeature{Principal: principal, MinuteBucket: t, CallCount: count, DistinctServices: 1, DistinctMethods: 1}
}

func TestSynthesise_Empty(t *testing.T) {
	if got := Synthesise(nil, 10*time.Minute); got != nil {
		t.Fatalf("want nil for empty input, got %+v", got)
	}
}

func TestSynthesise_SingleSessionContiguous(t *testing.T) {
	base := time.Date(2026, 4, 25, 10, 0, 0, 0, time.UTC)
	in := []MinuteFeature{
		mk("alice@example.com", base, 5),
		mk("alice@example.com", base.Add(time.Minute), 7),
		mk("alice@example.com", base.Add(2*time.Minute), 3),
	}
	got := Synthesise(in, 10*time.Minute)
	if len(got) != 1 {
		t.Fatalf("want 1 session, got %d", len(got))
	}
	if got[0].CallCount != 15 {
		t.Errorf("call_count: want 15, got %d", got[0].CallCount)
	}
	if !got[0].StartedAt.Equal(base) {
		t.Errorf("started_at mismatch")
	}
	if !got[0].EndedAt.Equal(base.Add(3 * time.Minute)) {
		t.Errorf("ended_at: want %v, got %v", base.Add(3*time.Minute), got[0].EndedAt)
	}
}

func TestSynthesise_GapSplitsSession(t *testing.T) {
	base := time.Date(2026, 4, 25, 10, 0, 0, 0, time.UTC)
	in := []MinuteFeature{
		mk("alice@example.com", base, 5),
		mk("alice@example.com", base.Add(time.Minute), 5),
		// 30-minute gap → new session
		mk("alice@example.com", base.Add(31*time.Minute), 5),
		mk("alice@example.com", base.Add(32*time.Minute), 5),
	}
	got := Synthesise(in, 10*time.Minute)
	if len(got) != 2 {
		t.Fatalf("want 2 sessions, got %d", len(got))
	}
	if got[0].CallCount != 10 || got[1].CallCount != 10 {
		t.Errorf("per-session call counts: %+v", got)
	}
	if got[0].SessionID == got[1].SessionID {
		t.Errorf("session ids should differ across split")
	}
}

func TestSynthesise_PrincipalChangeSplits(t *testing.T) {
	base := time.Date(2026, 4, 25, 10, 0, 0, 0, time.UTC)
	in := []MinuteFeature{
		mk("alice@example.com", base, 5),
		mk("bob@example.com", base.Add(time.Minute), 5),
	}
	got := Synthesise(in, 10*time.Minute)
	if len(got) != 2 {
		t.Fatalf("want 2 sessions for principal change, got %d", len(got))
	}
}

func TestSynthesise_GapAtThreshold(t *testing.T) {
	base := time.Date(2026, 4, 25, 10, 0, 0, 0, time.UTC)
	// idleThreshold = 10 min; gap from bucket-1 (10:01) to bucket-2 (10:11)
	// is exactly 10 min — should NOT split. (We allow gap = threshold + 1m
	// because adjacent buckets are 1m apart.)
	in := []MinuteFeature{
		mk("alice@example.com", base.Add(time.Minute), 5),
		mk("alice@example.com", base.Add(11*time.Minute), 5),
	}
	got := Synthesise(in, 10*time.Minute)
	if len(got) != 1 {
		t.Fatalf("want 1 session at gap=threshold, got %d", len(got))
	}
}

func TestSynthesise_DeterministicSessionID(t *testing.T) {
	base := time.Date(2026, 4, 25, 10, 0, 0, 0, time.UTC)
	in := []MinuteFeature{mk("alice@example.com", base, 5)}
	a := Synthesise(in, 10*time.Minute)
	b := Synthesise(in, 10*time.Minute)
	if a[0].SessionID != b[0].SessionID {
		t.Errorf("synth id should be deterministic; got %q vs %q", a[0].SessionID, b[0].SessionID)
	}
	if len(a[0].SessionID) != len("gcp-")+24 {
		t.Errorf("synth id length: got %d", len(a[0].SessionID))
	}
}

func TestSynthesise_OutOfOrderInputSorts(t *testing.T) {
	base := time.Date(2026, 4, 25, 10, 0, 0, 0, time.UTC)
	in := []MinuteFeature{
		mk("alice@example.com", base.Add(2*time.Minute), 3),
		mk("alice@example.com", base, 5),
		mk("alice@example.com", base.Add(time.Minute), 7),
	}
	got := Synthesise(in, 10*time.Minute)
	if len(got) != 1 {
		t.Fatalf("want 1 session, got %d", len(got))
	}
	if !got[0].StartedAt.Equal(base) {
		t.Errorf("StartedAt should be earliest bucket after sort; got %v", got[0].StartedAt)
	}
}

func TestSynthesise_AggregatesAndMedian(t *testing.T) {
	base := time.Date(2026, 4, 25, 10, 0, 0, 0, time.UTC)
	in := []MinuteFeature{
		{Principal: "alice@example.com", MinuteBucket: base, CallCount: 3,
			DistinctServices: 2, DistinctMethods: 4,
			ImpersonationCalls: 1, DeniedCalls: 0, SampleUA: "gcloud/x"},
		{Principal: "alice@example.com", MinuteBucket: base.Add(time.Minute), CallCount: 7,
			DistinctServices: 5, DistinctMethods: 9,
			ImpersonationCalls: 0, DeniedCalls: 2, SampleUA: ""},
	}
	got := Synthesise(in, 10*time.Minute)
	if got[0].CallCount != 10 || got[0].DistinctServicesM != 5 || got[0].DistinctMethodsM != 9 {
		t.Errorf("aggregates: %+v", got[0])
	}
	if got[0].ImpersonationCalls != 1 || got[0].DeniedCalls != 2 {
		t.Errorf("counts: %+v", got[0])
	}
	if got[0].SampleUA != "gcloud/x" {
		t.Errorf("first non-empty SampleUA should be picked: %q", got[0].SampleUA)
	}
	if got[0].MedianCallGapMs != 60000 {
		t.Errorf("median gap for 1m apart buckets: want 60000, got %v", got[0].MedianCallGapMs)
	}
}
