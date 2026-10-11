package main

import (
	"testing"
	"time"
)

// TestNewHTTPServerSetsTimeouts pins the C4 fix: the exposed listener must not
// accept a half-open connection forever, but its write deadline stays absent so
// the SSE timetable stream is not cut off.
func TestNewHTTPServerSetsTimeouts(t *testing.T) {
	srv := newHTTPServer(":0", nil)
	if srv.ReadHeaderTimeout != 15*time.Second {
		t.Errorf("ReadHeaderTimeout = %v, want 15s", srv.ReadHeaderTimeout)
	}
	if srv.ReadTimeout != 30*time.Second {
		t.Errorf("ReadTimeout = %v, want 30s", srv.ReadTimeout)
	}
	if srv.IdleTimeout != 120*time.Second {
		t.Errorf("IdleTimeout = %v, want 120s", srv.IdleTimeout)
	}
	if srv.WriteTimeout != 0 {
		t.Errorf("WriteTimeout = %v, want 0 so /api/timetable/stream is not cut off", srv.WriteTimeout)
	}
}
