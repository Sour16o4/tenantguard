package capture

import (
	"fmt"
	"sync"
)

// Kind identifies what a captured Event records.
type Kind string

const (
	KindParse     Kind = "parse"
	KindParamDesc Kind = "param_description"
	KindRowDesc   Kind = "row_description"
	KindBind      Kind = "bind"
	KindClose     Kind = "close"
	KindQuery     Kind = "query"
)

// Event is one observation from the frontend stream.
//
// A Bind carries Resolved: true when the statement it names was parsed on this
// same connection and is still open. When Resolved is false the SQL is unknown
// and must never be treated as if it were known — an unresolvable Bind is a
// capture gap, not an absence of risk.
type Event struct {
	Conn     int     `json:"conn"`
	Kind     Kind    `json:"kind"`
	Stmt     string  `json:"stmt,omitempty"`
	Portal   string  `json:"portal,omitempty"`
	SQL      string  `json:"sql,omitempty"`
	Resolved bool    `json:"resolved"`
	Params   []Param `json:"params,omitempty"`
	Note     string  `json:"note,omitempty"`
}

// Session tracks one client connection's prepared-statement state.
//
// Statement names are scoped to a connection, never shared between them: two
// connections may legitimately use the same name for different SQL, and
// resolving across them would attribute a query to the wrong statement.
type Session struct {
	// Both directions are fed from separate goroutines in the proxy.
	mu    sync.Mutex
	id    int
	stmts map[string]string
	fr    framer

	// Backend direction. Parameter type OIDs arrive here and nowhere else, so
	// without them every binary-format parameter is unreadable (design §9.5).
	bfr framer
	// pendingDescribe is the FIFO of statement names the client has asked the
	// server to describe. Backend replies carry no statement name, so the only
	// way to attribute a ParameterDescription is the order it was requested in.
	pendingDescribe []string
	lastDescribed   string
	paramOIDs       map[string][]uint32
	rowOIDs         map[string][]uint32
}

// NewSession returns a Session for a connection identified by id.
func NewSession(id int) *Session {
	return &Session{
		id:        id,
		stmts:     make(map[string]string),
		paramOIDs: make(map[string][]uint32),
		rowOIDs:   make(map[string][]uint32),
	}
}

// ID returns the connection identifier.
func (s *Session) ID() int { return s.id }

// Open reports how many prepared statements this connection currently holds.
func (s *Session) Open() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.stmts)
}

// FeedFrontend consumes bytes from the client-to-server direction and returns
// the events they complete. Bytes that do not yet form a whole message are
// retained until the rest arrives.
func (s *Session) FeedFrontend(b []byte) []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fr.push(b)
	var out []Event
	for {
		typ, body, ok := s.fr.next()
		if !ok {
			return out
		}
		if ev, emit := s.handle(typ, body); emit {
			out = append(out, ev)
		}
	}
}

// FeedBackend consumes bytes from the server-to-client direction, which is where
// parameter type information travels. Without it, binary-format parameters have
// no recoverable value and every query using one is UNATTRIBUTABLE.
func (s *Session) FeedBackend(b []byte) []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bfr.push(b)
	var out []Event
	for {
		typ, body, ok := s.bfr.next()
		if !ok {
			return out
		}
		if ev, emit := s.handleBackend(typ, body); emit {
			out = append(out, ev)
		}
	}
}

func (s *Session) handleBackend(typ byte, body []byte) (Event, bool) {
	switch typ {
	case msgParamDesc:
		oids, err := parseParameterDescription(body)
		if err != nil {
			return Event{}, false
		}
		// The reply carries no statement name; the only attribution available is
		// the order in which the client asked. Pop the oldest outstanding
		// Describe('S').
		if len(s.pendingDescribe) == 0 {
			return Event{Conn: s.id, Kind: KindParamDesc,
				Note: "ParameterDescription with no outstanding Describe; cannot attribute"}, true
		}
		name := s.pendingDescribe[0]
		s.pendingDescribe = s.pendingDescribe[1:]
		s.lastDescribed = name
		s.paramOIDs[name] = oids
		return Event{Conn: s.id, Kind: KindParamDesc, Stmt: name, Resolved: true}, true

	case msgRowDesc:
		oids, err := parseRowDescription(body)
		if err != nil {
			return Event{}, false
		}
		// A RowDescription follows the ParameterDescription for the same
		// statement, which has already been popped, so it is recorded against
		// the most recently attributed statement when there is one.
		name := s.lastDescribed
		if name != "" {
			s.rowOIDs[name] = oids
		}
		return Event{Conn: s.id, Kind: KindRowDesc, Stmt: name, Resolved: name != ""}, true
	}
	return Event{}, false
}

