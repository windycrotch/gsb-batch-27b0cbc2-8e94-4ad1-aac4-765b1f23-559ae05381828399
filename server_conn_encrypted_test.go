package gortmplib_test

import (
	"net"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/bluenviron/gortmplib"
	"github.com/bluenviron/gortmplib/pkg/amf0"
	"github.com/bluenviron/gortmplib/pkg/bytecounter"
	"github.com/bluenviron/gortmplib/pkg/handshake"
	"github.com/bluenviron/gortmplib/pkg/message"
	"github.com/bluenviron/gortmplib/pkg/rc4rw"
)

func TestServerConnEncrypted(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	defer clientConn.Close() //nolint:errcheck

	done := make(chan struct{})

	go func() {
		defer close(done)
		defer serverConn.Close() //nolint:errcheck

		conn := &gortmplib.ServerConn{RW: serverConn}
		err := conn.Initialize()
		require.NoError(t, err)

		err = conn.AcceptConn()
		require.NoError(t, err)

		err = conn.AcceptAction()
		require.NoError(t, err)
	}()

	bc := bytecounter.NewReadWriter(clientConn)

	keyIn, keyOut, err := handshake.DoClient(bc, true, false)
	require.NoError(t, err)
	require.NotNil(t, keyIn)
	require.NotNil(t, keyOut)

	rw, err := rc4rw.New(bc, keyIn, keyOut)
	require.NoError(t, err)

	mrw := message.NewReadWriter(rw, bc, true)

	err = mrw.Write(&message.CommandAMF0{
		ChunkStreamID: 3,
		Name:          "connect",
		CommandID:     1,
		Arguments: []any{
			amf0.Object{
				{Key: "app", Value: "stream"},
				{Key: "flashVer", Value: "LNX 9,0,124,2"},
				{Key: "tcUrl", Value: "rtmp://127.0.0.1:9121/stream"},
				{Key: "fpad", Value: false},
				{Key: "capabilities", Value: float64(15)},
				{Key: "audioCodecs", Value: float64(4071)},
				{Key: "videoCodecs", Value: float64(252)},
				{Key: "videoFunction", Value: float64(1)},
			},
		},
	})
	require.NoError(t, err)

	// SetWindowAckSize, SetPeerBandwidth, SetChunkSize, _result:
	// these are written by the server through the RC4 writer, therefore
	// they also prove that the key direction is correct.
	for i := 0; i < 4; i++ {
		_, err = mrw.Read()
		require.NoError(t, err)
	}

	err = mrw.Write(&message.SetChunkSize{Value: 65536})
	require.NoError(t, err)

	err = mrw.Write(&message.CommandAMF0{
		ChunkStreamID: 3,
		Name:          "createStream",
		CommandID:     2,
		Arguments:     []any{nil},
	})
	require.NoError(t, err)

	_, err = mrw.Read() // _result of createStream
	require.NoError(t, err)

	err = mrw.Write(&message.CommandAMF0{
		ChunkStreamID:   4,
		MessageStreamID: 0x1000000,
		Name:            "play",
		CommandID:       0,
		Arguments:       []any{nil, ""},
	})
	require.NoError(t, err)

	// messages written by AcceptAction.
	for i := 0; i < 6; i++ {
		_, err = mrw.Read()
		require.NoError(t, err)
	}

	<-done
}
