package srt

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"reflect"
	"sync"
	"testing"
	"time"

	gosrt "github.com/datarhei/gosrt"
	"github.com/voc/srtrelay/relay"
	"github.com/voc/srtrelay/stream"
)

func compareStats(got, expected []*relay.StreamStatistics) error {
	if len(got) != len(expected) {
		return fmt.Errorf("Wrong number of streams: got %d, expected %d", len(got), len(expected))
	}
	for i, v := range expected {
		if !reflect.DeepEqual(*v, *got[i]) {
			return fmt.Errorf("Invalid stream statistics: got %v, expected %v", *got[i], *v)
		}
	}
	return nil
}

func TestServerImpl_GetStatistics(t *testing.T) {
	r := relay.NewRelay(&relay.RelayConfig{
		BufferSize: 1,
		PacketSize: 1,
	})
	s := &Server{
		relay: r,
		config: &ServerConfig{
			Addresses: []netip.AddrPort{
				netip.MustParseAddrPort("127.0.0.1:1337"),
				netip.MustParseAddrPort("[::1]:1337"),
			}, PublicAddress: "testserver.de:1337",
		},
	}
	if _, err := r.Publish("s1"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.Subscribe("s1"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.Subscribe("s1"); err != nil {
		t.Fatal(err)
	}
	streams := s.GetStatistics()

	expected := []*relay.StreamStatistics{
		{Name: "s1", URL: "srt://testserver.de:1337?streamid=#!::m=request,r=s1", Clients: 2, Created: streams[0].Created}, // New Format
	}
	if err := compareStats(streams, expected); err != nil {
		t.Error(err)
	}
}

type testSocket struct {
	N  int
	ch chan []byte

	numWritten int
}

func (s *testSocket) Read(b []byte) (int, error) {
	buf, ok := <-s.ch
	if !ok {
		return 0, io.EOF
	}
	length := copy(b, buf)
	return length, nil
}

func (s *testSocket) Write(b []byte) (int, error) {
	s.numWritten++
	return len(b), nil
}

func (s *testSocket) Close() error { return nil }

func (s *testSocket) Stats(*gosrt.Statistics) {
}

func (s *testSocket) RemoteAddr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1234}
}

func TestPublish(t *testing.T) {
	s := NewServer(&Config{
		Server: ServerConfig{PacketSize: 1316},
		Relay:  relay.RelayConfig{BufferSize: 50, PacketSize: 1316},
	})

	rd := testSocket{ch: make(chan []byte)}
	wr := testSocket{}
	id, err := stream.NewStreamID("test", "", stream.ModePublish)
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	wg.Add(2)

	// pub routine
	go func() {
		defer wg.Done()
		err = s.publish(&srtConn{
			log:      slog.Default(),
			socket:   &rd,
			streamid: id,
		})
		if err != io.EOF {
			t.Error("publisher error", err)
		}
	}()

	// sub routine
	go func() {
		defer wg.Done()
		time.Sleep(50 * time.Millisecond)
		err := s.play(&srtConn{
			log:      slog.Default(),
			socket:   &wr,
			streamid: id,
		})
		if err != nil {
			t.Error("player error", err)
		}
	}()

	time.Sleep(100 * time.Millisecond)
	for i := 0; i < 100; i++ {
		rd.ch <- []byte{1, 2, 3, 4}
		time.Sleep(1 * time.Millisecond)
	}
	close(rd.ch)
	wg.Wait()
	if wr.numWritten != 100 {
		t.Errorf("Wrong number of packets written: got %d, expected %d", wr.numWritten, 100)
	}
}

// nullTSPacket is a valid MPEG-TS null packet which does not carry any
// decoder initialization data (no PAT/PMT, no SPS/PPS)
func nullTSPacket() []byte {
	data := make([]byte, 188)
	data[0] = 0x47 // sync byte
	data[1] = 0x1f // PID 0x1fff (null packet)
	data[2] = 0xff
	data[3] = 0x10 // payload only
	return data
}

func newSyncTestServer(syncTimeout time.Duration) *Server {
	return NewServer(&Config{
		Server: ServerConfig{
			PacketSize:  1316,
			SyncClients: true,
			SyncTimeout: syncTimeout,
		},
		Relay: relay.RelayConfig{BufferSize: 50 * 1316, PacketSize: 1316},
	})
}

func playStream(s *Server, name string, wr *testSocket) error {
	id, err := stream.NewStreamID(name, "", stream.ModePlay)
	if err != nil {
		return err
	}
	return s.play(&srtConn{
		log:      slog.Default(),
		socket:   wr,
		streamid: id,
	})
}

func streamClients(s *Server, name string) int {
	for _, st := range s.relay.GetStatistics() {
		if st.Name == name {
			return st.Clients
		}
	}
	return 0
}

