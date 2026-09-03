package capture

import (
	"encoding/binary"
	"testing"
)

// --- backend / describe message builders ---

func describeStmtMsg(name string) []byte {
	return msg(msgDescribe, append([]byte{'S'}, cstr(name)...))
}

func describePortalMsg(name string) []byte {
	return msg(msgDescribe, append([]byte{'P'}, cstr(name)...))
}

func u32(v uint32) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, v)
	return b
}

// paramDescMsg builds a backend ParameterDescription ('t').
func paramDescMsg(oids ...uint32) []byte {
	p := u16(len(oids))
	for _, o := range oids {
		p = append(p, u32(o)...)
	}
	return msg(msgParamDesc, p)
}

// rowDescMsg builds a minimal backend RowDescription ('T').
func rowDescMsg(cols map[string]uint32, order []string) []byte {
	p := u16(len(order))
	for _, name := range order {
		p = append(p, cstr(name)...)
		p = append(p, u32(0)...)          // table OID
		p = append(p, u16(0)...)          // column attribute number
		p = append(p, u32(cols[name])...) // type OID
		p = append(p, u16(0)...)          // type size
		p = append(p, u32(0)...)          // type modifier
		p = append(p, u16(0)...)          // format code
	}
	return msg(msgRowDesc, p)
}

func binaryBind(stmt string, vals [][]byte) []byte {
	formats := make([]ParamFormat, len(vals))
	for i := range formats {
		formats[i] = FormatBinary
	}
	return bindMsg("", stmt, formats, vals)
}

// --- the core requirement (TGD-FR-19) ---

// TestBinaryParamDecodedFromParameterDescription is the reason backend capture
// exists. Without the type OID a binary parameter's value is unknowable, which
// makes its query UNATTRIBUTABLE.
//
// This test must fail if FeedBackend stops recording ParameterDescription OIDs.
func TestBinaryParamDecodedFromParameterDescription(t *testing.T) {
	s := NewSession(1)
	s.FeedFrontend(parseMsg("q", "SELECT * FROM t WHERE id=$1 AND name=$2"))
	s.FeedFrontend(describeStmtMsg("q"))
	s.FeedBackend(paramDescMsg(OIDInt4, OIDText))

	if got := s.ParamOIDs("q"); len(got) != 2 || got[0] != OIDInt4 || got[1] != OIDText {
		t.Fatalf("ParamOIDs = %v, want [23 25]", got)
	}

	evs := s.FeedFrontend(binaryBind("q", [][]byte{
		{0x00, 0x00, 0x00, 0x2a}, // int4 42
		[]byte("acme"),
	}))
	p := binds(evs)[0].Params
	if len(p) != 2 {
		t.Fatalf("want 2 params, got %d", len(p))
	}
	if !p[0].Known || p[0].Text != "42" {
		t.Errorf("binary int4 = %+v, want Known with Text \"42\"", p[0])
	}
	if p[0].OID != OIDInt4 {
		t.Errorf("param 0 OID = %d, want %d", p[0].OID, OIDInt4)
	}
	if !p[1].Known || p[1].Text != "acme" {
		t.Errorf("binary text = %+v, want Known with Text \"acme\"", p[1])
	}
}

// TestBinaryParamWithoutOIDStaysUnknown: no Describe means no OIDs, and the
// parameter must stay unknown rather than being guessed.
func TestBinaryParamWithoutOIDStaysUnknown(t *testing.T) {
	s := NewSession(1)
	s.FeedFrontend(parseMsg("q", "SELECT $1"))
	evs := s.FeedFrontend(binaryBind("q", [][]byte{{0x00, 0x00, 0x00, 0x2a}}))

	p := binds(evs)[0].Params
	if p[0].Known {
		t.Errorf("binary parameter reported Known with no type OID captured: %+v", p[0])
	}
	if p[0].Reason == "" {
		t.Errorf("unknown parameter must carry a reason")
	}
	if string(p[0].Value) != "\x00\x00\x00\x2a" {
		t.Errorf("raw bytes must still be preserved for inspection")
	}
}

// TestUnsupportedOIDStaysUnknown: having an OID is not enough if the type has no
// decoder. numeric (1700) is deliberately unsupported.
func TestUnsupportedOIDStaysUnknown(t *testing.T) {
	const oidNumeric = 1700
	s := NewSession(1)
	s.FeedFrontend(parseMsg("q", "SELECT $1"))
	s.FeedFrontend(describeStmtMsg("q"))
	s.FeedBackend(paramDescMsg(oidNumeric))

	evs := s.FeedFrontend(binaryBind("q", [][]byte{{0x00, 0x01, 0x02, 0x03}}))
	p := binds(evs)[0].Params
	if p[0].Known {
		t.Errorf("unsupported OID %d reported Known; a wrong value is worse than a known gap", oidNumeric)
	}
	if p[0].OID != oidNumeric {
		t.Errorf("OID should still be recorded even when undecodable, got %d", p[0].OID)
	}
}

