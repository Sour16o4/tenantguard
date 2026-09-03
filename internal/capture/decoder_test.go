package capture

import (
	"bytes"
	"testing"
)

// TestDecodeEvents_RoundTripsRecorderOutput proves DecodeEvents is Emit's
// exact inverse: every Event field a consumer downstream (the differ) relies
// on — Resolved, SQL, and each Param's Known/Text/IsNull — must come back
// unchanged after a write-then-read cycle through the real JSONL wire format,
// not a separately-invented one.
func TestDecodeEvents_RoundTripsRecorderOutput(t *testing.T) {
	in := []Event{
		{
			Conn: 1, Kind: KindBind, Stmt: "s1", Portal: "p1",
			SQL: "SELECT * FROM invoices WHERE tenant_id = $1", Resolved: true,
			Params: []Param{
				{Known: true, Text: "acme", Format: FormatText},
			},
		},
		{
			Conn: 1, Kind: KindQuery, SQL: "SELECT now()", Resolved: true,
		},
		{
			// A NULL parameter must decode back as IsNull, not as a known
			// empty-string value — the differ treats those very differently.
			Conn: 2, Kind: KindBind, Stmt: "s2", Resolved: true,
			SQL: "INSERT INTO invoices (tenant_id, note) VALUES ($1, $2)",
			Params: []Param{
				{Known: true, Text: "globex", Format: FormatText},
				{IsNull: true, Format: FormatText},
			},
		},
		{
			// An undecoded binary parameter must come back Known: false with
			// its raw bytes intact — never silently promoted to a text value.
			Conn: 3, Kind: KindBind, Stmt: "s3", Resolved: true,
			SQL: "SELECT * FROM invoices WHERE id = $1",
			Params: []Param{
				{Known: false, Format: FormatBinary, OID: 23,
					Value: []byte{0x00, 0x01, 0x02}, Reason: "binary parameter; type OID not captured"},
			},
		},
		{
			// A Bind the capture layer could not resolve to its SQL at all —
			// Diff must treat this as Unattributable, never guess at SQL.
			Conn: 4, Kind: KindBind, Stmt: "s4", Resolved: false,
			Note: "no matching Parse for this statement name on this connection",
		},
	}

	var buf bytes.Buffer
	rec := NewRecorder(&buf)
	for _, e := range in {
		rec.Emit(e)
	}

	out, err := DecodeEvents(&buf)
	if err != nil {
		t.Fatalf("DecodeEvents: %v", err)
	}
	if len(out) != len(in) {
		t.Fatalf("got %d events, want %d", len(out), len(in))
	}

	for i := range in {
		want, got := in[i], out[i]
		if got.Conn != want.Conn || got.Kind != want.Kind || got.SQL != want.SQL ||
			got.Resolved != want.Resolved {
			t.Fatalf("event %d: got %+v, want %+v", i, got, want)
		}
		if len(got.Params) != len(want.Params) {
			t.Fatalf("event %d: got %d params, want %d", i, len(got.Params), len(want.Params))
		}
		for j := range want.Params {
			wp, gp := want.Params[j], got.Params[j]
			if gp.IsNull != wp.IsNull || gp.Known != wp.Known || gp.Text != wp.Text || gp.Format != wp.Format {
				t.Fatalf("event %d param %d: got %+v, want %+v", i, j, gp, wp)
			}
			if !wp.Known && !wp.IsNull && !bytes.Equal(gp.Value, wp.Value) {
				t.Fatalf("event %d param %d: undecoded bytes got %v, want %v", i, j, gp.Value, wp.Value)
			}
			if wp.IsNull && len(gp.Value) != 0 {
				t.Fatalf("event %d param %d: a null parameter must carry no value bytes, got %v", i, j, gp.Value)
			}
		}
	}
}

// TestDecodeEvents_EmptyInputIsNoEvents guards against DecodeEvents treating
// an empty capture file as an error — a target that made no queries during
// the capture window is a legitimate, if unusual, outcome.
func TestDecodeEvents_EmptyInputIsNoEvents(t *testing.T) {
	out, err := DecodeEvents(bytes.NewReader(nil))
	if err != nil {
		t.Fatalf("DecodeEvents(empty): %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("got %d events from empty input, want 0", len(out))
	}
}
