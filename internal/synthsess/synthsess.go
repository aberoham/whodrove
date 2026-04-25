// Package synthsess synthesises "sessions" from a stream of
// per-(principal, minute) feature rows. GCP audit logs do not
// emit a session_id, so phase-1 detection has to invent one.
//
// Algorithm: walk rows ordered by (principal, minute_bucket).
// Start a new synthetic session when:
//   - the principal changes, or
//   - the gap to the prior bucket exceeds idleThreshold.
//
// The synthetic session_id is deterministic — sha256 of
// "principal|first_bucket_iso" truncated to 24 hex chars,
// prefixed "gcp-". Re-running over the same input range produces
// the same IDs so upserts are stable.
package synthsess

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"time"
)

// MinuteFeature is one row of the BigQuery aggregation: one
// (principal, minute_bucket) bucket. Top* fields are pre-rendered
// JSON strings of the APPROX_TOP_COUNT result.
type MinuteFeature struct {
	Principal          string
	MinuteBucket       time.Time
	CallCount          int64
	DistinctServices   int64
	DistinctMethods    int64
	SampleUA           string
	SampleIP           string
	ImpersonationCalls int64
	DeniedCalls        int64
	TopServicesJSON    string
	TopMethodsJSON     string
}

// Session is one synthesised session.
type Session struct {
	SessionID  string
	Principal  string
	StartedAt  time.Time
	EndedAt    time.Time // last bucket + 1 minute
	SampleUA   string
	SampleIP   string
	Buckets    []MinuteFeature
	// Aggregates derived from Buckets, computed on Build.
	CallCount          int64
	DistinctServicesM  int64 // max distinct_services across buckets (lower bound on real distinct)
	DistinctMethodsM   int64
	ImpersonationCalls int64
	DeniedCalls        int64
	MedianCallGapMs    float64
}

// Synthesise glues per-minute features into sessions.
// idleThreshold is the maximum gap between adjacent buckets that
// still counts as "the same session". 600s is the default in
// notes-gcp/06.
//
// Input does not need to be sorted — Synthesise sorts a copy.
func Synthesise(features []MinuteFeature, idleThreshold time.Duration) []Session {
	if len(features) == 0 {
		return nil
	}
	sorted := make([]MinuteFeature, len(features))
	copy(sorted, features)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].Principal != sorted[j].Principal {
			return sorted[i].Principal < sorted[j].Principal
		}
		return sorted[i].MinuteBucket.Before(sorted[j].MinuteBucket)
	})

	var (
		out     []Session
		current []MinuteFeature
	)
	flush := func() {
		if len(current) == 0 {
			return
		}
		out = append(out, build(current))
		current = nil
	}
	for i, f := range sorted {
		if i == 0 {
			current = append(current, f)
			continue
		}
		prev := current[len(current)-1]
		gap := f.MinuteBucket.Sub(prev.MinuteBucket)
		if f.Principal != prev.Principal || gap > idleThreshold+time.Minute {
			// +1 minute because adjacent buckets are exactly 60s apart
			// and we want gaps measured from end-of-previous-bucket.
			flush()
		}
		current = append(current, f)
	}
	flush()
	return out
}

// build aggregates a contiguous run of MinuteFeatures into a Session.
// Pre: buckets is non-empty and sorted ascending by MinuteBucket,
// all entries share the same Principal.
func build(buckets []MinuteFeature) Session {
	first := buckets[0]
	last := buckets[len(buckets)-1]

	s := Session{
		SessionID: synthID(first.Principal, first.MinuteBucket),
		Principal: first.Principal,
		StartedAt: first.MinuteBucket,
		EndedAt:   last.MinuteBucket.Add(time.Minute),
		Buckets:   buckets,
	}

	for _, b := range buckets {
		s.CallCount += b.CallCount
		if b.DistinctServices > s.DistinctServicesM {
			s.DistinctServicesM = b.DistinctServices
		}
		if b.DistinctMethods > s.DistinctMethodsM {
			s.DistinctMethodsM = b.DistinctMethods
		}
		s.ImpersonationCalls += b.ImpersonationCalls
		s.DeniedCalls += b.DeniedCalls
		if s.SampleUA == "" && b.SampleUA != "" {
			s.SampleUA = b.SampleUA
		}
		if s.SampleIP == "" && b.SampleIP != "" {
			s.SampleIP = b.SampleIP
		}
	}

	// Median inter-bucket gap, in milliseconds. Useful as a phase-1
	// cadence feature: tight bursts vs paced human work.
	if len(buckets) > 1 {
		gaps := make([]float64, 0, len(buckets)-1)
		for i := 1; i < len(buckets); i++ {
			d := buckets[i].MinuteBucket.Sub(buckets[i-1].MinuteBucket)
			gaps = append(gaps, float64(d.Milliseconds()))
		}
		sort.Float64s(gaps)
		mid := len(gaps) / 2
		if len(gaps)%2 == 0 {
			s.MedianCallGapMs = (gaps[mid-1] + gaps[mid]) / 2
		} else {
			s.MedianCallGapMs = gaps[mid]
		}
	}
	return s
}

func synthID(principal string, firstBucket time.Time) string {
	h := sha256.Sum256([]byte(principal + "|" + firstBucket.UTC().Format(time.RFC3339)))
	return "gcp-" + hex.EncodeToString(h[:])[:24]
}
