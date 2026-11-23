package main

import (
	"fmt"
	"log"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/webrtc/v3"
)

// MediaState describes the lifecycle state of one RTP media path (audio or video).
type MediaState int

const (
	// MediaStateActive means the UDP socket is bound and packets are being read.
	MediaStateActive MediaState = iota
	// MediaStateReconnecting means a transient failure occurred and bounded
	// retries are in progress.
	MediaStateReconnecting
	// MediaStateStopped means the retry budget was exhausted; the HTTP service
	// keeps running and a background task keeps trying to recover the media.
	MediaStateStopped
)

func (s MediaState) String() string {
	switch s {
	case MediaStateActive:
		return "active"
	case MediaStateReconnecting:
		return "reconnecting"
	case MediaStateStopped:
	default:
	}
	return "stopped"
}

// MarshalJSON renders the state as its readable name in status responses.
func (s MediaState) MarshalJSON() ([]byte, error) {
	return []byte(`"` + s.String() + `"`), nil
}

// UnmarshalJSON parses the readable state name, accepting the numeric form too
// so older status payloads remain decodable.
func (s *MediaState) UnmarshalJSON(data []byte) error {
	switch string(data) {
	case `"active"`, "0":
		*s = MediaStateActive
	case `"reconnecting"`, "1":
		*s = MediaStateReconnecting
	case `"stopped"`, "2":
		*s = MediaStateStopped
	default:
		return fmt.Errorf("unknown media state %s", string(data))
	}
	return nil
}

// MediaSnapshot is an immutable view of a MediaSource, safe to expose over HTTP.
type MediaSnapshot struct {
	Kind            string     `json:"kind"`
	State           MediaState `json:"state"`
	Address         string     `json:"address"`
	Failures        int        `json:"failures"`
	PacketsReceived int64      `json:"packetsReceived"`
}

// udpConn is the subset of *net.UDPConn the read loop depends on. Declaring it
// as an interface lets tests inject failing connections without touching real
// sockets.
type udpConn interface {
	ReadFromUDP(b []byte) (int, *net.UDPAddr, error)
	Close() error
}

// dialUDPFn dials (binds) the UDP socket used by a media source. It is a field
// so tests can inject deterministic failures.
type dialUDPFn func(ip string, port int) (udpConn, error)

func defaultDialUDP(ip string, port int) (udpConn, error) {
	return net.ListenUDP("udp", &net.UDPAddr{
		IP:   net.ParseIP(ip),
		Port: port,
	})
}

// MediaSource owns one UDP RTP reception path and writes the packets into the
// shared WebRTC local track. Failures never crash the process: they are
// observed through Snapshot(), retried a bounded number of times and, once the
// budget is exhausted, the media path is parked in the "stopped" state while a
// background recovery loop keeps trying to bring it back.
type MediaSource struct {
	kind  string
	ip    string
	port  int
	track *webrtc.TrackLocalStaticRTP

	// maxFailures is the number of consecutive bind/read failures tolerated
	// before the media path is parked.
	maxFailures int
	// retryInterval is the back-off between bounded retries.
	retryInterval time.Duration
	// recoveryInterval is the back-off used by the background recovery loop
	// once the retry budget is exhausted.
	recoveryInterval time.Duration
	// dialUDP is the socket constructor (injectable for tests).
	dialUDP dialUDPFn
	// sleep is the wait function (injectable for tests).
	sleep func(time.Duration) bool

	mu              sync.RWMutex
	state           MediaState
	failures        int
	packetsReceived atomic.Int64
	activeConn      udpConn
	boundAddress    string

	stopOnce sync.Once
	stopCh   chan struct{}
	doneCh   chan struct{}
}

// NewMediaSource builds a media source with production defaults.
func NewMediaSource(kind, ip string, port int, track *webrtc.TrackLocalStaticRTP) *MediaSource {
	m := &MediaSource{
		kind:             kind,
		ip:               ip,
		port:             port,
		track:            track,
		maxFailures:      defaultMaxFailures,
		retryInterval:    defaultRetryInterval,
		recoveryInterval: defaultRecoveryInterval,
		dialUDP:          defaultDialUDP,
		state:            MediaStateReconnecting,
		stopCh:           make(chan struct{}),
		doneCh:           make(chan struct{}),
	}
	m.sleep = m.wait
	return m
}

const (
	defaultMaxFailures      = 5
	defaultRetryInterval    = 2 * time.Second
	defaultRecoveryInterval = 10 * time.Second
	readBufferSize          = 1500
)

// Run is the reception loop. It returns when Stop is called.
func (m *MediaSource) Run() {
	defer close(m.doneCh)

	for {
		select {
		case <-m.stopCh:
			return
		default:
		}

		conn, ok := m.connect()
		if !ok {
			// Retry budget exhausted: park the media path and wait for the
			// background recovery loop to bring it back.
			m.setState(MediaStateStopped)
			conn, ok = m.recoverLoop()
			if !ok {
				return // stopping
			}
		}

		m.setActiveConn(conn)
		m.setState(MediaStateActive)
		if !m.readLoop(conn) {
			return // stopping
		}
		m.setActiveConn(nil)
		// readLoop returned because of a read error: loop back and rebind.
	}
}

