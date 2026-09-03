package capture

import (
	"encoding/binary"
	"testing"
)

// --- wire-message builders, so tests speak the protocol rather than fixtures ---

func msg(typ byte, payload []byte) []byte {
	out := []byte{typ}
	length := make([]byte, 4)
	binary.BigEndian.PutUint32(length, uint32(4+len(payload)))
	out = append(out, length...)
	return append(out, payload...)
}

func cstr(s string) []byte { return append([]byte(s), 0) }

func u16(v int) []byte {
	b := make([]byte, 2)
	binary.BigEndian.PutUint16(b, uint16(v))
	return b
}

func parseMsg(name, sql string) []byte {
	p := append(cstr(name), cstr(sql)...)
	p = append(p, u16(0)...) // no parameter type OIDs
	return msg(msgParse, p)
}

// bindMsg builds a Bind. formats applies to the parameters; pass nil for "all
// text". A nil entry in params encodes SQL NULL.
func bindMsg(portal, stmt string, formats []ParamFormat, params [][]byte) []byte {
	b := append(cstr(portal), cstr(stmt)...)
	b = append(b, u16(len(formats))...)
	for _, f := range formats {
		b = append(b, u16(int(f))...)
	}
	b = append(b, u16(len(params))...)
	for _, p := range params {
		if p == nil {
			neg := make([]byte, 4)
			binary.BigEndian.PutUint32(neg, ^uint32(0)) // -1
			b = append(b, neg...)
			continue
		}
		l := make([]byte, 4)
		binary.BigEndian.PutUint32(l, uint32(len(p)))
		b = append(b, l...)
		b = append(b, p...)
	}
	b = append(b, u16(0)...) // no result format codes
	return msg(msgBind, b)
}

func closeStmtMsg(name string) []byte {
	return msg(msgClose, append([]byte{'S'}, cstr(name)...))
}

func queryMsg(sql string) []byte { return msg(msgQuery, cstr(sql)) }

func binds(evs []Event) []Event {
	var out []Event
	for _, e := range evs {
		if e.Kind == KindBind {
			out = append(out, e)
		}
	}
	return out
}

// --- TGD-BL-12: the defect that shipped in the spike proxy ---

// TestBindAfterCloseIsUnresolvable is the regression test for TGD-BL-12.
//
// The spike proxy ignored Close, so a Bind naming a closed statement resolved to
// the closed statement's SQL. The capture layer then reported a query the
// application never ran, with Resolved: true — a confident wrong answer, which
// is the R-2 failure class the tool exists to prevent.
//
// This test must fail if Close handling is removed from Session.handle.
func TestBindAfterCloseIsUnresolvable(t *testing.T) {
	const name = "stmt_1"
	const sql = "SELECT 1 AS this_is_the_stale_statement"

	s := NewSession(1)
	s.FeedFrontend(parseMsg(name, sql))
	s.FeedFrontend(closeStmtMsg(name))
	evs := s.FeedFrontend(bindMsg("", name, nil, nil))

	b := binds(evs)
	if len(b) != 1 {
		t.Fatalf("expected exactly 1 Bind event, got %d", len(b))
	}
	if b[0].Resolved {
		t.Errorf("Bind on a CLOSED statement reported Resolved=true; "+
			"capture would judge a query the application never ran (SQL=%q)", b[0].SQL)
	}
	if b[0].SQL != "" {
		t.Errorf("Bind on a closed statement carried stale SQL %q; want empty", b[0].SQL)
	}
	if s.Open() != 0 {
		t.Errorf("statement state survived Close: %d still open", s.Open())
	}
}

// TestCloseOnlyAffectsNamedStatement checks Close does not clear unrelated state.
func TestCloseOnlyAffectsNamedStatement(t *testing.T) {
	s := NewSession(1)
	s.FeedFrontend(parseMsg("keep", "SELECT 'keep'"))
	s.FeedFrontend(parseMsg("drop", "SELECT 'drop'"))
	s.FeedFrontend(closeStmtMsg("drop"))

	evs := s.FeedFrontend(bindMsg("", "keep", nil, nil))
	b := binds(evs)
	if len(b) != 1 || !b[0].Resolved {
		t.Fatalf("Bind on a statement that was NOT closed should resolve; got %+v", b)
	}
	if b[0].SQL != "SELECT 'keep'" {
		t.Errorf("resolved to %q, want %q", b[0].SQL, "SELECT 'keep'")
	}
}