// TestWrongWidthPayloadStaysUnknown: an int4 OID with 3 bytes is not an int4.
func TestWrongWidthPayloadStaysUnknown(t *testing.T) {
	s := NewSession(1)
	s.FeedFrontend(parseMsg("q", "SELECT $1"))
	s.FeedFrontend(describeStmtMsg("q"))
	s.FeedBackend(paramDescMsg(OIDInt4))

	evs := s.FeedFrontend(binaryBind("q", [][]byte{{0x00, 0x00, 0x2a}}))
	if binds(evs)[0].Params[0].Known {
		t.Errorf("3-byte payload decoded as int4; width must be validated")
	}
}

// TestDescribeCorrelationIsOrdered: backend replies carry no statement name, so
// attribution depends entirely on the order the client asked for them.
//
// This test must fail if the pending-describe queue is not FIFO.
func TestDescribeCorrelationIsOrdered(t *testing.T) {
	s := NewSession(1)
	s.FeedFrontend(parseMsg("first", "SELECT $1"))
	s.FeedFrontend(parseMsg("second", "SELECT $1"))
	s.FeedFrontend(describeStmtMsg("first"))
	s.FeedFrontend(describeStmtMsg("second"))

	s.FeedBackend(paramDescMsg(OIDInt4)) // answers "first"
	s.FeedBackend(paramDescMsg(OIDText)) // answers "second"

	if got := s.ParamOIDs("first"); len(got) != 1 || got[0] != OIDInt4 {
		t.Errorf("first statement OIDs = %v, want [%d]", got, OIDInt4)
	}
	if got := s.ParamOIDs("second"); len(got) != 1 || got[0] != OIDText {
		t.Errorf("second statement OIDs = %v, want [%d]", got, OIDText)
	}
}

// TestDescribePortalDoesNotConsumeQueue: Describe('P') yields a RowDescription
// but no ParameterDescription, so it must not enter the correlation queue.
func TestDescribePortalDoesNotConsumeQueue(t *testing.T) {
	s := NewSession(1)
	s.FeedFrontend(parseMsg("q", "SELECT $1"))
	s.FeedFrontend(describePortalMsg("")) // portal, not statement
	s.FeedFrontend(describeStmtMsg("q"))
	s.FeedBackend(paramDescMsg(OIDInt8))

	if got := s.ParamOIDs("q"); len(got) != 1 || got[0] != OIDInt8 {
		t.Errorf("portal describe consumed the statement's slot: OIDs = %v", got)
	}
}

// TestRowDescriptionCaptured records result column types, which a later differ
// needs to compare row sets.
func TestRowDescriptionCaptured(t *testing.T) {
	s := NewSession(1)
	s.FeedFrontend(parseMsg("q", "SELECT id, name FROM t"))
	s.FeedFrontend(describeStmtMsg("q"))
	s.FeedBackend(paramDescMsg())
	evs := s.FeedBackend(rowDescMsg(
		map[string]uint32{"id": OIDInt4, "name": OIDText},
		[]string{"id", "name"}))

	var found bool
	for _, e := range evs {
		if e.Kind == KindRowDesc {
			found = true
		}
	}
	if !found {
		t.Errorf("RowDescription was not captured as an event")
	}
}

// TestBackendFramingAcrossSplitReads: the backend direction needs its own framer.
func TestBackendFramingAcrossSplitReads(t *testing.T) {
	s := NewSession(1)
	s.FeedFrontend(parseMsg("q", "SELECT $1"))
	s.FeedFrontend(describeStmtMsg("q"))

	stream := paramDescMsg(OIDInt4)
	for i := 0; i < len(stream); i++ {
		s.FeedBackend(stream[i : i+1])
	}
	if got := s.ParamOIDs("q"); len(got) != 1 || got[0] != OIDInt4 {
		t.Errorf("backend framer failed across split reads: OIDs = %v", got)
	}
}

// --- decoder unit coverage ---