// resolveParams fills in values for binary parameters, using the type OIDs
// captured from the backend stream. A parameter whose OID is missing or whose
// type has no decoder is left unknown with a reason — never guessed.
func (s *Session) resolveParams(stmt string, params []Param) {
	oids := s.paramOIDs[stmt]

	// Arity guard. Backend replies carry no statement name, so OIDs are
	// attributed by the order Describe was requested in. If that correlation is
	// ever wrong, this statement holds another statement's OIDs — and where the
	// counts overlap, decoding would still succeed and produce confidently wrong
	// values. A count mismatch is the observable signature of that, so when it
	// appears no parameter may be decoded.
	correlationOK := len(oids) == len(params)

	for i := range params {
		p := &params[i]
		if p.Known || p.IsNull || p.Format != FormatBinary {
			continue
		}
		if len(oids) == 0 {
			p.Reason = "binary parameter; no type OID captured for this statement"
			continue
		}
		if !correlationOK {
			p.Reason = fmt.Sprintf(
				"binary parameter; %d type OIDs captured for %d parameters — correlation unreliable, refusing to decode",
				len(oids), len(params))
			continue
		}
		p.OID = oids[i]
		text, ok := decodeBinary(p.OID, p.Value)
		if !ok {
			p.Reason = fmt.Sprintf("binary parameter; no decoder for type OID %d (or wrong payload width)", p.OID)
			continue
		}
		p.Known, p.Text, p.Reason = true, text, ""
	}
}

// ParamOIDs returns the captured parameter type OIDs for a statement.
func (s *Session) ParamOIDs(stmt string) []uint32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.paramOIDs[stmt]
}

func (s *Session) handle(typ byte, body []byte) (Event, bool) {
	switch typ {
	case msgParse:
		name, sql, err := parseParse(body)
		if err != nil {
			return Event{}, false
		}
		s.stmts[name] = sql
		return Event{Conn: s.id, Kind: KindParse, Stmt: name, SQL: sql}, true

	case msgBind:
		portal, stmt, params, err := parseBind(body)
		if err != nil {
			return Event{}, false
		}
		s.resolveParams(stmt, params)
		sql, ok := s.stmts[stmt]
		ev := Event{
			Conn: s.id, Kind: KindBind, Stmt: stmt, Portal: portal,
			SQL: sql, Resolved: ok, Params: params,
		}
		if !ok {
			ev.Note = fmt.Sprintf("unresolvable: no open statement named %q on this connection", stmt)
		}
		return ev, true

	case msgDescribe:
		// Only Describe('S') produces a ParameterDescription. Describe('P')
		// yields a RowDescription alone and must not enter the queue, or every
		// later correlation is shifted by one.
		if len(body) < 1 {
			return Event{}, false
		}
		if body[0] != 'S' {
			return Event{}, false
		}
		name, _, err := cstring(body, 1)
		if err != nil {
			return Event{}, false
		}
		s.pendingDescribe = append(s.pendingDescribe, name)
		return Event{}, false

	case msgClose:
		// TGD-BL-12. Statement state must be removed here. Without it a later
		// Bind naming this statement resolves to the closed statement's SQL and
		// reports Resolved: true — a confident verdict on a query the
		// application never ran.
		//
		// Kind 'S' closes a prepared statement; 'P' closes a portal, which does
		// not affect prepared-statement state.
		kind, name, err := parseClose(body)
		if err != nil {
			return Event{}, false
		}
		if kind != 'S' {
			return Event{}, false
		}
		delete(s.stmts, name)
		return Event{
			Conn: s.id, Kind: KindClose, Stmt: name,
			Note: "statement closed; a later Bind on this name is unresolvable",
		}, true

	case msgQuery:
		sql, _, err := cstring(body, 0)
		if err != nil {
			return Event{}, false
		}
		// Simple protocol: parameters are already interpolated by the client,
		// so the SQL is captured whole and there is nothing to resolve.
		return Event{Conn: s.id, Kind: KindQuery, SQL: sql, Resolved: true}, true
	}
	return Event{}, false
}
