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
	"context"
	"fmt"
	"github.com/onitake/restreamer/protocol"
	"net/http"
	"sync"
	"time"
)

// sendResult describes the outcome of an attempt to queue a packet for a
// connection.
type sendResult int

const (
	// sendQueued means the packet was handed over to the client queue.
	sendQueued sendResult = iota
	// sendDropped means the client queue was full and the packet was dropped.
	sendDropped
	// sendClosed means the client connection has already been shut down.
	sendClosed
)

// Connection is a single active client connection.
//
// This is meant to be called directly from a ServeHTTP handler.
// No separate thread is created.
type Connection struct {
	// Queue is the per-connection packet queue.
	//
	// Do not close or write to this channel directly: use closeQueue() to
	// terminate the connection and trySend() to enqueue packets. Both
	// methods synchronize on queueLock, which makes it safe for the stream
	// loop and the connection handler to race on connection shutdown.
	Queue chan protocol.MpegTsPacket
	// ClientAddress is the remote client address
	ClientAddress string
	// the destination socket
	writer http.ResponseWriter
	// Closed is true once the connection queue has been closed.
	// It is only written by closeQueue(), under queueLock.
	Closed bool
	// context contains the cached context object for this connection
	context context.Context
	// queueLock protects Queue, Closed and queueClosed against concurrent
	// access from the stream loop (send) and the connection handler or
	// control interface (close).
	queueLock sync.Mutex
	// queueClosed is true after the queue has been closed.
	queueClosed bool
	// finishOnce makes sure the connection quota is returned and the
	// statistics are settled no more than once.
	finishOnce sync.Once
	// startedAt records the time the connection was accepted.
	startedAt time.Time
}

// closeQueue terminates the packet queue of this connection.
//
// It is safe to call this method more than once and from multiple goroutines:
// only the first call has an effect. All other calls are silently ignored.
// Because trySend() synchronizes on the same lock, a concurrent packet send
// can never observe a closed channel.
func (conn *Connection) closeQueue() {
	conn.queueLock.Lock()
	defer conn.queueLock.Unlock()
	if conn.queueClosed {
		return
	}
	conn.queueClosed = true
	conn.Closed = true
	close(conn.Queue)
}

// trySend attempts to enqueue a packet without blocking.
//
// It returns sendQueued if the packet was accepted, sendDropped if the queue
// is full and sendClosed if the connection has been shut down.
func (conn *Connection) trySend(packet protocol.MpegTsPacket) sendResult {
	conn.queueLock.Lock()
	defer conn.queueLock.Unlock()
	if conn.queueClosed {
		return sendClosed
	}
	select {
	case conn.Queue <- packet:
		return sendQueued
	default:
		return sendDropped
	}
}

// NewConnection creates a new connection object.
// To start sending data to a client, call Serve().
//
// clientaddr should point to the remote address of the connecting client
// and will be used for logging.
func NewConnection(destination http.ResponseWriter, qsize int, clientaddr string, ctx context.Context) *Connection {
	conn := &Connection{
		Queue:         make(chan protocol.MpegTsPacket, qsize),
		ClientAddress: clientaddr,
		writer:        destination,
		context:       ctx,
	}
	return conn
}

// Serve starts serving data to a client, continuously feeding packets from the queue.
// An optional preamble buffer can be passed that will be sent before streaming the live payload
// (but after the HTTP response headers).
func (conn *Connection) Serve(preamble []byte) {
	// set the content type (important)
	conn.writer.Header().Set("Content-Type", "video/mpeg")
	// a stream is always current
	conn.writer.Header().Set("Last-Modified", time.Now().UTC().Format(http.TimeFormat))
	// other headers to comply with the specs
	conn.writer.Header().Set("Accept-Range", "none")
	// suppress caching by intermediate proxies
	conn.writer.Header().Set("Cache-Control", "no-cache,no-store,no-transform")
	// use Add and Set to set more headers here
	// chunked mode should be on by default
	conn.writer.WriteHeader(http.StatusOK)
	// try to flush the header
	flusher, ok := conn.writer.(http.Flusher)
	if !ok {
		logger.Logkv(
			"event", eventConnectionError,
			"error", errorConnectionNotFlushable,
			"message", "ResponseWriter is not flushable!",
		)
	} else {
		flusher.Flush()
	}
	logger.Logkv(
		"event", eventHeaderSent,
		"message", "Sent header",
	)

	running := true

	// send the preamble
	if len(preamble) > 0 {
		_, err := conn.writer.Write(preamble)
		if err != nil {
			logger.Logkv(
				"event", eventConnectionClosed,
				"message", "Downstream connection closed during preamble",
			)
			running = false
		}
	}

	// start reading packets
	for running {
		select {
		case packet, ok := <-conn.Queue:
			if ok {
				// packet received, log
				//log.Printf("Sending packet (length %d):\n%s\n", len(packet), hex.Dump(packet))
				// send the packet out
				_, err := conn.writer.Write(packet)
				// NOTE we shouldn't flush here, to avoid swamping the kernel with syscalls.
				// see https://golang.org/pkg/net/http/?m=all#response.Write for details
				// on how Go buffers HTTP responses (hint: a 2KiB bufio and a 4KiB bufio)
				if err != nil {
					logger.Logkv(
						"event", eventConnectionClosed,
						"message", "Downstream connection closed",
					)
					running = false
				}
				//log.Printf("Wrote packet of %d bytes\n", bytes)
			} else {
				// channel closed, exit
				logger.Logkv(
					"event", eventConnectionShutdown,
					"message", "Shutting down client connection",
				)
				running = false
				// Closed has already been set by closeQueue(), which is the
				// only place that closes the queue.
			}
		case <-conn.context.Done():
			// connection closed while we were waiting for more data
			logger.Logkv(
				"event", eventConnectionClosedWait,
				"message", "Downstream connection closed (while waiting)",
				"error", fmt.Sprintf("%v", conn.context.Err()),
			)
			running = false
		}
	}

	// we cannot drain the channel here, as it might not be closed yet.
	// better let our caller handle closure and draining.

	logger.Logkv(
		"event", eventConnectionDone,
		"message", "Streaming finished",
	)
}

// ServeStreamError returns an appropriate error response to the client.
func ServeStreamError(writer http.ResponseWriter, status int) {
	// set the content type (important)
	writer.Header().Set("Content-Type", "video/mpeg")
	// a stream is always current
	writer.Header().Set("Last-Modified", time.Now().UTC().Format(http.TimeFormat))
	// other headers to comply with the specs
	writer.Header().Set("Accept-Range", "none")
	// suppress caching by intermediate proxies
	writer.Header().Set("Cache-Control", "no-cache,no-store,no-transform")
	// ...and the application-supplied status code
	writer.WriteHeader(http.StatusNotFound)
}
