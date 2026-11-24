/* Copyright (c) 2016-2017 Gregor Riepl
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 */

package streaming

import (
	"errors"
	"fmt"
	"github.com/onitake/restreamer/auth"
	"github.com/onitake/restreamer/event"
	"github.com/onitake/restreamer/metrics"
	"github.com/onitake/restreamer/protocol"
	"github.com/onitake/restreamer/util"
	"github.com/prometheus/client_golang/prometheus"
	"net/http"
	"sync"
	"time"
)

var (
	// ErrAlreadyRunning is thrown when trying to connect a stream that is already online.
	ErrAlreadyRunning = errors.New("restreamer: service is already active")
	// ErrNotRunning is thrown trying to shut down a stopped stream.
	ErrNotRunning = errors.New("restreamer: service is not running")
	// ErrOffline is thrown when receiving a connection while the stream is offline
	ErrOffline = errors.New("restreamer: refusing connection on an offline stream")
	// ErrSlowRead is logged (not thrown) when a client can not handle the bandwidth.
	ErrSlowRead = errors.New("restreamer: send buffer overrun, increase client bandwidth")
	// ErrPoolFull is logged when the connection pool is full.
	ErrPoolFull = errors.New("restreamer: maximum number of active connections exceeded")
)

var (
	metricPacketsSent = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "streaming_packets_sent",
			Help: "Total number of MPEG-TS packets sent from the output queue.",
		},
		[]string{"stream"},
	)
	metricBytesSent = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "streaming_bytes_sent",
			Help: "Total number of bytes sent from the output queue.",
		},
		[]string{"stream"},
	)
	metricPacketsDropped = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "streaming_packets_dropped",
			Help: "Total number of MPEG-TS packets dropped from the output queue.",
		},
		[]string{"stream"},
	)
	metricBytesDropped = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "streaming_bytes_dropped",
			Help: "Total number of bytes dropped from the output queue.",
		},
		[]string{"stream"},
	)
	metricConnections = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "streaming_connections",
			Help: "Number of active client connections.",
		},
		[]string{"stream"},
	)
	metricDuration = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "streaming_duration",
			Help: "Total time spent streaming, summed over all client connections. In nanoseconds.",
		},
		[]string{"stream"},
	)
)

func init() {
	metrics.MustRegister(metricPacketsSent)
	metrics.MustRegister(metricBytesSent)
	metrics.MustRegister(metricPacketsDropped)
	metrics.MustRegister(metricBytesDropped)
	metrics.MustRegister(metricConnections)
	metrics.MustRegister(metricDuration)
}

// Command is one of several possible constants.
// See StreamerCommandAdd for more information.
type Command int

const (
	// streamerCommandIgnore is a default dummy command
	streamerCommandIgnore Command = iota
	// streamerCommandStart is an internal start command, used to signal request
	// processing to commence.
	streamerCommandStart
	// StreamerCommandAdd signals a stream to add a connection.
	StreamerCommandAdd
	// StreamerCommandRemove signals a stream to remove a connection.
	StreamerCommandRemove
	// StreamerCommandInhibit signals that all connections should be closed
	// and not further connections should be allowed
	StreamerCommandInhibit
	// StreamerCommandAllow signals that new connections should be allowed
	StreamerCommandAllow
)

// ConnectionRequest encapsulates a request that new connection be added or removed.
type ConnectionRequest struct {
	// Command is the command to execute
	Command Command
	// Address is the remote client address
	Address string
	// Connection is the connection to add (if this is an Add command)
	Connection *Connection
	// Waiter is a WaitGroup that can be used to track handling of the connection
	// in the streaming thread. If it is non-nil, the streamer will signal
	// Done once the request has been handled.
	Waiter *sync.WaitGroup
	// Ok tells the caller if a connection was handled without error.
	// You should always wait on the Waiter before checking it.
	Ok bool
}

// Streamer implements a TS packet multiplier,
// distributing received packets on the input queue to the output queues.
// It also handles and manages HTTP connections when added to an HTTP server.
type Streamer struct {
	// name is a unique name for this stream, only used for logging and metrics
	name string
	// lock is the outgoing connection pool lock
	lock sync.Mutex
	// streamLock serializes Stream invocations.
	// An upstream reconnect may start a new Stream before the previous one
	// has noticed that its input queue was closed; waiting here guarantees
	// that only one stream loop consumes packets at a time and that the new
	// input queue is never left unread.
	streamLock sync.Mutex
	// broker is a global connection broker
	broker ConnectionBroker
	// queueSize defines the maximum number of packets to queue per outgoing connection
	queueSize int
	// running reflects the state of the stream: if true, the Stream thread is running and
	// incoming connections are allowed.
	// If false, incoming connections are blocked.
	running util.AtomicBool
	// inhibited reflects the operator-controlled online/offline state.
	// If true, no new connections are accepted and all existing connections
	// are terminated. The state is kept on the streamer (instead of the
	// stream loop) so that it survives upstream reconnects and can be set
	// even while no stream is running.
	inhibited util.AtomicBool
	// stats is the statistics collector for this stream
	stats metrics.Collector
	// request is an unbuffered queue for requests to add or remove a connection
	request chan *ConnectionRequest
	// events is an event receiver
	events event.Notifiable
	// auth is an authentication verifier for client requests
	auth auth.Authenticator
	// promCounter allows enabling/disabling Prometheus packet metrics.
	promCounter bool
	// preamble contains a static preamble that is sent before the actual streamed data
	preamble []byte
}