func TestDecodeBinaryTypes(t *testing.T) {
	cases := []struct {
		name string
		oid  uint32
		in   []byte
		want string
	}{
		{"bool true", OIDBool, []byte{1}, "true"},
		{"int2", OIDInt2, []byte{0x00, 0x2a}, "42"},
		{"int4 negative", OIDInt4, []byte{0xff, 0xff, 0xff, 0xd6}, "-42"},
		{"int8", OIDInt8, []byte{0, 0, 0, 0, 0, 0, 0x01, 0x00}, "256"},
		{"text", OIDText, []byte("acme"), "acme"},
		{"uuid", OIDUUID, []byte{
			0x00, 0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77,
			0x88, 0x99, 0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff},
			"00112233-4455-6677-8899-aabbccddeeff"},
		{"date", OIDDate, []byte{0x00, 0x00, 0x00, 0x00}, "2000-01-01"},
	}
	for _, c := range cases {
		got, ok := decodeBinary(c.oid, c.in)
		if !ok {
			t.Errorf("%s: decode reported failure", c.name)
			continue
		}
		if got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestDecodeBinaryRejectsBadWidths(t *testing.T) {
	for _, c := range []struct {
		oid uint32
		in  []byte
	}{
		{OIDInt4, []byte{1, 2, 3}},
		{OIDInt8, []byte{1}},
		{OIDBool, []byte{1, 2}},
		{OIDUUID, []byte{1, 2, 3}},
	} {
		if _, ok := decodeBinary(c.oid, c.in); ok {
			t.Errorf("oid %d accepted a %d-byte payload", c.oid, len(c.in))
		}
	}
}

// --- Describe correlation safety (follow-up to the risk flagged in slice 2) ---

// TestOIDCountMismatchRefusesToDecode is the guard on misattributed OIDs.
//
// Backend replies carry no statement name, so correlation rests on FIFO order of
// outstanding Describe('S') messages. If that order is ever wrong, a statement
// receives another statement's OIDs. Where the counts overlap, decoding would
// still "succeed" — producing confidently wrong values, which the differ would
// then rebind and judge. That is the R-2 failure class.
//
// The defence is arity: if the captured OID count does not match the bound
// parameter count, correlation is unreliable and NO parameter may be decoded.
//
// This test must fail if the arity check is removed.
func TestOIDCountMismatchRefusesToDecode(t *testing.T) {
	s := NewSession(1)
	s.FeedFrontend(parseMsg("q", "SELECT * FROM t WHERE a=$1 AND b=$2"))
	s.FeedFrontend(describeStmtMsg("q"))
	// Only ONE OID arrives for a statement that will bind TWO parameters —
	// the shape a misattributed ParameterDescription produces.
	s.FeedBackend(paramDescMsg(OIDInt4))

	evs := s.FeedFrontend(binaryBind("q", [][]byte{
		{0x00, 0x00, 0x00, 0x2a},
		{0x00, 0x00, 0x00, 0x07},
	}))
	p := binds(evs)[0].Params
	for i := range p {
		if p[i].Known {
			t.Errorf("param %d decoded despite an OID/parameter count mismatch "+
				"(1 OID, 2 params): value would rest on an unverified correlation", i)
		}
		if p[i].Reason == "" {
			t.Errorf("param %d has no reason recorded for being unknown", i)
		}
	}
}

// TestDroppedDescribeIsDetected: if a queued Describe is dropped, the next
// ParameterDescription is attributed to the wrong statement. The arity check is
// what stops that becoming a wrong value.
func TestDroppedDescribeIsDetected(t *testing.T) {
	s := NewSession(1)
	s.FeedFrontend(parseMsg("one", "SELECT $1"))         // 1 param
	s.FeedFrontend(parseMsg("two", "SELECT $1, $2, $3")) // 3 params
	s.FeedFrontend(describeStmtMsg("one"))
	s.FeedFrontend(describeStmtMsg("two"))

	// The server answers "one" with 1 OID, then "two" with 3.
	s.FeedBackend(paramDescMsg(OIDInt4))
	s.FeedBackend(paramDescMsg(OIDInt4, OIDInt4, OIDInt4))

	// "two" binds three params: correct correlation, all decodable.
	evs := s.FeedFrontend(binaryBind("two", [][]byte{
		{0, 0, 0, 1}, {0, 0, 0, 2}, {0, 0, 0, 3},
	}))
	for i, p := range binds(evs)[0].Params {
		if !p.Known {
			t.Errorf("correctly correlated param %d should decode, got %q", i, p.Reason)
		}
	}

	// "one" binds one param and must not have picked up "two"'s OIDs.
	evs = s.FeedFrontend(binaryBind("one", [][]byte{{0, 0, 0, 9}}))
	if got := binds(evs)[0].Params[0]; !got.Known || got.Text != "9" {
		t.Errorf("statement \"one\" resolved to %+v; correlation crossed statements", got)
	}
}

// TestUnattributedParameterDescriptionIsReported: a reply with nothing
// outstanding must be surfaced, not dropped.
func TestUnattributedParameterDescriptionIsReported(t *testing.T) {
	s := NewSession(1)
	evs := s.FeedBackend(paramDescMsg(OIDInt4))
	if len(evs) != 1 || evs[0].Kind != KindParamDesc {
		t.Fatalf("want one param_description event, got %+v", evs)
	}
	if evs[0].Resolved {
		t.Errorf("a ParameterDescription with no outstanding Describe must not report Resolved")
	}
	if evs[0].Note == "" {
		t.Errorf("an unattributable ParameterDescription must carry a note")
	}
}
