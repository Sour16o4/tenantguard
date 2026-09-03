// Package capture observes the PostgreSQL frontend message stream and records
// the queries an application issues, with their parameters.
//
// It observes only. It never modifies the byte stream, and it does not judge
// whether a query is correctly scoped — that belongs to a later layer.
//
// Protocol parsing is deliberately separated from network plumbing so the
// message handling can be tested without a database or a socket.
package capture

import (
	"encoding/binary"
	"errors"
)

// Frontend message type bytes this package understands. Everything else is
// forwarded untouched and ignored for capture purposes.
const (
	msgParse    = 'P'
	msgBind     = 'B'
	msgClose    = 'C'
	msgQuery    = 'Q'
	msgDescribe = 'D'
)

// Backend (server-to-client) message types. Parameter type information travels
// only in this direction.
const (
	msgParamDesc = 't'
	msgRowDesc   = 'T'
)

// Startup-phase request codes, which arrive without a type byte.
const (
	sslRequestCode = 80877103
	gssRequestCode = 80877104
	protocolV3     = 196608
)

var errShortMessage = errors.New("capture: message body shorter than its declared fields")

// framer accumulates bytes from a connection and yields complete typed
// messages. It must survive a message split across several reads and several
// messages arriving in one read; both happen routinely in practice.
type framer struct {
	buf []byte
}

func (f *framer) push(b []byte) {
	f.buf = append(f.buf, b...)
}

// next returns the next complete message, or ok=false if more bytes are needed.
func (f *framer) next() (typ byte, body []byte, ok bool) {
	if len(f.buf) < 5 {
		return 0, nil, false
	}
	length := binary.BigEndian.Uint32(f.buf[1:5])
	// The length field counts itself but not the type byte.
	if length < 4 {
		// Malformed. Drop the byte so we cannot spin forever on it.
		f.buf = f.buf[1:]
		return 0, nil, false
	}
	total := int(length) + 1
	if len(f.buf) < total {
		return 0, nil, false
	}
	typ = f.buf[0]
	body = make([]byte, total-5)
	copy(body, f.buf[5:total])
	f.buf = f.buf[total:]
	return typ, body, true
}

// cstring reads a NUL-terminated string beginning at i, returning it and the
// index just past the terminator.
func cstring(b []byte, i int) (string, int, error) {
	if i < 0 || i > len(b) {
		return "", 0, errShortMessage
	}
	for j := i; j < len(b); j++ {
		if b[j] == 0 {
			return string(b[i:j]), j + 1, nil
		}
	}
	return "", 0, errShortMessage
}

// ParamFormat describes how a bound parameter was encoded on the wire.
type ParamFormat int16

const (
	FormatText   ParamFormat = 0
	FormatBinary ParamFormat = 1
)

func (f ParamFormat) String() string {
	if f == FormatBinary {
		return "binary"
	}
	return "text"
}

// Param is a single bound parameter, captured as it appeared on the wire.
//
// Value always holds the raw wire bytes. Known reports whether the parameter's
// *value* is recoverable — text-format parameters always are, binary-format ones
// only when the type OID was captured from the backend stream and the type has a
// decoder. When Known is false, Reason says why, and nothing downstream may
// treat the parameter as though its value were understood: a query with an
// unknown parameter cannot be re-executed, and is therefore UNATTRIBUTABLE.
type Param struct {
	IsNull bool
	Format ParamFormat
	Value  []byte
	// OID is the parameter's PostgreSQL type, or 0 when it was never captured.
	OID uint32
	// Text is the decoded rendering; meaningful only when Known.
	Text string
	// Known reports whether Text carries the parameter's value.
	Known bool
	// Reason explains a false Known.
	Reason string
}

// parseParse extracts the statement name and SQL text from a Parse message.
func parseParse(body []byte) (name, sql string, err error) {
	name, i, err := cstring(body, 0)
	if err != nil {
		return "", "", err
	}
	sql, _, err = cstring(body, i)
	if err != nil {
		return "", "", err
	}
	return name, sql, nil
}

