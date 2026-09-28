// Package tlsmux serves plain HTTP and HTTPS on one port.
//
// A TLS connection always opens with a handshake record, whose first byte
// is 0x16; an HTTP request opens with a method name. Peeking at that byte
// lets the helper add HTTPS without a second port — so existing installs
// need no compose change, and app versions from before HTTPS keep talking
// plain HTTP to the same address.
package tlsmux

import (
	"bufio"
	"crypto/tls"
	"errors"
	"net"
	"sync"
	"time"
)

// tlsHandshakeRecord is the TLS record type every ClientHello starts with.
const tlsHandshakeRecord = 0x16

// DefaultSniffTimeout bounds how long a connection may sit silent before
// its first byte. Without it, idle connections would pile up unclassified.
const DefaultSniffTimeout = 10 * time.Second

// Listener hands out connections that are already sorted by protocol: TLS
// ones wrapped in *tls.Conn (which net/http recognises and completes the
// handshake for), everything else as-is.
type Listener struct {
	inner        net.Listener
	config       *tls.Config
	sniffTimeout time.Duration

	conns chan net.Conn
	errs  chan error
	done  chan struct{}
	once  sync.Once
}

// New starts sorting connections from inner. Classification runs per
// connection in its own goroutine, so a slow client can't hold up Accept.
func New(inner net.Listener, config *tls.Config, sniffTimeout time.Duration) *Listener {
	l := &Listener{
		inner:        inner,
		config:       config,
		sniffTimeout: sniffTimeout,
		conns:        make(chan net.Conn),
		errs:         make(chan error),
		done:         make(chan struct{}),
	}
	go l.acceptLoop()
	return l
}

func (l *Listener) acceptLoop() {
	for {
		conn, err := l.inner.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			// Hand transient errors (EMFILE and the like) to the server,
			// which backs off before calling Accept again — the unbuffered
			// channel is what paces this loop.
			select {
			case l.errs <- err:
				continue
			case <-l.done:
				return
			}
		}
		go l.classify(conn)
	}
}

func (l *Listener) classify(conn net.Conn) {
	_ = conn.SetReadDeadline(time.Now().Add(l.sniffTimeout))
	reader := bufio.NewReader(conn)
	first, err := reader.Peek(1)
	_ = conn.SetReadDeadline(time.Time{})
	if err != nil {
		conn.Close()
		return
	}

	var sorted net.Conn = &peekedConn{Conn: conn, reader: reader}
	if first[0] == tlsHandshakeRecord {
		sorted = tls.Server(sorted, l.config)
	}
	select {
	case l.conns <- sorted:
	case <-l.done:
		sorted.Close()
	}
}

// Accept returns the next sorted connection.
func (l *Listener) Accept() (net.Conn, error) {
	select {
	case conn := <-l.conns:
		return conn, nil
	case err := <-l.errs:
		return nil, err
	case <-l.done:
		return nil, net.ErrClosed
	}
}

// Close stops accepting; connections already handed out are unaffected.
func (l *Listener) Close() error {
	l.once.Do(func() { close(l.done) })
	return l.inner.Close()
}

// Addr is the underlying listener's address.
func (l *Listener) Addr() net.Addr { return l.inner.Addr() }

// peekedConn replays the byte consumed while sniffing.
type peekedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *peekedConn) Read(p []byte) (int, error) { return c.reader.Read(p) }
