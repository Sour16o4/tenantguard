package capture

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync/atomic"
)

// Sink receives captured events. Implementations must tolerate concurrent calls
// from multiple connections.
type Sink interface {
	Emit(Event)
}

// SinkFunc adapts a function to Sink.
type SinkFunc func(Event)

func (f SinkFunc) Emit(e Event) { f(e) }

// Proxy forwards a PostgreSQL client connection to an upstream server and
// records the frontend messages it carries.
//
// It never modifies the byte stream in either direction. Capture is strictly an
// observation; the bytes the server sees are the bytes the client sent.
type Proxy struct {
	// Upstream is the address of the real PostgreSQL server.
	Upstream string
	// Sink receives every captured event.
	Sink Sink

	connSeq atomic.Int64
}

// ErrTLSRequired reports a client that would not proceed without TLS.
//
// The proxy refuses TLS so the stream stays readable, which means a client
// configured with sslmode=require cannot connect through it. This is a stated
// Tier 1 constraint (SRS C-2), not a defect.
var ErrTLSRequired = errors.New("capture: client requires TLS; Tier 1 needs a non-TLS test DSN")

// Serve accepts connections until ln is closed.
func (p *Proxy) Serve(ln net.Listener) error {
	for {
		c, err := ln.Accept()
		if err != nil {
			return err
		}
		go func() {
			id := int(p.connSeq.Add(1))
			if err := p.handle(c, id); err != nil && p.Sink != nil {
				p.Sink.Emit(Event{Conn: id, Kind: "error", Note: err.Error()})
			}
		}()
	}
}

func (p *Proxy) handle(client net.Conn, id int) error {
	defer client.Close()

	server, err := net.Dial("tcp", p.Upstream)
	if err != nil {
		return fmt.Errorf("dial upstream: %w", err)
	}
	defer server.Close()

	if err := p.startup(client, server, id); err != nil {
		return err
	}

	sess := NewSession(id)
	done := make(chan struct{}, 2)

	// Server to client: forwarded untouched, and observed. Parameter type OIDs
	// travel only in this direction (design §9.5), so capture is bidirectional.
	go func() {
		buf := make([]byte, 16*1024)
		for {
			n, rerr := server.Read(buf)
			if n > 0 {
				if _, werr := client.Write(buf[:n]); werr != nil {
					break
				}
				for _, ev := range sess.FeedBackend(buf[:n]) {
					if p.Sink != nil {
						p.Sink.Emit(ev)
					}
				}
			}
			if rerr != nil {
				break
			}
		}
		done <- struct{}{}
	}()

	// Client to server: forwarded untouched, and observed.
	go func() {
		buf := make([]byte, 16*1024)
		for {
			n, rerr := client.Read(buf)
			if n > 0 {
				if _, werr := server.Write(buf[:n]); werr != nil {
					break
				}
				for _, ev := range sess.FeedFrontend(buf[:n]) {
					if p.Sink != nil {
						p.Sink.Emit(ev)
					}
				}
			}
			if rerr != nil {
				break
			}
		}
		done <- struct{}{}
	}()

	<-done
	return nil
}

// startup consumes the startup phase, answering SSL and GSS requests with a
// refusal so the remaining stream stays in cleartext and therefore readable.
func (p *Proxy) startup(client, server net.Conn, id int) error {
	for {
		var header [4]byte
		if _, err := io.ReadFull(client, header[:]); err != nil {
			return fmt.Errorf("read startup length: %w", err)
		}
		length := binary.BigEndian.Uint32(header[:])
		if length < 8 || length > 1<<20 {
			return fmt.Errorf("implausible startup length %d", length)
		}
		body := make([]byte, length-4)
		if _, err := io.ReadFull(client, body); err != nil {
			return fmt.Errorf("read startup body: %w", err)
		}

		code := binary.BigEndian.Uint32(body[:4])
		if length == 8 && (code == sslRequestCode || code == gssRequestCode) {
			// 'N' declines the encryption request. A client that requires it
			// will close the connection; see ErrTLSRequired.
			if _, err := client.Write([]byte{'N'}); err != nil {
				return err
			}
			continue
		}

		if _, err := server.Write(header[:]); err != nil {
			return err
		}
		if _, err := server.Write(body); err != nil {
			return err
		}
		if p.Sink != nil {
			p.Sink.Emit(Event{Conn: id, Kind: "startup", Resolved: true})
		}
		return nil
	}
}
