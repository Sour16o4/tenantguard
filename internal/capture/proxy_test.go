package capture

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// collector is a Sink that accumulates events for assertions.
type collector struct {
	mu sync.Mutex
	ev []Event
}

func (c *collector) Emit(e Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ev = append(c.ev, e)
}

func (c *collector) snapshot() []Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Event(nil), c.ev...)
}

// fakeUpstream stands in for PostgreSQL. It records what the proxy forwarded so
// the test can assert the byte stream was not modified, and never replies —
// nothing in the capture path depends on server responses.
type fakeUpstream struct {
	ln net.Listener
	mu sync.Mutex
	rx bytes.Buffer
}

func newFakeUpstream(t *testing.T) *fakeUpstream {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("fake upstream listen: %v", err)
	}
	u := &fakeUpstream{ln: ln}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				buf := make([]byte, 4096)
				for {
					n, err := c.Read(buf)
					if n > 0 {
						u.mu.Lock()
						u.rx.Write(buf[:n])
						u.mu.Unlock()
					}
					if err != nil {
						return
					}
				}
			}()
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return u
}

func (u *fakeUpstream) received() []byte {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]byte(nil), u.rx.Bytes()...)
}

// startProxy runs a Proxy in front of the fake upstream and returns its address.
func startProxy(t *testing.T, up *fakeUpstream, sink Sink) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("proxy listen: %v", err)
	}
	p := &Proxy{Upstream: up.ln.Addr().String(), Sink: sink}
	go p.Serve(ln)
	t.Cleanup(func() { ln.Close() })
	return ln.Addr().String()
}

func startupMsg() []byte {
	var params []byte
	params = append(params, cstr("user")...)
	params = append(params, cstr("tgd")...)
	params = append(params, 0)
	out := make([]byte, 8)
	binary.BigEndian.PutUint32(out[0:4], uint32(8+len(params)))
	binary.BigEndian.PutUint32(out[4:8], protocolV3)
	return append(out, params...)
}

func sslRequestMsg() []byte {
	out := make([]byte, 8)
	binary.BigEndian.PutUint32(out[0:4], 8)
	binary.BigEndian.PutUint32(out[4:8], sslRequestCode)
	return out
}

