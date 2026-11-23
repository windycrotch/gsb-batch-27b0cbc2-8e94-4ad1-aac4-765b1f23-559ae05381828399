/*
Copyright 2022-2025 The nagare media authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package http

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/valyala/fasthttp"
	"sigs.k8s.io/controller-runtime/pkg/log"

	enginev1 "github.com/nagare-media/engine/api/v1alpha1"
	engineio "github.com/nagare-media/engine/internal/functions/io"
	"github.com/nagare-media/engine/internal/pkg/backoff"
	nbmpconvv2 "github.com/nagare-media/engine/internal/pkg/nbmpconv/v2"
	"github.com/nagare-media/engine/pkg/http"
	"github.com/nagare-media/engine/pkg/starter"
	nbmpv2 "github.com/nagare-media/models.go/iso/nbmp/v2"
)

const (
	// CloseTimeout is the default inactivity timeout after which a port is
	// closed automatically.
	CloseTimeout = 1 * time.Minute

	// MaxRetries is the default maximum number of attempts (including the
	// initial one) for client-side requests before giving up and failing the
	// port.
	MaxRetries = 5
)

// ErrPortClosed is returned when an operation is attempted on a port that has
// already been closed. A closed port is never revived.
var ErrPortClosed = errors.New("http: port closed")

type port struct {
	mtx sync.Mutex

	pt     engineio.PortType
	d      enginev1.MediaDirection
	name   string
	number uint16
	url    *url.URL

	client *fasthttp.Client
	con    engineio.Connection
	pr     *io.PipeReader
	pw     *io.PipeWriter

	// closeTimeout is the inactivity timeout after which the port is closed
	// automatically.
	closeTimeout time.Duration
	// maxTries is the maximum number of attempts (including the initial one)
	// for client-side requests.
	maxTries uint

	closeTimer *time.Timer
	closed     bool
}

var (
	_ engineio.Port       = &port{}
	_ engineio.InputPort  = &port{}
	_ engineio.OutputPort = &port{}
)

func (p *port) Start(ctx context.Context) error {
	// Release all port resources (pipe, close timer, underlying connections)
	// when the function terminates. Close is idempotent, so this is safe even
	// if the port was already closed before.
	stop := context.AfterFunc(ctx, func() { _ = p.Close() })
	defer stop()

	switch {
	case p.pt == engineio.InputPortType && p.d == enginev1.PushMediaDirection:
		//                  input (push)
		// ┌────────────┐           ┌───┐          ┌─────┐
		// │ handlePost │──Server──▶│ p │──Start──▶│ con │
		// └────────────┘           └───┘          └─────┘
		defer p.con.Close()
		_, err := p.con.ReadFrom(p.pr)
		if err != nil {
			return err
		}

	case p.pt == engineio.InputPortType && p.d == enginev1.PullMediaDirection:
		//                  input (pull)
		// ┌────────────┐          ┌───┐          ┌─────┐
		// │ getRequest │──Start──▶│ p │──Start──▶│ con │
		// └────────────┘          └───┘          └─────┘
		defer p.con.Close()
		send := starter.Func(func(ctx context.Context) error {
			_, err := p.con.ReadFrom(p.pr)
			return err
		})
		get := starter.Func(func(ctx context.Context) error {
			err := p.getRequest(ctx)
			if err != nil {
				// Unblock the send side so the function can terminate
				// with this (diagnosable) error.
				_ = p.Close()
			}
			return err
		})
		return starter.Manage(send, get).WaitForAllToTerminate().Start(ctx)

	case p.pt == engineio.OutputPortType && p.d == enginev1.PushMediaDirection:
		//                  output (push)
		// ┌─────┐           ┌───┐          ┌─────────────┐
		// │ str │──Stream──▶│ p │──Start──▶│ postRequest │
		// └─────┘           └───┘          └─────────────┘
		return p.postRequest(ctx)

	case p.pt == engineio.OutputPortType && p.d == enginev1.PullMediaDirection:
		//                  output (pull)
		// ┌─────┐           ┌───┐           ┌───────────┐
		// │ str │──Stream──▶│ p │──Server──▶│ handleGet │
		// └─────┘           └───┘           └───────────┘
		// nothing to do here
	}

	return nil
}

func (p *port) Connect(c engineio.Connection) {
	p.con = c
}

func (p *port) Write(b []byte) (n int, err error) {
	return p.pw.Write(b)
}

func (p *port) ReadFrom(r io.Reader) (n int64, err error) {
	n, err = io.Copy(p.pw, r)
	// we don't automatically close after EOF (i.e. err == nil)
	return
}

func (p *port) Read(b []byte) (int, error) {
	return p.pr.Read(b)
}

// Close closes the port. It stops a potentially scheduled close timer, closes
// the underlying pipe and releases idle client connections. Close is
// idempotent: calling it multiple times neither panics nor leaks resources. A
// closed port is never revived; subsequent requests are rejected.
func (p *port) Close() error {
	p.mtx.Lock()
	defer p.mtx.Unlock()
	p.closeLocked()
	return nil
}

// closeLocked implements the actual close logic. It assumes p.mtx is locked.
func (p *port) closeLocked() {
	if p.closed {
		return
	}
	p.closed = true
	if p.closeTimer != nil {
		p.closeTimer.Stop()
		p.closeTimer = nil
	}
	_ = p.pw.Close()
	if p.client != nil {
		p.client.CloseIdleConnections()
	}
}

func (p *port) isClosed() bool {
	p.mtx.Lock()
	defer p.mtx.Unlock()
	return p.closed
}

func (p *port) Name() string {
	return p.name
}

func (p *port) BaseName() string {
	return engineio.PortCommonBaseName(p.Name())
}

func (p *port) PortNumber() uint16 {
	return p.number
}

func (p *port) Protocol() string {
	return Protocol
}

func (p *port) Type() engineio.PortType {
	return p.pt
}

func (p *port) Direction() enginev1.MediaDirection {
	return p.d
}

func (p *port) MountTo(srv engineio.Server) error {
	s, ok := srv.(Server)
	if !ok {
		return engineio.ServerIncompatibleWithPort
	}

	switch {
	case p.pt == engineio.InputPortType && p.d == enginev1.PushMediaDirection:
		s.Router().Post(p.url.EscapedPath(), p.handlePost)

	case p.pt == engineio.OutputPortType && p.d == enginev1.PullMediaDirection:
		s.Router().Get(p.url.EscapedPath(), p.handleGet)
	}

	return nil
}

func (p *port) getRequest(ctx context.Context) error {
	// input (pull)

	l := log.FromContext(ctx, "port", p.Name())
	ctx = log.IntoContext(ctx, l)

	op := func() error {
		if err := ctx.Err(); err != nil {
			return backoff.Permanent(err)
		}

		// A closed port must not be revived by a retry; fail permanently
		// instead.
		if p.isClosed() {
			return backoff.Permanent(fmt.Errorf("http: input (pull) port %s: %w", p.Name(), ErrPortClosed))
		}

		req := fasthttp.AcquireRequest()
		defer fasthttp.ReleaseRequest(req)
		req.Header.SetMethod(fasthttp.MethodGet)
		req.SetRequestURI(p.url.String())
		req.SetConnectionClose()

		resp := fasthttp.AcquireResponse()
		resp.StreamBody = true
		defer fasthttp.ReleaseResponse(resp)

		err := p.client.Do(req, resp)
		if err != nil && !errors.Is(err, fasthttp.ErrConnectionClosed) {
			return err
		}

		if resp.StatusCode() != fasthttp.StatusOK {
			return fmt.Errorf("event: unexpected HTTP status code in response: %d", resp.StatusCode())
		}

		// At this point we will at least try to read from the response once.
		// We will therefore abort a scheduled close of this port. If the close
		// already took effect in the meantime, we must not revive the port and
		// fail permanently instead.
		if !p.abortCloseTimer(ctx) {
			return backoff.Permanent(fmt.Errorf("http: input (pull) port %s: %w", p.Name(), ErrPortClosed))
		}

		n, err := p.ReadFrom(resp.BodyStream())
		// TODO: deal with "n > 0"
		_ = n

		// Restart the inactivity timer. A successful retry will abort it
		// again; if it fires before that, the port is closed for good.
		p.initCloseTimer(ctx)

		if err != nil {
			return err
		}

		return nil
	}

	no := func(err error, t time.Duration) {
		l.Error(err, fmt.Sprintf("failed; retrying after %s", t))
	}

	return backoff.RetryFunc(ctx, op, backoff.WithNotify(no), backoff.WithMaxTries(p.maxTries))
}

func (p *port) postRequest(ctx context.Context) error {
	// output (push)

	l := log.FromContext(ctx, "port", p.Name())
	ctx = log.IntoContext(ctx, l)

	op := func() error {
		if err := ctx.Err(); err != nil {
			return backoff.Permanent(err)
		}

		// A closed port must not be revived by a retry; fail permanently
		// instead.
		if p.isClosed() {
			return backoff.Permanent(fmt.Errorf("http: output (push) port %s: %w", p.Name(), ErrPortClosed))
		}

		req := fasthttp.AcquireRequest()
		defer fasthttp.ReleaseRequest(req)
		req.Header.SetMethod(fasthttp.MethodPost)
		req.SetBodyStream(io.NopCloser(p), -1)
		req.SetRequestURI(p.url.String())
		req.SetConnectionClose()

		resp := fasthttp.AcquireResponse()
		defer func() { _ = resp.CloseBodyStream() }()
		defer fasthttp.ReleaseResponse(resp)

		err := p.client.Do(req, resp)
		if err != nil && !errors.Is(err, fasthttp.ErrConnectionClosed) {
			return err
		}

		if resp.StatusCode() > 299 {
			return fmt.Errorf("event: unexpected HTTP status code in response: %d", resp.StatusCode())
		}

		return nil
	}

	no := func(err error, t time.Duration) {
		l.Error(err, fmt.Sprintf("failed; retrying after %s", t))
	}

	return backoff.RetryFunc(ctx, op, backoff.WithNotify(no), backoff.WithMaxTries(p.maxTries))
}

func (p *port) handleGet(c *fiber.Ctx) error {
	// output (pull)

	ctx := http.ContextFromFiberCtx(c)
	ctx = log.IntoContext(ctx, log.FromContext(ctx, "port", p.Name()))

	// A new request aborts a scheduled close. If the close already took
	// effect, the request is rejected: a closed port is never revived.
	if !p.abortCloseTimer(ctx) {
		return fiber.ErrGone
	}

	// TODO: allow other content-types
	c.Response().Header.SetContentType("video/mp2t")
	c.Response().Header.Set(fiber.HeaderXContentTypeOptions, "nosniff")

	// flush headers for chunked transfer encoding
	c.Response().ImmediateHeaderFlush = true

	// [Close] is called by fiber. Once the stream terminates (e.g. because
	// the client disconnected), the close timer is started: if no new request
	// arrives before the timeout, the port is closed.
	c.Response().SetBodyStream(&closeNotifyingReadCloser{
		Reader: p,
		done:   func() { p.initCloseTimer(ctx) },
	}, -1)

	c.Status(fiber.StatusOK)
	return nil
}

func (p *port) handlePost(c *fiber.Ctx) error {
	// input (push)

	// Only one request at a time: two requests must never write to the same
	// pipe concurrently.
	p.mtx.Lock()
	defer p.mtx.Unlock()

	ctx := http.ContextFromFiberCtx(c)
	ctx = log.IntoContext(ctx, log.FromContext(ctx, "port", p.Name()))

	// A new request aborts a scheduled close. If the close already took
	// effect, the request is rejected: a closed port is never revived.
	if !p.abortCloseTimerLocked(ctx) {
		return fiber.ErrGone
	}

	var err error
	if c.Request().IsBodyStream() {
		defer func() {
			_ = c.Request().CloseBodyStream()
		}()
		_, err = p.ReadFrom(c.Request().BodyStream())
	} else {
		_, err = p.Write(c.Body())
	}

	if err != nil {
		return fiber.ErrInternalServerError
	}

	// start timer to close port
	p.initCloseTimerLocked(ctx)

	c.Status(fiber.StatusCreated)
	return nil
}

// abortCloseTimer aborts a scheduled close of the port. It returns false if
// the port is already closed.
func (p *port) abortCloseTimer(ctx context.Context) bool {
	p.mtx.Lock()
	defer p.mtx.Unlock()
	return p.abortCloseTimerLocked(ctx)
}

// abortCloseTimerLocked implements the actual abort logic. It assumes p.mtx
// is locked.
func (p *port) abortCloseTimerLocked(ctx context.Context) bool {
	l := log.FromContext(ctx)
	if p.closed {
		return false
	}
	if p.closeTimer == nil {
		return true
	}
	l.Info("aborting timer to close port")
	p.closeTimer.Stop()
	p.closeTimer = nil
	return true
}

// initCloseTimer (re)starts the inactivity timer after which the port is
// closed. It is a no-op if the port is already closed.
func (p *port) initCloseTimer(ctx context.Context) {
	p.mtx.Lock()
	defer p.mtx.Unlock()
	p.initCloseTimerLocked(ctx)
}

// initCloseTimerLocked implements the actual timer logic. It assumes p.mtx is
// locked.
func (p *port) initCloseTimerLocked(ctx context.Context) {
	if p.closed {
		return
	}

	l := log.FromContext(ctx)
	l.Info("stopped receiving data")
	l.Info("starting timer to close port")
	if p.closeTimer != nil {
		p.closeTimer.Stop()
	}
	p.closeTimer = time.AfterFunc(p.closeTimeout, func() {
		p.mtx.Lock()
		defer p.mtx.Unlock()
		if p.closed {
			return
		}
		l.Info("close timeout reached; closing port")
		p.closeLocked()
	})
}

// closeNotifyingReadCloser wraps an io.Reader and invokes done exactly once
// when the stream is exhausted or closed.
type closeNotifyingReadCloser struct {
	io.Reader
	done func()
	once sync.Once
}

func (r *closeNotifyingReadCloser) Read(b []byte) (int, error) {
	n, err := r.Reader.Read(b)
	if err == io.EOF {
		r.once.Do(r.done)
	}
	return n, err
}

func (r *closeNotifyingReadCloser) Close() error {
	r.once.Do(r.done)
	return nil
}

func NewInputPortFor(p nbmpv2.Port, mp nbmpv2.MediaOrMetadataParameter) (engineio.InputPort, error) {
	in, err := newPortFor(p, mp)
	if err != nil {
		return nil, err
	}
	in.pt = engineio.InputPortType
	return in, nil
}

func NewOutputPortFor(p nbmpv2.Port, mp nbmpv2.MediaOrMetadataParameter) (engineio.OutputPort, error) {
	out, err := newPortFor(p, mp)
	if err != nil {
		return nil, err
	}
	out.pt = engineio.OutputPortType
	return out, nil
}

func newPortFor(p nbmpv2.Port, mp nbmpv2.MediaOrMetadataParameter) (*port, error) {
	// protocol
	if Protocol != mp.GetProtocol() && ProtocolTLS != mp.GetProtocol() {
		return nil, engineio.ProtocolIncompatibleWithPort
	}

	// direction
	if mp.GetMode() == nil {
		return nil, engineio.PortDirectionUnknown
	}
	d := nbmpconvv2.MediaAccessModeToMediaDirection(*mp.GetMode())
	if d == "" {
		return nil, engineio.PortDirectionUnknown
	}

	// URL
	if mp.GetCachingServerURL() == nil {
		return nil, engineio.PortURLUnknown
	}
	u, err := mp.GetCachingServerURL().URL()
	if err != nil {
		return nil, err
	}

	// port number
	// TODO: port number should not be derived from caching-server-url (= Service port) but from function configuration (i.e. the Job template)
	pn := DefaultPortNumber
	pnstr := u.Port()
	if pnstr != "" {
		pnp, err := strconv.ParseUint(pnstr, 10, 16)
		if err != nil {
			return nil, err
		}
		pn = uint16(pnp)
	}

	pr, pw := engineio.Pipe()
	port := &port{
		name:         p.PortName,
		d:            d,
		url:          u,
		number:       pn,
		client:       &fasthttp.Client{},
		pr:           pr,
		pw:           pw,
		closeTimeout: CloseTimeout,
		maxTries:     MaxRetries,
	}

	return port, nil
}

func init() {
	engineio.InputPortBuilders.Register(Protocol, NewInputPortFor)
	engineio.OutputPortBuilders.Register(Protocol, NewOutputPortFor)
	engineio.InputPortBuilders.Register(ProtocolTLS, NewInputPortFor)
	engineio.OutputPortBuilders.Register(ProtocolTLS, NewOutputPortFor)
}
