package capture

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
)

// DecodeEvents reads newline-delimited JSON events in the format Recorder
// writes and reconstructs them as Events — the exact inverse of Emit.
//
// A parameter's undecoded bytes are restored from value_base64 only when the
// wire form itself marked the parameter neither null nor known; a Known
// parameter is trusted for its Text alone, and a null parameter carries no
// value at all. This mirrors Emit's own encoding decision precisely, so a
// round trip through this pair never promotes an undecoded parameter into one
// the differ would treat as safe to bind.
func DecodeEvents(r io.Reader) ([]Event, error) {
	dec := json.NewDecoder(r)
	var out []Event
	for {
		var je jsonEvent
		if err := dec.Decode(&je); err != nil {
			if err == io.EOF {
				break
			}
			return nil, fmt.Errorf("decode event: %w", err)
		}

		e := Event{
			Conn: je.Conn, Kind: je.Kind, Stmt: je.Stmt, Portal: je.Portal,
			SQL: je.SQL, Resolved: je.Resolved, Note: je.Note,
		}
		for _, jp := range je.Params {
			p := Param{
				IsNull: jp.Null, OID: jp.OID, Known: jp.Known, Reason: jp.Reason,
				Format: FormatText,
			}
			if jp.Format == "binary" {
				p.Format = FormatBinary
			}
			switch {
			case jp.Null:
				// Deliberately empty: a null parameter carries neither Text
				// nor Value. Equivalent in practice to falling through to the
				// default branch, since Emit never writes a value_base64 for
				// a null parameter — kept as its own case for readability,
				// not because the two paths differ in output.
			case jp.Known:
				p.Text = jp.Value
			default:
				b, err := base64.StdEncoding.DecodeString(jp.ValueB64)
				if err != nil {
					return nil, fmt.Errorf("decode param value_base64: %w", err)
				}
				p.Value = b
			}
			e.Params = append(e.Params, p)
		}
		out = append(out, e)
	}
	return out, nil
}