func waitForClients(t *testing.T, s *Server, name string, expected int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if got := streamClients(s, name); got == expected {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("Timed out waiting for %d clients on stream %s, got %d", expected, name, streamClients(s, name))
}

// publishChunks sends data in packet-sized chunks to the named stream
func publishChunks(t *testing.T, s *Server, pub chan<- *relay.Buffer, data []byte) {
	t.Helper()
	for len(data) > 0 {
		n := min(len(data), 1316)
		buf := s.relay.AcquireBuffer()
		copy(buf.Bytes(), data[:n])
		buf.Resize(n)
		pub <- buf
		data = data[n:]
	}
}

// A waiting client that never receives initialization data must be
// disconnected with an observable error once the sync timeout expires
func TestPlaySyncTimeout(t *testing.T) {
	s := newSyncTestServer(100 * time.Millisecond)

	pub, err := s.relay.Publish("test")
	if err != nil {
		t.Fatal(err)
	}
	defer close(pub)

	wr := testSocket{}
	errCh := make(chan error, 1)
	go func() {
		errCh <- playStream(s, "test", &wr)
	}()

	waitForClients(t, s, "test", 1)

	// keep the client busy with packets that contain no init data,
	// paced slowly enough to not overflow the client buffer
	nullPacket := nullTSPacket()
	start := time.Now()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
loop:
	for {
		select {
		case err := <-errCh:
			if !errors.Is(err, relay.ErrSyncTimeout) {
				t.Fatalf("Expected sync timeout error, got %v", err)
			}
			break loop
		case <-ticker.C:
			publishChunks(t, s, pub, nullPacket)
		case <-time.After(10 * time.Second):
			t.Fatal("play did not return after sync timeout")
		}
	}

	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("Sync timeout took too long: %v", elapsed)
	}
	if wr.numWritten != 0 {
		t.Errorf("No data should be written before sync, got %d writes", wr.numWritten)
	}

	// subscription must be released after the timeout
	waitForClients(t, s, "test", 0)
}

// A waiting client must be disconnected promptly when the sync timeout
// expires even if the publisher stays completely silent
func TestPlaySyncTimeoutSilentPublisher(t *testing.T) {
	s := newSyncTestServer(100 * time.Millisecond)

	pub, err := s.relay.Publish("test")
	if err != nil {
		t.Fatal(err)
	}
	defer close(pub)

	wr := testSocket{}
	errCh := make(chan error, 1)
	go func() {
		errCh <- playStream(s, "test", &wr)
	}()

	waitForClients(t, s, "test", 1)

	select {
	case err := <-errCh:
		if !errors.Is(err, relay.ErrSyncTimeout) {
			t.Fatalf("Expected sync timeout error, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("play did not return after sync timeout")
	}

	waitForClients(t, s, "test", 0)
}

// A client receiving initialization data within the sync timeout
// starts playing normally
func TestPlaySyncSuccess(t *testing.T) {
	s := newSyncTestServer(5 * time.Second)

	pub, err := s.relay.Publish("test")
	if err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile("../mpegts/h264.ts")
	if err != nil {
		t.Fatal(err)
	}

	wr := testSocket{}
	errCh := make(chan error, 1)
	go func() {
		errCh <- playStream(s, "test", &wr)
	}()

	waitForClients(t, s, "test", 1)
	publishChunks(t, s, pub, data)
	// let the client play a bit, then close upstream
	time.Sleep(100 * time.Millisecond)
	close(pub)

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("play failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("play did not return after upstream close")
	}

	if wr.numWritten == 0 {
		t.Error("Expected data to be written after successful sync")
	}
}

// A client still waiting for initialization data must be released
// promptly when the publisher goes away
func TestPlaySyncUpstreamCloseWhileWaiting(t *testing.T) {
	s := newSyncTestServer(time.Minute) // long timeout, close must win

	pub, err := s.relay.Publish("test")
	if err != nil {
		t.Fatal(err)
	}

	wr := testSocket{}
	errCh := make(chan error, 1)
	go func() {
		errCh <- playStream(s, "test", &wr)
	}()

	waitForClients(t, s, "test", 1)

	// publisher goes away without ever sending init data
	close(pub)

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Expected clean disconnect on upstream close, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("play did not return promptly after upstream close")
	}

	if wr.numWritten != 0 {
		t.Errorf("No data should be written before sync, got %d writes", wr.numWritten)
	}
}

// With multiple waiting clients, one may time out while another syncs
// successfully; client counts must track the actual subscriptions
func TestPlaySyncMultipleClients(t *testing.T) {
	s := newSyncTestServer(500 * time.Millisecond)

	pub, err := s.relay.Publish("test")
	if err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile("../mpegts/h264.ts")
	if err != nil {
		t.Fatal(err)
	}

	// client A starts waiting at t=0, times out at t=500ms
	wrA := testSocket{}
	errChA := make(chan error, 1)
	go func() {
		errChA <- playStream(s, "test", &wrA)
	}()
	waitForClients(t, s, "test", 1)

	// client B starts waiting at t=250ms, times out at t=750ms
	time.Sleep(250 * time.Millisecond)
	wrB := testSocket{}
	errChB := make(chan error, 1)
	go func() {
		errChB <- playStream(s, "test", &wrB)
	}()
	waitForClients(t, s, "test", 2)

	// A times out while B is still waiting
	select {
	case err := <-errChA:
		if !errors.Is(err, relay.ErrSyncTimeout) {
			t.Fatalf("Expected sync timeout error for client A, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("client A did not time out")
	}
	waitForClients(t, s, "test", 1)

	// init data arrives at t≈600ms: too late for A, in time for B
	publishChunks(t, s, pub, data)
	time.Sleep(100 * time.Millisecond)
	close(pub)

	select {
	case err := <-errChB:
		if err != nil {
			t.Fatalf("client B should have started playing, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("client B did not return after upstream close")
	}

	if wrA.numWritten != 0 {
		t.Errorf("Client A should not have received data, got %d writes", wrA.numWritten)
	}
	if wrB.numWritten == 0 {
		t.Error("Client B should have received data after syncing")
	}

	// all subscriptions released, no count drift
	waitForClients(t, s, "test", 0)
}