// TestClosePortalDoesNotDropStatement: Close with kind 'P' targets a portal, not
// a prepared statement, and must leave statement state alone.
func TestClosePortalDoesNotDropStatement(t *testing.T) {
	s := NewSession(1)
	s.FeedFrontend(parseMsg("st", "SELECT 1"))
	s.FeedFrontend(msg(msgClose, append([]byte{'P'}, cstr("st")...)))

	evs := s.FeedFrontend(bindMsg("", "st", nil, nil))
	b := binds(evs)
	if len(b) != 1 || !b[0].Resolved {
		t.Errorf("closing a PORTAL dropped prepared-statement state; Bind became unresolvable")
	}
}

// --- per-connection scoping ---

// TestStatementNamesAreScopedPerConnection is the guard on the collision that
// lib/pq's sequential naming makes possible: two connections legitimately using
// the same statement name for different SQL.
//
// This test must fail if statement state is shared between Sessions.
func TestStatementNamesAreScopedPerConnection(t *testing.T) {
	a := NewSession(1)
	b := NewSession(2)

	a.FeedFrontend(parseMsg("1", "SELECT 'from_conn_a'"))
	b.FeedFrontend(parseMsg("1", "SELECT 'from_conn_b'"))

	ea := binds(a.FeedFrontend(bindMsg("", "1", nil, nil)))
	eb := binds(b.FeedFrontend(bindMsg("", "1", nil, nil)))

	if len(ea) != 1 || len(eb) != 1 {
		t.Fatalf("expected one Bind per session, got %d and %d", len(ea), len(eb))
	}
	if ea[0].SQL != "SELECT 'from_conn_a'" {
		t.Errorf("connection A resolved %q, want its own SQL", ea[0].SQL)
	}
	if eb[0].SQL != "SELECT 'from_conn_b'" {
		t.Errorf("connection B resolved %q, want its own SQL", eb[0].SQL)
	}
}

// --- framing ---

// TestFramingAcrossSplitReads feeds the stream one byte at a time.
func TestFramingAcrossSplitReads(t *testing.T) {
	stream := append(parseMsg("s", "SELECT 42"), bindMsg("", "s", nil, nil)...)

	s := NewSession(1)
	var all []Event
	for i := 0; i < len(stream); i++ {
		all = append(all, s.FeedFrontend(stream[i:i+1])...)
	}
	if len(all) != 2 {
		t.Fatalf("byte-at-a-time feed produced %d events, want 2", len(all))
	}
	if !binds(all)[0].Resolved || binds(all)[0].SQL != "SELECT 42" {
		t.Errorf("Bind did not resolve across split reads: %+v", binds(all)[0])
	}
}

// TestFramingMultipleMessagesInOneRead is the other half: pipelined messages.
func TestFramingMultipleMessagesInOneRead(t *testing.T) {
	stream := append(parseMsg("s", "SELECT 42"), bindMsg("", "s", nil, nil)...)
	stream = append(stream, queryMsg("SELECT 'simple'")...)

	evs := NewSession(1).FeedFrontend(stream)
	if len(evs) != 3 {
		t.Fatalf("single-read feed produced %d events, want 3", len(evs))
	}
	if evs[2].Kind != KindQuery || evs[2].SQL != "SELECT 'simple'" {
		t.Errorf("third event was %+v, want the simple query", evs[2])
	}
}