// ConnectionBroker represents a policy handler for new connections.
// It is used to determine if new connections can be accepted,
// based on arbitrary rules.
type ConnectionBroker interface {
	// Accept will be called on each incoming connection,
	// with the remote client address and the streamer that wants to accept the connection.
	Accept(remoteaddr string, streamer *Streamer) bool
	// Release will be called each time a client disconnects.
	// The streamer argument corresponds to a streamer that has previously called Accept().
	Release(streamer *Streamer)
}

// NewStreamer creates a new packet streamer.
// queue is an input packet queue.
// qsize is the length of each connection's queue (in packets).
// broker handles policy enforcement
// stats is a statistics collector object.
func NewStreamer(name string, qsize uint, broker ConnectionBroker, auth auth.Authenticator) *Streamer {
	if broker == nil {
		// Be lenient about a missing broker: an unlimited access controller
		// behaves like "allow everything", which is the least surprising
		// default.
		broker = NewAccessController(0)
	}
	streamer := &Streamer{
		name:      name,
		broker:    broker,
		queueSize: int(qsize),
		running:   util.AtomicFalse,
		inhibited: util.AtomicFalse,
		stats:     &metrics.DummyCollector{},
		request:   make(chan *ConnectionRequest),
		auth:      auth,
	}
	// start the command eater
	go streamer.eatCommands()
	return streamer
}

// SetCollector assigns a stats collector
func (streamer *Streamer) SetCollector(stats metrics.Collector) {
	if stats == nil {
		stats = &metrics.DummyCollector{}
	}
	streamer.stats = stats
}

// SetNotifier assigns an event notifier
func (streamer *Streamer) SetNotifier(events event.Notifiable) {
	streamer.events = events
}

func (streamer *Streamer) SetPreamble(preamble []byte) {
	streamer.preamble = preamble
}

// notifyConnect forwards a connection count change to the event notifier,
// if one has been configured.
func (streamer *Streamer) notifyConnect(connected int) {
	if streamer.events != nil {
		streamer.events.NotifyConnect(connected)
	}
}

// isInhibited returns the current operator-controlled online/offline state.
func (streamer *Streamer) isInhibited() bool {
	return util.LoadBool(&streamer.inhibited)
}

// SetInhibit switches the stream between the offline (inhibit=true) and
// online (inhibit=false) state.
//
// While offline, new connections are refused and all existing downstream
// connections are terminated. The state is persistent: it survives upstream
// reconnects and is only reset by another call with inhibit=false.
//
// The method returns after the stream loop has processed the state change,
// so callers can rely on new connections being refused immediately and on
// the termination of existing connections having been initiated. The flag is
// also set synchronously, so it takes effect even when no stream loop is
// running.
func (streamer *Streamer) SetInhibit(inhibit bool) {
	// Update the state first so that any connection attempt that races with
	// the command below is already refused.
	util.StoreBool(&streamer.inhibited, inhibit)
	// Without a running stream loop there is no connection pool to drain.
	// The flag above is enough; this also avoids blocking on the request
	// channel while the loop is being restarted after an upstream loss.
	if !util.LoadBool(&streamer.running) {
		return
	}
	command := StreamerCommandAllow
	if inhibit {
		command = StreamerCommandInhibit
	}
	waiter := &sync.WaitGroup{}
	waiter.Add(1)
	streamer.request <- &ConnectionRequest{
		Command: command,
		Waiter:  waiter,
	}
	// Wait until the request has been handled, so that the caller
	// (the control API) can report completion.
	waiter.Wait()
}

// eatCommands is started in the background to drain the command
// queue and wait for a start command, in which case it will exit.
func (streamer *Streamer) eatCommands() {
	running := true
	for running {
		select {
		case request := <-streamer.request:
			switch request.Command {
			case streamerCommandStart:
				logger.Logkv(
					"event", eventStreamerQueueStart,
					"message", "Stopping eater process and starting real processing",
				)
				running = false
			default:
				// Eating all other commands
			}
			// make sure the caller isn't waiting forever
			if request.Waiter != nil {
				request.Waiter.Done()
			}
		}
	}
}

