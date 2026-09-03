package capture

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"sync"
)

// jsonParam is the wire form of a captured parameter.
//
// Text parameters carry their value directly. Binary parameters carry base64
// bytes and value_decoded=false, because they cannot be rendered without the
// parameter's type OID, which this layer never sees. Nothing downstream may
// treat an undecoded parameter as if its value were known.
type jsonParam struct {
	Null     bool   `json:"null,omitempty"`
	Format   string `json:"format"`
	OID      uint32 `json:"oid,omitempty"`
	Known    bool   `json:"value_known"`
	Value    string `json:"value,omitempty"`
	ValueB64 string `json:"value_base64,omitempty"`
	Reason   string `json:"unknown_reason,omitempty"`
}

type jsonEvent struct {
	Conn     int         `json:"conn"`
	Kind     Kind        `json:"kind"`
	Stmt     string      `json:"stmt,omitempty"`
	Portal   string      `json:"portal,omitempty"`
	SQL      string      `json:"sql,omitempty"`
	Resolved bool        `json:"resolved"`
	Params   []jsonParam `json:"params,omitempty"`
	Note     string      `json:"note,omitempty"`
}

// Recorder writes captured events as newline-delimited JSON.
//
// It records what was observed. It does not judge, rank, or summarise — a
// consumer of this stream is reading evidence, not a verdict.
type Recorder struct {
	mu  sync.Mutex
	enc *json.Encoder
}

// NewRecorder writes JSONL to w.
func NewRecorder(w io.Writer) *Recorder {
	return &Recorder{enc: json.NewEncoder(w)}
}

// Emit implements Sink.
func (r *Recorder) Emit(e Event) {
	je := jsonEvent{
		Conn: e.Conn, Kind: e.Kind, Stmt: e.Stmt, Portal: e.Portal,
		SQL: e.SQL, Resolved: e.Resolved, Note: e.Note,
	}
	for _, p := range e.Params {
		jp := jsonParam{
			Null: p.IsNull, Format: p.Format.String(), OID: p.OID,
			Known: p.Known, Reason: p.Reason,
		}
		switch {
		case p.IsNull:
		case p.Known:
			jp.Value = p.Text
		default:
			// Bytes are preserved so the gap is inspectable, but they are not
			// presented as a value.
			jp.ValueB64 = base64.StdEncoding.EncodeToString(p.Value)
		}
		je.Params = append(je.Params, jp)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	_ = r.enc.Encode(je)
}
