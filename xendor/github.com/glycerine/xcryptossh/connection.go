// Copyright 2013 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ssh

import (
	"context"
	"fmt"
	"io"
	"net"
)

// OpenChannelError is returned if the other side rejects an
// OpenChannel request.
type OpenChannelError struct {
	Reason  RejectionReason
	Message string
}

func (e *OpenChannelError) Error() string {
	return fmt.Sprintf("ssh: rejected: %s (%s)", e.Reason, e.Message)
}

// ConnMetadata holds metadata for the connection.
type ConnMetadata interface {
	// User returns the user ID for this connection.
	User() string

	// SessionID returns the session hash, also denoted by H.
	SessionID() []byte

	// ClientVersion returns the client's version string as hashed
	// into the session ID.
	ClientVersion() []byte

	// ServerVersion returns the server's version string as hashed
	// into the session ID.
	ServerVersion() []byte

	// RemoteAddr returns the remote address for this connection.
	RemoteAddr() net.Addr

	// LocalAddr returns the local address for this connection.
	LocalAddr() net.Addr
}

// Conn represents an SSH connection for both server and client roles.
// Conn is the basis for implementing an application layer, such
// as ClientConn, which implements the traditional shell access for
// clients.
type Conn interface {
	ConnMetadata

	// SendRequest sends a global request, and returns the
	// reply. If wantReply is true, it returns the response status
	// and payload. See also RFC4254, section 4.
	SendRequest(ctx context.Context, name string, wantReply bool, payload []byte) (bool, []byte, error)

	// OpenChannel tries to open an channel. If the request is
	// rejected, it returns *OpenChannelError. On success it returns
	// the SSH Channel and a Go channel for incoming, out-of-band
	// requests. The Go channel must be serviced, or the
	// connection will hang.
	OpenChannel(ctx context.Context, name string, data []byte, parHalt *Halter) (Channel, <-chan *Request, error)

	// Close closes the underlying network connection
	Close() error

	// Wait blocks until the connection has shut down, and returns the
	// error causing the shutdown.
	Wait() error

	// Done can be used to await connection shutdown. The
	// returned channel will be closed when the Conn is
	// shutting down.
	Done() <-chan struct{}

	// NcCloser retreives the underlying net.Conn so
	// that it can be closed.
	NcCloser() io.Closer

	// TODO(hanwen): consider exposing:
	//   RequestKeyChange
	//   Disconnect
}

// DiscardRequests consumes and rejects all requests from the
// passed-in channel.
func DiscardRequests(ctx context.Context, in <-chan *Request, halt *Halter) {

	var reqStop chan struct{}
	if halt != nil {
		reqStop = halt.ReqStopChan()
	}
	for {
		select {
		case req := <-in:
			if req != nil && req.WantReply {
				req.Reply(false, nil)
			}
		case <-reqStop:
			return
		case <-ctx.Done():
			return
		}
	}
}

// A connection represents an incoming connection.
type connection struct {
	transport *handshakeTransport
	sshConn

	// the Config used
	cfg *Config

	// for client connections, provides the User, HostPort
	clicfg *ClientConfig

	// clean shutdown mechanism
	halt *Halter

	// The connection protocol.
	*mux
}

func newConnection(nc net.Conn, cfg *Config, clicfg *ClientConfig) *connection {
	// clicfg will be nil for server side.

	if cfg.Halt == nil {
		panic("assert: cfg.Halt cannot be nil in newConnection()")
	}
	conn := &connection{
		sshConn: sshConn{conn: nc},
		halt:    cfg.Halt,
		cfg:     cfg,
		clicfg:  clicfg,
	}

	return conn
}

func (c *connection) Close() error {
	c.halt.RequestStop()
	return c.sshConn.conn.Close()
}

func (c *connection) Done() <-chan struct{} {
	return c.halt.ReqStopChan()
}

// sshconn provides net.Conn metadata, but disallows direct reads and
// writes.
type sshConn struct {
	conn net.Conn

	user          string
	sessionID     []byte
	clientVersion []byte
	serverVersion []byte
}

func dup(src []byte) []byte {
	dst := make([]byte, len(src))
	copy(dst, src)
	return dst
}

func (c *sshConn) NcCloser() io.Closer {
	return c.conn
}

func (c *sshConn) User() string {
	return c.user
}

func (c *sshConn) RemoteAddr() net.Addr {
	return c.conn.RemoteAddr()
}

func (c *sshConn) Close() error {
	return c.conn.Close()
}

func (c *sshConn) LocalAddr() net.Addr {
	return c.conn.LocalAddr()
}

func (c *sshConn) SessionID() []byte {
	return dup(c.sessionID)
}

func (c *sshConn) ClientVersion() []byte {
	return dup(c.clientVersion)
}

func (c *sshConn) ServerVersion() []byte {
	return dup(c.serverVersion)
}