// TestPartialMessageEmitsNothing: a truncated message must not emit.
func TestPartialMessageEmitsNothing(t *testing.T) {
	full := parseMsg("s", "SELECT 42")
	s := NewSession(1)
	if evs := s.FeedFrontend(full[:len(full)-1]); len(evs) != 0 {
		t.Fatalf("incomplete message produced %d events, want 0", len(evs))
	}
	if evs := s.FeedFrontend(full[len(full)-1:]); len(evs) != 1 {
		t.Fatalf("completing the message produced %d events, want 1", len(evs))
	}
}

// --- parameter recovery ---

func TestBindParameterRecovery(t *testing.T) {
	s := NewSession(1)
	s.FeedFrontend(parseMsg("s", "SELECT * FROM t WHERE a=$1 AND b=$2 AND c=$3"))

	evs := s.FeedFrontend(bindMsg("", "s", nil, [][]byte{
		[]byte("acme"),
		nil, // SQL NULL
		[]byte("42"),
	}))
	b := binds(evs)
	if len(b) != 1 {
		t.Fatalf("want 1 Bind, got %d", len(b))
	}
	p := b[0].Params
	if len(p) != 3 {
		t.Fatalf("want 3 params, got %d", len(p))
	}
	if string(p[0].Value) != "acme" || !p[0].Known {
		t.Errorf("param 0 = %+v, want decoded text \"acme\"", p[0])
	}
	if !p[1].IsNull {
		t.Errorf("param 1 = %+v, want NULL", p[1])
	}
	if !p[1].Known {
		t.Errorf("a NULL is a value the differ can rebind, so it must count as Known")
	}
	if string(p[2].Value) != "42" {
		t.Errorf("param 2 = %q, want \"42\"", p[2].Value)
	}
}

// TestBinaryParametersAreCapturedbutNotDecoded records a real limitation:
// binary-format parameters cannot be rendered as values without their type OID,
// which this layer does not see. They are captured as raw bytes and must report
// Decoded() == false rather than pretending to be text.
func TestBinaryParametersAreCapturedButNotDecoded(t *testing.T) {
	s := NewSession(1)
	s.FeedFrontend(parseMsg("s", "SELECT * FROM t WHERE a=$1"))

	raw := []byte{0x00, 0x00, 0x00, 0x2a} // int4 42, binary
	evs := s.FeedFrontend(bindMsg("", "s", []ParamFormat{FormatBinary}, [][]byte{raw}))
	p := binds(evs)[0].Params
	if len(p) != 1 {
		t.Fatalf("want 1 param, got %d", len(p))
	}
	if p[0].Format != FormatBinary {
		t.Errorf("format = %v, want binary", p[0].Format)
	}
	if p[0].Known {
		t.Errorf("binary parameter reported Decoded()==true; it cannot be read as text")
	}
	if string(p[0].Value) != string(raw) {
		t.Errorf("binary bytes were not captured verbatim")
	}
}

// TestPerParameterFormatCodes covers the case of one format code per parameter.
func TestPerParameterFormatCodes(t *testing.T) {
	s := NewSession(1)
	s.FeedFrontend(parseMsg("s", "SELECT $1, $2"))
	evs := s.FeedFrontend(bindMsg("", "s",
		[]ParamFormat{FormatText, FormatBinary},
		[][]byte{[]byte("txt"), {0x01}}))
	p := binds(evs)[0].Params
	if p[0].Format != FormatText || p[1].Format != FormatBinary {
		t.Errorf("per-parameter formats mis-assigned: %v, %v", p[0].Format, p[1].Format)
	}
}

// --- simple protocol ---

func TestSimpleQueryCapturedWhole(t *testing.T) {
	evs := NewSession(1).FeedFrontend(queryMsg("SELECT count(*) FROM invoices WHERE tenant_id = 'acme'"))
	if len(evs) != 1 || evs[0].Kind != KindQuery {
		t.Fatalf("want 1 query event, got %+v", evs)
	}
	if !evs[0].Resolved {
		t.Errorf("a simple query needs no resolution and must report Resolved=true")
	}
	if evs[0].SQL != "SELECT count(*) FROM invoices WHERE tenant_id = 'acme'" {
		t.Errorf("simple query SQL not captured verbatim: %q", evs[0].SQL)
	}
}
