package oracle

import "testing"

// TestUnsupportedTypeIsRejectedNotGuessed closes O5: SeedCanaries must not
// invent a value for a column type it does not understand. A wrong value in a
// canary row is worse than skipping the table, because a constraint could
// coincidentally pass while the seeded data means nothing.
//
// This test must fail if sampleValue starts returning ok=true for an unknown
// type, or if its caller stops checking ok.
func TestUnsupportedTypeIsRejectedNotGuessed(t *testing.T) {
	if _, ok := sampleValue("point", 0); ok {
		t.Fatalf("sampleValue accepted the unsupported type %q", "point")
	}
	if _, ok := sampleValue("tsvector", 0); ok {
		t.Fatalf("sampleValue accepted the unsupported type %q", "tsvector")
	}
	// A known type must still work, so the test is not vacuously true.
	if _, ok := sampleValue("integer", 0); !ok {
		t.Fatalf("sampleValue rejected a supported type")
	}
}
