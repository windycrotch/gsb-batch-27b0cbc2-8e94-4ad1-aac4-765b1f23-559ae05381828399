// Copyright 2024 Jelly Terra
// This Source Code Form is subject to the terms of the Mozilla Public License, v. 2.0
// that can be found in the LICENSE file and https://mozilla.org/MPL/2.0/.

package main

import (
	"bytes"
	"fmt"
	"github.com/yutopp/go-rtmp"
	rtmpmsg "github.com/yutopp/go-rtmp/message"
	"io"
	"net/url"
	"sync"
	"time"
)

type Handler struct {
	rtmp.DefaultHandler

	ConnMsg *rtmpmsg.NetConnectionConnect
	PubMsg  *rtmpmsg.NetStreamPublish
	Url     *url.URL

	Time int64

	HandleFunc func(h *Handler) error

	HooksOnClose []func()

	// mu guards endpoints and closed. Every fan-out write pass holds the
	// read lock, while Close and failed-endpoint removal take the write
	// lock, so a close never mutates or frees an endpoint mid-write.
	mu        sync.RWMutex
	closed    bool
	endpoints []Endpoint

	// closeOnce makes sure every endpoint is closed and the afterClose hooks
	// run at most once, no matter how often Close/OnClose is triggered.
	closeOnce sync.Once
}

func (h *Handler) Logln(a ...interface{}) {
	fmt.Println(append([]any{h.Time}, a...)...)
}

// AddEndpoint registers a newly established endpoint. The Handler takes
// ownership of it. If the Handler is already closed (for example because
// startup failed after other endpoints were established), the endpoint is
// closed immediately instead of being leaked as a dangling connection or
// half-written file.
func (h *Handler) AddEndpoint(ep Endpoint) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.closed {
		ep.Close()
		return fmt.Errorf("handler is already closed, reject new endpoint")
	}

	h.endpoints = append(h.endpoints, ep)
	return nil
}

// dropEndpoint closes a failed endpoint and removes it so that no subsequent
// write reaches it. The ownership transfer happens under the write lock; the
// actual Close runs outside the lock. A concurrent drop or Close can
// therefore never close the same endpoint twice: exactly one of them finds
// the endpoint in the slice and becomes its sole closer.
func (h *Handler) dropEndpoint(failed Endpoint) {
	h.mu.Lock()
	for i, ep := range h.endpoints {
		if ep == failed {
			h.endpoints = append(h.endpoints[:i], h.endpoints[i+1:]...)
			h.mu.Unlock()

			ep.Close()
			return
		}
	}
	h.mu.Unlock()
}

// fanout writes one message to every active endpoint. A failure on one
// endpoint only isolates that endpoint: it is logged, closed and excluded
// from future writes, while every other endpoint keeps receiving the stream
// and the inbound stream itself is never aborted.
//
// The read lock is held for the whole write pass: Close and dropEndpoint
// both need the write lock, so an endpoint can never be closed while a write
// to it is still in flight, and once Close has marked the Handler closed no
// new write can start. Failed endpoints are collected under the read lock
// and closed only after it is released, so a slow Close never blocks the
// remaining endpoints' writes for long.
func (h *Handler) fanout(write func(ep Endpoint) error) {
	h.mu.RLock()
	if h.closed {
		h.mu.RUnlock()
		return
	}

	var failed []Endpoint
	for i, ep := range h.endpoints {
		if err := write(ep); err != nil {
			h.Logln("Endpoint", i, fmt.Sprintf("(%T)", ep), "write failed:", err,
				"- closing and excluding it from future writes")
			failed = append(failed, ep)
		}
	}
	h.mu.RUnlock()

	for _, ep := range failed {
		h.dropEndpoint(ep)
	}
}

// Close shuts down every established endpoint exactly once and runs the
// afterClose hooks at most once. It is safe to call repeatedly and from
// multiple goroutines.
func (h *Handler) Close() {
	h.closeOnce.Do(h.closeAll)
}

func (h *Handler) closeAll() {
	h.mu.Lock()
	eps := h.endpoints
	h.endpoints = nil
	h.closed = true
	h.mu.Unlock()

	for _, ep := range eps {
		ep.Close()
	}

	h.Logln("Closed.")
	for _, hook := range h.HooksOnClose {
		hook()
	}
}

func (h *Handler) OnConnect(timestamp uint32, cmd *rtmpmsg.NetConnectionConnect) (err error) {
	h.ConnMsg = cmd
	h.Url, err = url.Parse(cmd.Command.TCURL)
	if err != nil {
		return err
	}

	return nil
}

func (h *Handler) OnPublish(_ *rtmp.StreamContext, _ uint32, cmd *rtmpmsg.NetStreamPublish) error {
	h.PubMsg = cmd
	h.Time = time.Now().UnixMicro()

	err := h.HandleFunc(h)
	if err != nil {
		h.Logln(err)
		return err
	}

	return err
}

func (h *Handler) OnAudio(timestamp uint32, payload io.Reader) error {
	p, err := io.ReadAll(payload)
	if err != nil {
		return err
	}

	h.fanout(func(ep Endpoint) error {
		return ep.WriteAudio(timestamp, p)
	})

	return nil
}

func (h *Handler) OnVideo(timestamp uint32, payload io.Reader) error {
	p, err := io.ReadAll(payload)
	if err != nil {
		return err
	}

	h.fanout(func(ep Endpoint) error {
		return ep.WriteVideo(timestamp, p)
	})

	return nil
}

func (h *Handler) OnSetDataFrame(timestamp uint32, data *rtmpmsg.NetStreamSetDataFrame) error {
	h.fanout(func(ep Endpoint) error {
		// A fresh reader per endpoint: writing consumes the body.
		return ep.WriteSetFrame(timestamp, &rtmpmsg.DataMessage{
			Name:     "@setDataFrame",
			Encoding: rtmpmsg.EncodingTypeAMF0,
			Body:     bytes.NewReader(data.Payload),
		})
	})

	return nil
}

func (h *Handler) OnClose() {
	h.Close()
}
