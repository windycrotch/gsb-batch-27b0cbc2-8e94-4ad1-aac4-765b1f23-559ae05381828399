package main

import (
	"net"
	"sync"
	"time"
)

// discardConn is an in-memory net.Conn used by tests. Writes are buffered
// (and counted) so notification messages can be asserted on, while reads block
// until the connection is closed. No real RTMP handshake is required because
// the tests drive the channel/player state machine directly.
type discardConn struct {
	mu       sync.Mutex
	closed   bool
	data     []byte
	closedCh chan struct{}
	deadline time.Time
}

func newDiscardConn() *discardConn {
	return &discardConn{closedCh: make(chan struct{})}
}

func (c *discardConn) Read(b []byte) (int, error) {
	<-c.closedCh
	return 0, net.ErrClosed
}

func (c *discardConn) Write(b []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return 0, net.ErrClosed
	}
	c.data = append(c.data, b...)
	return len(b), nil
}

func (c *discardConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.closed {
		c.closed = true
		close(c.closedCh)
	}
	return nil
}

func (c *discardConn) LocalAddr() net.Addr              { return &net.TCPAddr{} }
func (c *discardConn) RemoteAddr() net.Addr             { return &net.TCPAddr{} }
func (c *discardConn) SetDeadline(time.Time) error      { return nil }
func (c *discardConn) SetReadDeadline(time.Time) error  { return nil }
func (c *discardConn) SetWriteDeadline(time.Time) error { return nil }
func (c *discardConn) bytesWritten() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.data)
}
func (c *discardConn) isClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

// newTestServer builds an RTMPServer without binding any TCP port, so the
// tests are hermetic and can run repeatedly in parallel of the network.
func newTestServer() *RTMPServer {
	return &RTMPServer{
		mutex:             &sync.Mutex{},
		session_id_mutex:  &sync.Mutex{},
		ip_mutex:          &sync.Mutex{},
		sessions:          make(map[uint64]*RTMPSession),
		channels:          make(map[string]*RTMPChannel),
		next_session_id:   1,
		closed:            false,
		ipCount:           make(map[string]uint32),
		ipLimit:           IP_DEFAULT_LIMIT,
		gopCacheLimit:     GOP_CACHE_DEFAULT_LIMIT,
		streamIdMaxLength: STREAM_ID_DEFAULT_MAX_LENGTH,
	}
}

// newTestSession creates a registered session backed by an in-memory
// connection. The returned connection is also exposed for assertions.
func newTestSession(server *RTMPServer, channel string, key string) (*RTMPSession, *discardConn) {
	id := server.NextSessionID()
	conn := newDiscardConn()
	s := CreateRTMPSession(server, id, "127.0.0.1", conn)
	s.channel = channel
	s.key = key
	s.isConnected = true
	s.playStreamId = 1
	server.AddSession(&s)
	return &s, conn
}

// startPublish drives the full publish path used by the production code:
// SetPublisher followed by StartIdlePlayers, mirroring HandlePublish.
func startPublish(server *RTMPServer, publisher *RTMPSession, channel, key, streamID string) bool {
	publisher.channel = channel
	publisher.key = key
	publisher.stream_id = streamID
	publisher.publishStreamId = 1
	if !server.SetPublisher(channel, key, streamID, publisher) {
		return false
	}
	publisher.isPublishing = true
	publisher.StartIdlePlayers()
	return true
}

// addPlayer mirrors the server-side portion of HandlePlay: AddPlayer and, when
// the stream is live, StartPlayer on the publisher.
func addPlayer(server *RTMPServer, s *RTMPSession) (idle bool, err error) {
	idle, err = server.AddPlayer(s.channel, s.key, s)
	if err != nil || idle {
		return idle, err
	}
	publisher := server.GetPublisher(s.channel)
	if publisher != nil {
		publisher.StartPlayer(s)
	}
	return false, nil
}
