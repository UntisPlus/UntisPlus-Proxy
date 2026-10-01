package store

// Tests for the proxy-local homework completion flags: the keys, the isolation
// between students and schools, and the behaviour a client retry depends on.

import (
	"testing"
	"time"
)

func TestHomeworkDoneRoundTrip(t *testing.T) {
	st := openTestStore(t)
	at, err := st.SetHomeworkDone("testschool", "dee", 1001)
	if err != nil {
		t.Fatalf("SetHomeworkDone: %v", err)
	}
	if at.IsZero() {
		t.Error("SetHomeworkDone returned a zero timestamp")
	}
	done, err := st.HomeworkDone("testschool", "dee")
	if err != nil {
		t.Fatalf("HomeworkDone: %v", err)
	}
	if len(done) != 1 {
		t.Fatalf("got %d flags, want 1", len(done))
	}
	got, ok := done[1001]
	if !ok {
		t.Fatalf("flag 1001 is missing from %v", done)
	}
	if !got.Equal(at) {
		t.Errorf("stored timestamp %v != returned %v", got, at)
	}
	if n, _ := st.HomeworkDoneCount("testschool", "dee"); n != 1 {
		t.Errorf("count = %d, want 1", n)
	}
}

// TestHomeworkDoneIsolatedPerUser is the property that makes the feature safe to
// ship: one student's answers must never be readable as another's.
func TestHomeworkDoneIsolatedPerUser(t *testing.T) {
	st := openTestStore(t)
	if _, err := st.SetHomeworkDone("testschool", "dee", 1001); err != nil {
		t.Fatal(err)
	}
	sam, err := st.HomeworkDone("testschool", "sam")
	if err != nil {
		t.Fatalf("HomeworkDone: %v", err)
	}
	if len(sam) != 0 {
		t.Errorf("dee's flag leaked into sam's result: %v", sam)
	}
	if n, _ := st.HomeworkDoneCount("testschool", "sam"); n != 0 {
		t.Errorf("sam count = %d, want 0", n)
	}
}

// TestHomeworkDoneIsolatedPerSchool: homework ids are only unique within a
// school, so the school has to be part of the key.
func TestHomeworkDoneIsolatedPerSchool(t *testing.T) {
	st := openTestStore(t)
	if _, err := st.SetHomeworkDone("schoola", "dee", 1001); err != nil {
		t.Fatal(err)
	}
	other, err := st.HomeworkDone("schoolb", "dee")
	if err != nil {
		t.Fatalf("HomeworkDone: %v", err)
	}
	if len(other) != 0 {
		t.Errorf("a flag set in schoola leaked into schoolb: %v", other)
	}
}

// TestHomeworkDoneNormalizesUsername: the app is inconsistent about case, so the
// same human must not end up with two sets of flags.
func TestHomeworkDoneNormalizesUsername(t *testing.T) {
	st := openTestStore(t)
	if _, err := st.SetHomeworkDone("testschool", "Dee", 1001); err != nil {
		t.Fatal(err)
	}
	lower, err := st.HomeworkDone("testschool", "dee")
	if err != nil {
		t.Fatalf("HomeworkDone: %v", err)
	}
	if len(lower) != 1 {
		t.Errorf("case-insensitive lookup returned %d flags, want 1", len(lower))
	}
	upper, err := st.HomeworkDone("testschool", "DEE")
	if err != nil {
		t.Fatalf("HomeworkDone: %v", err)
	}
	if len(upper) != 1 {
		t.Errorf("upper-case lookup returned %d flags, want 1", len(upper))
	}
	// The two spellings must be the same row, not two.
	if n, _ := st.HomeworkDoneCount("testschool", "dee"); n != 1 {
		t.Errorf("count = %d, want 1 (the two spellings must not duplicate)", n)
	}
}

// TestHomeworkDoneSetIsIdempotent: a client that retries after a dropped
// response must not end up with duplicate rows.
func TestHomeworkDoneSetIsIdempotent(t *testing.T) {
	st := openTestStore(t)
	for i := 0; i < 5; i++ {
		if _, err := st.SetHomeworkDone("testschool", "dee", 1001); err != nil {
			t.Fatalf("set %d: %v", i, err)
		}
	}
	if n, _ := st.HomeworkDoneCount("testschool", "dee"); n != 1 {
		t.Errorf("five sets produced %d rows, want 1", n)
	}
}