// Stream is the main stream multiplier loop.
// It reads data from the input queue and distributes it to the connections.
//
// This routine will block; you should run it asynchronously like this:
//
// queue := make(chan protocol.MpegTsPacket, inputQueueSize)
//
//	go func() {
//	  log.Fatal(streamer.Stream(queue))
//	}
//
// or simply:
//
// go streamer.Stream(queue)
func (streamer *Streamer) Stream(queue <-chan protocol.MpegTsPacket) error {
	// Only one stream loop may consume packets at a time.
	//
	// An upstream reconnect creates a new input queue and starts a new stream
	// loop while the previous one may still be draining its old queue. The
	// lock makes the new invocation wait for the old one instead of failing
	// with ErrAlreadyRunning (which would leave the new queue unread and all
	// viewers without data).
	streamer.streamLock.Lock()
	defer streamer.streamLock.Unlock()

	// interlock and check for availability first
	if !util.CompareAndSwapBool(&streamer.running, false, true) {
		return ErrAlreadyRunning
	}

	// create the local outgoing connection pool
	pool := make(map[*Connection]bool)

	// stop the eater process
	streamer.request <- &ConnectionRequest{
		Command: streamerCommandStart,
	}

	logger.Logkv(
		"event", eventStreamerStart,
		"message", "Starting streaming",
	)

	// Loop until the input channel is closed.
	//
	// The offline state is kept on the streamer and read on each connection
	// attempt, so it survives upstream reconnects and is not reset when a new
	// stream loop starts.
	running := true
	for running {
		select {
		case packet, ok := <-queue:
			if ok {
				// got a packet, distribute
				for conn := range pool {
					switch conn.trySend(packet) {
					case sendQueued:
						// packet distributed, done
						// report the packet
						streamer.stats.PacketSent()
						if streamer.promCounter {
							metricPacketsSent.With(prometheus.Labels{"stream": streamer.name}).Inc()
							metricBytesSent.With(prometheus.Labels{"stream": streamer.name}).Add(protocol.MpegTsPacketSize)
						}

					case sendClosed:
						// The connection has already been terminated (offline
						// switch or upstream loss). Remove it from the pool so
						// we don't try to send to it again, and settle its
						// accounting (idempotent) so the quota is available
						// again immediately.
						delete(pool, conn)
						streamer.settleConnection(conn)

					default:
						// queue is full
						// report the drop
						streamer.stats.PacketDropped()
						if streamer.promCounter {
							metricPacketsDropped.With(prometheus.Labels{"stream": streamer.name}).Inc()
							metricBytesDropped.With(prometheus.Labels{"stream": streamer.name}).Add(protocol.MpegTsPacketSize)
						}
					}
				}
			} else {
				// channel closed, exit
				running = false
				// and stop everything
				util.StoreBool(&streamer.running, false)
			}
		case request := <-streamer.request:
			switch request.Command {
			case StreamerCommandRemove:
				logger.Logkv(
					"event", eventStreamerClientRemove,
					"message", fmt.Sprintf("Removing client %s from pool", request.Address),
				)
				// Terminate the connection queue (idempotent) and forget the
				// connection. Removing an already removed connection is a
				// no-op; this happens after an offline switch or a stream
				// restart, where connections are removed in bulk.
				request.Connection.closeQueue()
				delete(pool, request.Connection)
				// The connection has ended; settle its accounting exactly
				// once, no matter how many removal paths race here.
				streamer.settleConnection(request.Connection)
			case StreamerCommandAdd:
				// check if the connection can be accepted.
				// The offline flag is read from the streamer so the decision
				// is consistent across stream restarts.
				if !streamer.isInhibited() && streamer.broker.Accept(request.Address, streamer) {
					logger.Logkv(
						"event", eventStreamerClientAdd,
						"remote", request.Address,
						"message", fmt.Sprintf("Adding client %s to pool", request.Address),
					)
					// Account for the viewer in the same critical section
					// that makes it visible, so the reported connection
					// count never disagrees with the pool.
					request.Connection.startedAt = time.Now()
					pool[request.Connection] = true
					streamer.stats.ConnectionAdded()
					metricConnections.With(prometheus.Labels{"stream": streamer.name}).Inc()
					streamer.notifyConnect(1)
					request.Ok = true
				} else {
					logger.Logkv(
						"event", eventStreamerError,
						"error", errorStreamerPoolFull,
						"remote", request.Address,
						"message", fmt.Sprintf("Refusing connection from %s, pool is full or offline", request.Address),
					)
					request.Ok = false
				}
			case StreamerCommandInhibit:
				logger.Logkv(
					"event", eventStreamerInhibit,
					"message", fmt.Sprintf("Turning stream offline"),
				)
				// the persistent flag has already been set by SetInhibit;
				// make sure it is set even if the command was submitted
				// through the request channel directly.
				util.StoreBool(&streamer.inhibited, true)
				// Close all downstream connections in an orderly fashion and
				// remove them from the pool. Closing the queue ends Serve
				// (and thus the HTTP response) normally; the quota of each
				// connection is returned exactly once here, so it is
				// available again as soon as the stream is put back online.
				for conn := range pool {
					conn.closeQueue()
					delete(pool, conn)
					streamer.settleConnection(conn)
				}
			case StreamerCommandAllow:
				logger.Logkv(
					"event", eventStreamerAllow,
					"message", fmt.Sprintf("Turning stream online"),
				)
				util.StoreBool(&streamer.inhibited, false)
			default:
				logger.Logkv(
					"event", eventStreamerError,
					"error", errorStreamerInvalidCommand,
					"message", "Ignoring invalid command in started state",
				)
			}
			// signal the caller that we have handled the message
			if request.Waiter != nil {
				request.Waiter.Done()
			}
		}
	}

	// clean up
	for range queue {
		// drain any leftovers
	}
	for conn := range pool {
		conn.closeQueue()
		delete(pool, conn)
		streamer.settleConnection(conn)
	}

	// start the command eater again
	go streamer.eatCommands()

	logger.Logkv(
		"event", eventStreamerStop,
		"message", "Ending streaming",
	)
	return nil
}