// waitFor polls until cond holds or the deadline passes.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// TestProxyCapturesKnownWorkload drives a fixed workload through a real socket
// and asserts the exact captured set. This is the gate on the capture layer:
// nothing may be built on top of it until this holds.
func TestProxyCapturesKnownWorkload(t *testing.T) {
	up := newFakeUpstream(t)
	col := &collector{}
	addr := startProxy(t, up, col)

	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer c.Close()

	// SSLRequest first, which the proxy must decline with 'N' so the rest of the
	// stream stays readable.
	if _, err := c.Write(sslRequestMsg()); err != nil {
		t.Fatalf("write SSLRequest: %v", err)
	}
	var reply [1]byte
	if _, err := io.ReadFull(c, reply[:]); err != nil {
		t.Fatalf("read SSL reply: %v", err)
	}
	if reply[0] != 'N' {
		t.Fatalf("SSL reply = %q, want 'N'", reply[0])
	}

	if _, err := c.Write(startupMsg()); err != nil {
		t.Fatalf("write startup: %v", err)
	}

	// The workload: a named statement bound twice with different parameters, a
	// close, a bind after the close, and a simple-protocol query.
	workload := [][]byte{
		parseMsg("q1", "SELECT * FROM invoices WHERE tenant_id = $1"),
		bindMsg("", "q1", nil, [][]byte{[]byte("acme")}),
		bindMsg("", "q1", nil, [][]byte{nil}),
		closeStmtMsg("q1"),
		bindMsg("", "q1", nil, [][]byte{[]byte("globex")}),
		queryMsg("SELECT count(*) FROM invoices"),
	}
	var sent []byte
	for _, m := range workload {
		sent = append(sent, m...)
		if _, err := c.Write(m); err != nil {
			t.Fatalf("write workload: %v", err)
		}
	}

	waitFor(t, "all workload events", func() bool {
		n := 0
		for _, e := range col.snapshot() {
			if e.Kind != "startup" {
				n++
			}
		}
		return n >= 6
	})

	var got []Event
	for _, e := range col.snapshot() {
		if e.Kind != "startup" {
			got = append(got, e)
		}
	}
	if len(got) != 6 {
		t.Fatalf("captured %d events, want 6: %+v", len(got), got)
	}

	want := []struct {
		kind     Kind
		stmt     string
		sql      string
		resolved bool
	}{
		{KindParse, "q1", "SELECT * FROM invoices WHERE tenant_id = $1", false},
		{KindBind, "q1", "SELECT * FROM invoices WHERE tenant_id = $1", true},
		{KindBind, "q1", "SELECT * FROM invoices WHERE tenant_id = $1", true},
		{KindClose, "q1", "", false},
		{KindBind, "q1", "", false}, // after Close: unresolvable, not stale
		{KindQuery, "", "SELECT count(*) FROM invoices", true},
	}
	for i, w := range want {
		g := got[i]
		if g.Kind != w.kind {
			t.Errorf("event %d kind = %q, want %q", i, g.Kind, w.kind)
		}
		if g.Stmt != w.stmt {
			t.Errorf("event %d stmt = %q, want %q", i, g.Stmt, w.stmt)
		}
		if g.SQL != w.sql {
			t.Errorf("event %d sql = %q, want %q", i, g.SQL, w.sql)
		}
		if g.Kind == KindBind && g.Resolved != w.resolved {
			t.Errorf("event %d resolved = %v, want %v", i, g.Resolved, w.resolved)
		}
	}

	// Parameters, including the NULL.
	if p := got[1].Params; len(p) != 1 || string(p[0].Value) != "acme" {
		t.Errorf("first bind params = %+v, want one text param \"acme\"", p)
	}
	if p := got[2].Params; len(p) != 1 || !p[0].IsNull {
		t.Errorf("second bind params = %+v, want one NULL param", p)
	}

	// The proxy must forward the client stream byte for byte.
	waitFor(t, "upstream to receive the workload", func() bool {
		return bytes.Contains(up.received(), sent)
	})
	rx := up.received()
	if !bytes.Contains(rx, sent) {
		t.Errorf("upstream did not receive the client bytes verbatim")
	}
	if bytes.Contains(rx, sslRequestMsg()) {
		t.Errorf("SSLRequest was forwarded upstream; it must be answered locally")
	}
}

// TestRecorderOutput asserts the JSONL contract: binary parameters are marked
// undecoded and carry base64 rather than being presented as text.
func TestRecorderOutput(t *testing.T) {
	var buf bytes.Buffer
	r := NewRecorder(&buf)

	r.Emit(Event{Conn: 1, Kind: KindBind, Stmt: "s", SQL: "SELECT $1, $2, $3", Resolved: true,
		Params: []Param{
			{Format: FormatText, Value: []byte("acme"), Known: true, Text: "acme"},
			{IsNull: true, Format: FormatText, Known: true, Text: "NULL"},
			{Format: FormatBinary, Value: []byte{0x00, 0x2a},
				Reason: "binary parameter; type OID not captured"},
		}})

	line := strings.TrimSpace(buf.String())
	var out jsonEvent
	if err := json.Unmarshal([]byte(line), &out); err != nil {
		t.Fatalf("recorder emitted invalid JSON: %v\n%s", err, line)
	}
	if len(out.Params) != 3 {
		t.Fatalf("want 3 params in JSON, got %d", len(out.Params))
	}
	if out.Params[0].Value != "acme" || !out.Params[0].Known {
		t.Errorf("text param = %+v, want decoded value \"acme\"", out.Params[0])
	}
	if !out.Params[1].Null {
		t.Errorf("NULL param not marked null: %+v", out.Params[1])
	}
	if out.Params[2].Known {
		t.Errorf("binary param with no OID marked known; it cannot be read as a value")
	}
	if out.Params[2].Reason == "" {
		t.Errorf("an unknown parameter must carry a reason in the JSON output")
	}
	if out.Params[2].ValueB64 == "" || out.Params[2].Value != "" {
		t.Errorf("binary param = %+v, want base64 bytes and no text value", out.Params[2])
	}
}