// TestHomeworkDoneClear: clearing removes the row, and clearing again is not an
// error, so a repeated "set false" converges.
func TestHomeworkDoneClear(t *testing.T) {
	st := openTestStore(t)
	if _, err := st.SetHomeworkDone("testschool", "dee", 1001); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := st.ClearHomeworkDone("testschool", "dee", 1001); err != nil {
			t.Fatalf("clear %d: %v", i, err)
		}
	}
	done, err := st.HomeworkDone("testschool", "dee")
	if err != nil {
		t.Fatalf("HomeworkDone: %v", err)
	}
	if len(done) != 0 {
		t.Errorf("flags survived clearing: %v", done)
	}
}

// TestHomeworkDoneClearKeepsOtherRows: a targeted clear must not wipe a
// student's whole board.
func TestHomeworkDoneClearKeepsOtherRows(t *testing.T) {
	st := openTestStore(t)
	for _, id := range []int64{1001, 1002, 1003} {
		if _, err := st.SetHomeworkDone("testschool", "dee", id); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.ClearHomeworkDone("testschool", "dee", 1002); err != nil {
		t.Fatalf("clear: %v", err)
	}
	done, err := st.HomeworkDone("testschool", "dee")
	if err != nil {
		t.Fatalf("HomeworkDone: %v", err)
	}
	if len(done) != 2 {
		t.Fatalf("got %d flags after a targeted clear, want 2: %v", len(done), done)
	}
	if _, ok := done[1002]; ok {
		t.Error("the cleared flag is still present")
	}
}

// TestHomeworkDoneLargeID: homework ids are the storage key. A float64 round trip
// would silently corrupt ids above 2^53, so the value has to travel as int64 the
// whole way.
func TestHomeworkDoneLargeID(t *testing.T) {
	st := openTestStore(t)
	const bigID = int64(9007199254740993)
	if _, err := st.SetHomeworkDone("testschool", "dee", bigID); err != nil {
		t.Fatalf("SetHomeworkDone: %v", err)
	}
	done, err := st.HomeworkDone("testschool", "dee")
	if err != nil {
		t.Fatalf("HomeworkDone: %v", err)
	}
	if _, ok := done[bigID]; !ok {
		t.Errorf("large id did not round-trip: got keys %v, want %d", done, bigID)
	}
	if err := st.ClearHomeworkDone("testschool", "dee", bigID); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if n, _ := st.HomeworkDoneCount("testschool", "dee"); n != 0 {
		t.Errorf("count after clearing the large id = %d, want 0", n)
	}
}

// TestHomeworkDoneCountOnEmptyStore: the dashboard and the flags endpoint must
// not error on a store that has never seen a flag.
func TestHomeworkDoneCountOnEmptyStore(t *testing.T) {
	st := openTestStore(t)
	n, err := st.HomeworkDoneCount("testschool", "dee")
	if err != nil {
		t.Fatalf("HomeworkDoneCount: %v", err)
	}
	if n != 0 {
		t.Errorf("count = %d, want 0", n)
	}
	done, err := st.HomeworkDone("testschool", "dee")
	if err != nil {
		t.Fatalf("HomeworkDone: %v", err)
	}
	if len(done) != 0 {
		t.Errorf("flags = %v, want none", done)
	}
}

// TestHomeworkDoneTimestampIsRecent: a stored flag carries the moment it was
// marked, so the app can show it without inventing a time.
func TestHomeworkDoneTimestampIsRecent(t *testing.T) {
	st := openTestStore(t)
	before := time.Now().Add(-2 * time.Second)
	at, err := st.SetHomeworkDone("testschool", "dee", 1001)
	if err != nil {
		t.Fatalf("SetHomeworkDone: %v", err)
	}
	if at.Before(before) || at.After(time.Now().Add(2*time.Second)) {
		t.Errorf("timestamp %v is not close to now", at)
	}
}