// ServeHTTP handles an incoming HTTP connection.
// Satisfies the http.Handler interface, so it can be used in an HTTP server.
func (streamer *Streamer) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	// fail-fast: verify that this user can access this resource first
	if !auth.HandleHttpAuthentication(streamer.auth, request, writer) {
		return
	}

	// create the connection object first
	conn := NewConnection(writer, streamer.queueSize, request.RemoteAddr, request.Context())
	// and pass it on
	command := &ConnectionRequest{
		Command:    StreamerCommandAdd,
		Address:    request.RemoteAddr,
		Connection: conn,
		Waiter:     &sync.WaitGroup{},
	}
	command.Waiter.Add(1)
	streamer.request <- command

	// wait for the handler
	command.Waiter.Wait()

	// verify that the connection was added
	if !command.Ok {
		// nope, destroy the connection
		conn = nil
		logger.Logkv(
			"event", eventStreamerError,
			"error", errorStreamerOffline,
			"message", fmt.Sprintf("Refusing connection from %s, stream is offline", request.RemoteAddr),
		)
	}

	if conn != nil {
		logger.Logkv(
			"event", eventStreamerStreaming,
			"message", fmt.Sprintf("Streaming to %s", request.RemoteAddr),
			"remote", request.RemoteAddr,
		)

		conn.Serve(streamer.preamble)
		duration := time.Since(conn.startedAt)

		// Terminate the queue idempotently. An offline switch or a stream
		// restart may already have closed it; in that case this is a no-op.
		conn.closeQueue()

		// Settle the accounting before handing the removal to the stream
		// loop. The stream loop normally also settles when it processes the
		// remove command, but it may already have ended (upstream loss) - in
		// which case the command is eaten by the idle command eater. Settling
		// here guarantees that the quota is returned promptly, and
		// settleConnection() being idempotent guarantees it is returned only
		// once even if both paths run.
		streamer.settleConnection(conn)

		// Remove the connection from the active pool.
		streamer.request <- &ConnectionRequest{
			Command:    StreamerCommandRemove,
			Address:    request.RemoteAddr,
			Connection: conn,
		}
		// and drain the queue AFTER we have sent the shutdown signal
		for range conn.Queue {
			// drain any leftovers
		}
		logger.Logkv(
			"event", eventStreamerClosed,
			"message", fmt.Sprintf("Connection from %s closed", request.RemoteAddr),
			"remote", request.RemoteAddr,
			"duration", duration,
		)
	} else {
		// Return a suitable error
		// TODO This should be 503 or 504, but client support seems to be poor
		// and the standards mandate nothing. Bummer.
		ServeStreamError(writer, http.StatusNotFound)
	}
}

// settleConnection settles the accounting for a connection that has ended.
//
// It reports the disconnect, updates the statistics and returns the quota to
// the broker exactly once per accepted connection. Calling it again for the
// same connection is a no-op, which makes it safe when an offline switch, an
// upstream loss and a client disconnect race with each other.
func (streamer *Streamer) settleConnection(conn *Connection) {
	conn.finishOnce.Do(func() {
		duration := time.Since(conn.startedAt)
		streamer.notifyConnect(-1)
		streamer.stats.ConnectionRemoved()
		metricConnections.With(prometheus.Labels{"stream": streamer.name}).Dec()
		streamer.stats.StreamDuration(duration)
		metricDuration.With(prometheus.Labels{"stream": streamer.name}).Add(float64(duration))

		// also notify the broker
		streamer.broker.Release(streamer)
	})
}