// parseClose extracts what a Close message targets: kind 'S' for a prepared
// statement, 'P' for a portal.
func parseClose(body []byte) (kind byte, name string, err error) {
	if len(body) < 1 {
		return 0, "", errShortMessage
	}
	name, _, err = cstring(body, 1)
	if err != nil {
		return 0, "", err
	}
	return body[0], name, nil
}

// parseBind extracts the portal, source statement name, and bound parameters.
func parseBind(body []byte) (portal, stmt string, params []Param, err error) {
	portal, i, err := cstring(body, 0)
	if err != nil {
		return "", "", nil, err
	}
	stmt, i, err = cstring(body, i)
	if err != nil {
		return "", "", nil, err
	}

	if i+2 > len(body) {
		return "", "", nil, errShortMessage
	}
	numFormats := int(binary.BigEndian.Uint16(body[i:]))
	i += 2
	formats := make([]ParamFormat, 0, numFormats)
	for n := 0; n < numFormats; n++ {
		if i+2 > len(body) {
			return "", "", nil, errShortMessage
		}
		formats = append(formats, ParamFormat(binary.BigEndian.Uint16(body[i:])))
		i += 2
	}

	if i+2 > len(body) {
		return "", "", nil, errShortMessage
	}
	numParams := int(binary.BigEndian.Uint16(body[i:]))
	i += 2

	params = make([]Param, 0, numParams)
	for n := 0; n < numParams; n++ {
		if i+4 > len(body) {
			return "", "", nil, errShortMessage
		}
		length := int32(binary.BigEndian.Uint32(body[i:]))
		i += 4

		// Per the protocol: zero format codes means every parameter is text; a
		// single code applies to all of them; otherwise there is one per
		// parameter.
		format := FormatText
		switch {
		case numFormats == 1:
			format = formats[0]
		case numFormats > 1 && n < numFormats:
			format = formats[n]
		}

		if length < 0 {
			// A NULL is a value the differ can rebind, so it counts as known.
			params = append(params, Param{IsNull: true, Format: format, Known: true, Text: "NULL"})
			continue
		}
		if i+int(length) > len(body) {
			return "", "", nil, errShortMessage
		}
		value := make([]byte, length)
		copy(value, body[i:i+int(length)])
		i += int(length)
		pr := Param{Format: format, Value: value}
		if format == FormatText {
			// Text format is the value's own representation.
			pr.Known, pr.Text = true, string(value)
		} else {
			// Binary needs the type OID, which only the backend stream carries.
			pr.Reason = "binary parameter; type OID not captured"
		}
		params = append(params, pr)
	}
	return portal, stmt, params, nil
}

// parseParameterDescription extracts parameter type OIDs from a backend
// ParameterDescription ('t').
func parseParameterDescription(body []byte) ([]uint32, error) {
	if len(body) < 2 {
		return nil, errShortMessage
	}
	n := int(binary.BigEndian.Uint16(body[0:2]))
	if len(body) < 2+4*n {
		return nil, errShortMessage
	}
	oids := make([]uint32, 0, n)
	for i := 0; i < n; i++ {
		oids = append(oids, binary.BigEndian.Uint32(body[2+4*i:]))
	}
	return oids, nil
}

// parseRowDescription extracts result column type OIDs from a backend
// RowDescription ('T').
func parseRowDescription(body []byte) ([]uint32, error) {
	if len(body) < 2 {
		return nil, errShortMessage
	}
	n := int(binary.BigEndian.Uint16(body[0:2]))
	i := 2
	oids := make([]uint32, 0, n)
	for f := 0; f < n; f++ {
		var err error
		if _, i, err = cstring(body, i); err != nil {
			return nil, err
		}
		// table OID(4) + column attr(2) + type OID(4) + size(2) + modifier(4) + format(2)
		if i+18 > len(body) {
			return nil, errShortMessage
		}
		oids = append(oids, binary.BigEndian.Uint32(body[i+6:]))
		i += 18
	}
	return oids, nil
}