// connect performs the bounded retry phase around socket binding. It returns
// a ready-to-read connection, or false when the source is stopping or the
// retry budget has been exhausted.
func (m *MediaSource) connect() (udpConn, bool) {
	for {
		select {
		case <-m.stopCh:
			return nil, false
		default:
		}

		conn, err := m.dialUDP(m.ip, m.port)
		if err == nil {
			m.noteSuccess()
			address := m.recordBoundAddress(conn)
			log.Printf("UDP RTP socket (%s) bound on %s", m.kind, address)
			return conn, true
		}

		failures := m.noteFailure()
		log.Printf("UDP RTP bind error (%s) on %s:%d: %v (failure %d/%d)",
			m.kind, m.ip, m.port, err, failures, m.maxFailures)

		if failures >= m.maxFailures {
			return nil, false
		}
		m.setState(MediaStateReconnecting)
		if !m.sleep(m.retryInterval) {
			return nil, false
		}
	}
}

// recoverLoop keeps trying to rebind the socket with a long back-off while the
// media path is parked. It never gives up on its own: recovery is always
// possible once the port/network issue disappears.
func (m *MediaSource) recoverLoop() (udpConn, bool) {
	for {
		select {
		case <-m.stopCh:
			return nil, false
		default:
		}

		log.Printf("UDP RTP media (%s) parked after %d failures; retrying recovery every %s",
			m.kind, m.maxFailures, m.recoveryInterval)
		if !m.sleep(m.recoveryInterval) {
			return nil, false
		}

		conn, err := m.dialUDP(m.ip, m.port)
		if err != nil {
			log.Printf("UDP RTP recovery attempt (%s) still failing: %v", m.kind, err)
			continue
		}
		m.noteSuccess()
		address := m.recordBoundAddress(conn)
		log.Printf("UDP RTP media (%s) recovered on %s", m.kind, address)
		return conn, true
	}
}

// readLoop reads packets until a read error occurs or the source is stopped.
// It returns false when the source must terminate, true when a read error
// happened and the caller should rebind and retry.
func (m *MediaSource) readLoop(conn udpConn) bool {
	buf := make([]byte, readBufferSize)
	for {
		select {
		case <-m.stopCh:
			_ = conn.Close()
			return false
		default:
		}

		n, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			select {
			case <-m.stopCh:
				_ = conn.Close()
				return false
			default:
			}
			_ = conn.Close()

			failures := m.noteFailure()
			log.Printf("UDP RTP read error (%s) on %s:%d: %v (failure %d/%d)",
				m.kind, m.ip, m.port, err, failures, m.maxFailures)

			if failures >= m.maxFailures {
				m.setState(MediaStateStopped)
				return true
			}
			m.setState(MediaStateReconnecting)
			if !m.sleep(m.retryInterval) {
				return false
			}
			return true
		}

		if n > 0 {
			// A successful read proves the path is healthy; reset the budget.
			m.notePacket()
			if _, err := m.track.Write(buf[:n]); err != nil {
				// A track write failure does not tear down the UDP path:
				// it just means no viewer is currently attached.
				log.Printf("RTP track write (%s) failed: %v", m.kind, err)
			}
		}
	}
}

// Stop terminates the reception loop and waits for it to exit. It is safe to
// call multiple times.
func (m *MediaSource) Stop() {
	m.stopOnce.Do(func() {
		close(m.stopCh)
		// Closing the active socket unblocks a ReadFromUDP that is otherwise
		// waiting for the next packet, so Run can actually exit.
		if conn := m.activeConnection(); conn != nil {
			_ = conn.Close()
		}
	})
	<-m.doneCh
}

func (m *MediaSource) setActiveConn(conn udpConn) {
	m.mu.Lock()
	m.activeConn = conn
	m.mu.Unlock()
}

// recordBoundAddress stores and returns the address the socket is bound to.
func (m *MediaSource) recordBoundAddress(conn udpConn) string {
	address := net.JoinHostPort(m.ip, strconv.Itoa(m.port))
	if local, ok := conn.(interface{ LocalAddr() net.Addr }); ok && local.LocalAddr() != nil {
		address = local.LocalAddr().String()
	}
	m.setBoundAddress(address)
	return address
}

func (m *MediaSource) setBoundAddress(addr string) {
	m.mu.Lock()
	m.boundAddress = addr
	m.mu.Unlock()
}

func (m *MediaSource) activeConnection() udpConn {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.activeConn
}

// wait sleeps for d but returns early (false) when the source is stopped.
func (m *MediaSource) wait(d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-m.stopCh:
		return false
	case <-timer.C:
		return true
	}
}

func (m *MediaSource) noteFailure() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.failures++
	return m.failures
}

func (m *MediaSource) noteSuccess() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.failures = 0
	m.state = MediaStateActive
}

// notePacket accounts for a received packet and clears the consecutive-failure
// budget without taking the state lock on every packet.
func (m *MediaSource) notePacket() {
	m.packetsReceived.Add(1)
	m.mu.Lock()
	m.failures = 0
	m.mu.Unlock()
}

func (m *MediaSource) setState(state MediaState) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state != state {
		log.Printf("UDP RTP media (%s) state: %s -> %s", m.kind, m.state, state)
		m.state = state
	}
}

// Snapshot returns an immutable view of the current media state.
func (m *MediaSource) Snapshot() MediaSnapshot {
	m.mu.RLock()
	defer m.mu.RUnlock()
	address := m.boundAddress
	if address == "" {
		address = net.JoinHostPort(m.ip, strconv.Itoa(m.port))
	}
	return MediaSnapshot{
		Kind:            m.kind,
		State:           m.state,
		Address:         address,
		Failures:        m.failures,
		PacketsReceived: m.packetsReceived.Load(),
	}
}

// Track exposes the local WebRTC track fed by this media source.
func (m *MediaSource) Track() *webrtc.TrackLocalStaticRTP {
	return m.track
}
