// RTMP server

package main

import (
	"crypto/subtle"
	"crypto/tls"
	"errors"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	tls_certificate_loader "github.com/AgustinSRG/go-tls-certificate-loader"
)

// Stores status data for a specific streaming channel
type RTMPChannel struct {
	channel string // The channel ID
	key     string // The channel key

	// True when the key field holds the key of a publisher (active or the most
	// recent one). While true, joining players must present a matching key,
	// even if the channel is temporarily idle between two publishing sessions.
	// Reset to false only when the channel record is deleted, so an idle
	// channel created solely by waiting players does not enforce a stale key.
	hasPublisherKey bool

	is_publishing bool   // True if there is an stream being published
	publisher     uint64 // The ID of the session that is publishing

	stream_id string // The current stream ID

	players map[uint64]bool // Players receiving the stream or waiting for it
}

// RTMP server
type RTMPServer struct {
	host string // Hostname
	port int    // Port

	listener       net.Listener // TCP listener
	secureListener net.Listener // TCP + SSL listener

	websocketControlConnection *ControlServerConnection // Connection to the coordinator server

	mutex *sync.Mutex // Mutex to access the status data (sessions, channels)

	sessions map[uint64]*RTMPSession // Active sessions
	channels map[string]*RTMPChannel // Active streaming channels

	streamIdMaxLength int // Max length for stream IDs, rooms and keys

	ipLimit uint32            // Max number of active sessions
	ipCount map[string]uint32 // Mapping IP -> Number of active sessions

	ip_mutex *sync.Mutex // Mutex for the IP count mapping

	next_session_id  uint64      // ID for the next incoming session
	session_id_mutex *sync.Mutex // Mutex to ensure session IDs are unique

	gopCacheLimit int64 // Limit of the GOP cache (in bytes)

	closed bool // True if the server is closed
}

const STREAM_ID_DEFAULT_MAX_LENGTH = 128
const GOP_CACHE_DEFAULT_LIMIT = 256 * 1024 * 1024
const IP_DEFAULT_LIMIT = 4

// Creates a RTMP server using the configuration from the environment variables
func CreateRTMPServer() *RTMPServer {
	server := RTMPServer{
		host:                       os.Getenv("RTMP_HOST"),
		listener:                   nil,
		secureListener:             nil,
		mutex:                      &sync.Mutex{},
		session_id_mutex:           &sync.Mutex{},
		ip_mutex:                   &sync.Mutex{},
		sessions:                   make(map[uint64]*RTMPSession),
		channels:                   make(map[string]*RTMPChannel),
		next_session_id:            1,
		closed:                     false,
		ipCount:                    make(map[string]uint32),
		ipLimit:                    IP_DEFAULT_LIMIT,
		gopCacheLimit:              GOP_CACHE_DEFAULT_LIMIT,
		websocketControlConnection: nil,
		streamIdMaxLength:          STREAM_ID_DEFAULT_MAX_LENGTH,
	}

	custom_ip_limit := os.Getenv("MAX_IP_CONCURRENT_CONNECTIONS")
	if custom_ip_limit != "" {
		cil, e := strconv.Atoi(custom_ip_limit)
		if e != nil {
			server.ipLimit = uint32(cil)
		}
	}

	custom_gop_limit := os.Getenv("GOP_CACHE_SIZE_MB")
	if custom_gop_limit != "" {
		cgl, e := strconv.Atoi(custom_gop_limit)
		if e != nil {
			server.gopCacheLimit = int64(cgl) * 1024 * 1024
		}
	}

	bind_addr := os.Getenv("BIND_ADDRESS")

	// Setup RTMP server
	var tcp_port int
	tcp_port = 1935
	customTCPPort := os.Getenv("RTMP_PORT")
	if customTCPPort != "" {
		tcpp, e := strconv.Atoi(customTCPPort)
		if e == nil {
			tcp_port = tcpp
		}
	}
	server.port = tcp_port

	lTCP, errTCP := net.Listen("tcp", bind_addr+":"+strconv.Itoa(tcp_port))
	if errTCP != nil {
		LogError(errTCP)
		return nil
	} else {
		server.listener = lTCP
		LogInfo("[RTMP] Listing on " + bind_addr + ":" + strconv.Itoa(tcp_port))
	}

	// Setup RTMPS server
	var ssl_port int
	ssl_port = 443
	customSSLPort := os.Getenv("SSL_PORT")
	if customSSLPort != "" {
		sslp, e := strconv.Atoi(customSSLPort)
		if e == nil {
			ssl_port = sslp
		}
	}

	certFile := os.Getenv("SSL_CERT")
	keyFile := os.Getenv("SSL_KEY")

	if certFile != "" && keyFile != "" {
		checkReloadSeconds := 60

		customCheckReloadSeconds := os.Getenv("SSL_CHECK_RELOAD_SECONDS")
		if customCheckReloadSeconds != "" {
			n, e := strconv.Atoi(customCheckReloadSeconds)
			if e == nil {
				checkReloadSeconds = n

				if checkReloadSeconds < 1 {
					checkReloadSeconds = 1
				}
			}
		}

		cerLoader, err := tls_certificate_loader.NewTlsCertificateLoader(tls_certificate_loader.TlsCertificateLoaderConfig{
			CertificatePath:   certFile,
			KeyPath:           keyFile,
			CheckReloadPeriod: time.Duration(checkReloadSeconds) * time.Second,
			OnReload: func() {
				LogInfo("Reloaded SSL certificates")
			},
			OnError: func(err error) {
				LogError(err)
			},
		})

		if err != nil {
			LogError(err)
			if server.listener != nil {
				server.listener.Close()
			}
			return nil
		}

		config := &tls.Config{
			GetCertificate: cerLoader.GetCertificate,
		}

		lnSSL, errSSL := tls.Listen("tcp", bind_addr+":"+strconv.Itoa(ssl_port), config)

		if errSSL != nil {
			cerLoader.Close()
			LogError(errSSL)
			return nil
		} else {
			server.secureListener = lnSSL
			LogInfo("[SSL] Listening on " + bind_addr + ":" + strconv.Itoa(ssl_port))
		}
	}

	idCustomMaxLength := os.Getenv("ID_MAX_LENGTH")

	if idCustomMaxLength != "" {
		var e error
		idMaxLen, e := strconv.Atoi(idCustomMaxLength)
		if e == nil && idMaxLen > 0 {
			server.streamIdMaxLength = idMaxLen
		}
	}

	if os.Getenv("CONTROL_USE") == "YES" {
		server.websocketControlConnection = &ControlServerConnection{}
	}

	return &server
}

// Adds an active session to the count for an IP address
// ip - The IP address
// Returns true if it was added, false if it reached the limit
func (server *RTMPServer) AddIP(ip string) bool {
	server.ip_mutex.Lock()
	defer server.ip_mutex.Unlock()

	c := server.ipCount[ip]

	if c >= server.ipLimit {
		return false
	}

	server.ipCount[ip] = c + 1

	return true
}

// Checks if an IP address if exempted from the IP limit
// ipStr - The IP address
// Returns true if exempted
func (server *RTMPServer) isIPExempted(ipStr string) bool {
	r := os.Getenv("CONCURRENT_LIMIT_WHITELIST")

	if r == "" {
		return false
	}

	if r == "*" {
		return true
	}

	ip := net.ParseIP(ipStr)

	parts := strings.Split(r, ",")

	for i := 0; i < len(parts); i++ {
		_, rang, e := net.ParseCIDR(parts[i])

		if e != nil {
			LogError(e)
			continue
		}

		if rang.Contains(ip) {
			return true
		}
	}

	return false
}

// Removes an active session from the count of an IP
// Call after the session is closed
// ip - The IP address
func (server *RTMPServer) RemoveIP(ip string) {
	server.ip_mutex.Lock()
	defer server.ip_mutex.Unlock()

	c := server.ipCount[ip]

	if c <= 1 {
		delete(server.ipCount, ip)
	} else {
		server.ipCount[ip] = c - 1
	}
}

// Generates an unique session ID
func (server *RTMPServer) NextSessionID() uint64 {
	server.session_id_mutex.Lock()
	defer server.session_id_mutex.Unlock()

	r := server.next_session_id
	server.next_session_id++
	return r
}

// Adds a session to the list
// s - The session
func (server *RTMPServer) AddSession(s *RTMPSession) {
	server.mutex.Lock()
	defer server.mutex.Unlock()

	server.sessions[s.id] = s
}

// Removes a session from the list
// id - The session ID
func (server *RTMPServer) RemoveSession(id uint64) {
	server.mutex.Lock()
	defer server.mutex.Unlock()

	delete(server.sessions, id)
}

// Checks if there is an active stream being published on a given channel
// channel - Channel ID
// Returns true if active publishing
func (server *RTMPServer) isPublishing(channel string) bool {
	server.mutex.Lock()
	defer server.mutex.Unlock()

	return server.channels[channel] != nil && server.channels[channel].is_publishing
}

// Obtains a reference to the session that is publishing on a given channel
// channel - The channel ID
// Returns the reference, or nil
func (server *RTMPServer) GetPublisher(channel string) *RTMPSession {
	server.mutex.Lock()
	defer server.mutex.Unlock()

	if server.channels[channel] == nil {
		return nil
	}

	if !server.channels[channel].is_publishing {
		return nil
	}

	id := server.channels[channel].publisher
	return server.sessions[id]
}

// Sets a publisher and a stream for a given channel
// channel - The channel ID
// key - The channel key
// stream_id - The stream ID
// s - The session that is publishing
// Returns true if success, false if there was another session publishing
func (server *RTMPServer) SetPublisher(channel string, key string, stream_id string, s *RTMPSession) bool {
	server.mutex.Lock()
	defer server.mutex.Unlock()

	if server.channels[channel] != nil && server.channels[channel].is_publishing {
		return false
	}

	if server.channels[channel] == nil {
		c := RTMPChannel{
			channel:         channel,
			key:             key,
			hasPublisherKey: true,
			stream_id:       stream_id,
			is_publishing:   true,
			publisher:       s.id,
			players:         make(map[uint64]bool),
		}
		server.channels[channel] = &c
	} else {
		server.channels[channel].key = key
		server.channels[channel].hasPublisherKey = true
		server.channels[channel].stream_id = stream_id
		server.channels[channel].is_publishing = true
		server.channels[channel].publisher = s.id
	}

	return true
}

// EndPublisher tears down the active publisher of a channel.
// Idempotent: a repeated invocation while no publisher is active is a no-op
// and returns an empty snapshot, so a duplicated end event can neither rewrite
// the channel state nor move the players back to idle a second time.
//
// All state changes (channel flags and each player's isIdling/isPlaying flags)
// happen atomically under server.mutex. It returns the snapshot of players
// that were actively playing, so the caller can send end notifications outside
// the lock without risking a deadlock against the connection mutex.
func (server *RTMPServer) EndPublisher(channel string) []*RTMPSession {
	server.mutex.Lock()
	defer server.mutex.Unlock()

	c := server.channels[channel]
	if c == nil || !c.is_publishing {
		return make([]*RTMPSession, 0)
	}

	activePlayers := make([]*RTMPSession, 0)

	// Every player returns to the waiting state. They stay registered on the
	// channel so the next publisher can pick them up (key permitting).
	for sid := range c.players {
		player := server.sessions[sid]
		if player == nil {
			continue
		}
		if player.isPlaying {
			activePlayers = append(activePlayers, player)
		}
		player.isIdling = true
		player.isPlaying = false
	}

	c.publisher = 0
	c.is_publishing = false

	// The authoritative key is retained (hasPublisherKey stays true) while
	// idle players remain, so wrong-key joiners are still rejected. Only when
	// nobody is left do we drop the whole channel record.
	if len(c.players) == 0 {
		delete(server.channels, channel)
	}

	return activePlayers
}

// Obtains the list of idle players for a given channel
// channel - The channel ID
// Returns the list of sessions waiting to play the stream
func (server *RTMPServer) GetIdlePlayers(channel string) []*RTMPSession {
	server.mutex.Lock()
	defer server.mutex.Unlock()

	if server.channels[channel] == nil {
		return make([]*RTMPSession, 0)
	}

	players := server.channels[channel].players

	playersToStart := make([]*RTMPSession, 0)

	for sid := range players {
		player := server.sessions[sid]
		if player != nil && player.isIdling {
			playersToStart = append(playersToStart, player)
		}
	}

	return playersToStart
}

// Obtains the list of players for a given channel
// channel - The channel ID
// Returns the list of sessions playing the stream
func (server *RTMPServer) GetPlayers(channel string) []*RTMPSession {
	server.mutex.Lock()
	defer server.mutex.Unlock()

	if server.channels[channel] == nil {
		return make([]*RTMPSession, 0)
	}

	players := server.channels[channel].players

	playersToStart := make([]*RTMPSession, 0)

	for sid := range players {
		player := server.sessions[sid]
		if player != nil && player.isPlaying {
			playersToStart = append(playersToStart, player)
		}
	}

	return playersToStart
}

// Marks a player session as receiving the stream.
// All isPlaying/isIdling transitions go through here under server.mutex so
// publishers (other goroutines) and the player's own session never race on
// these flags.
func (server *RTMPServer) setPlayerPlaying(s *RTMPSession) {
	server.mutex.Lock()
	defer server.mutex.Unlock()

	s.isPlaying = true
	s.isIdling = false
}

// Marks a player session as waiting for the stream.
func (server *RTMPServer) setPlayerIdle(s *RTMPSession) {
	server.mutex.Lock()
	defer server.mutex.Unlock()

	s.isPlaying = false
	s.isIdling = true
}

// Adds a player to a given channel
// channel - The channel ID
// key - The channel key used by the player
// s - The session
// Returns:
//
//	idling - True if the channel was not active, so the player becomes idle. False means the player can begin receiving the stream
//	err - Error. If not nil, it means the channel of the key are not valid
func (server *RTMPServer) AddPlayer(channel string, key string, s *RTMPSession) (bool, error) {
	server.mutex.Lock()
	defer server.mutex.Unlock()

	if server.channels[channel] == nil {
		c := RTMPChannel{
			channel:         channel,
			key:             key,
			hasPublisherKey: false, // Created by waiting players: no authoritative key yet
			stream_id:       "",
			is_publishing:   false,
			publisher:       0,
			players:         make(map[uint64]bool),
		}
		server.channels[channel] = &c
	}

	c := server.channels[channel]

	// A channel keeps the key of its (last) publisher while it is idle between
	// publishing sessions. A joining viewer must match it, otherwise it is
	// refused immediately instead of slipping into the waiting state.
	if c.hasPublisherKey && subtle.ConstantTimeCompare([]byte(key), []byte(c.key)) != 1 {
		return false, errors.New("invalid key")
	}

	if c.is_publishing {
		// Key was already validated above.
		s.isIdling = false
	} else {
		s.isIdling = true
	}

	c.players[s.id] = true

	return s.isIdling, nil
}

// Removes a player from a channel
// channel - The channel ID
// s - The session
func (server *RTMPServer) RemovePlayer(channel string, key string, s *RTMPSession) {
	server.mutex.Lock()
	defer server.mutex.Unlock()

	c := server.channels[channel]
	if c == nil {
		// The channel may already have been cleaned up after the publisher and
		// the rest of the players left. Just reset the local session state.
		s.isIdling = false
		s.isPlaying = false
		return
	}

	delete(c.players, s.id)

	s.isIdling = false
	s.isPlaying = false

	// Once nobody is publishing and nobody is waiting, the record must not
	// linger, otherwise the next publisher would be rejected as "already
	// publishing".
	if !c.is_publishing && len(c.players) == 0 {
		delete(server.channels, channel)
	}
}

// Runs a loop to indefinitely accept incoming connections
// listener - The TCP listener
// wg - The waiting group
func (server *RTMPServer) AcceptConnections(listener net.Listener, wg *sync.WaitGroup) {
	defer func() {
		listener.Close()
		wg.Done()
	}()
	for {
		c, err := listener.Accept()
		if err != nil {
			LogError(err)
			return
		}
		id := server.NextSessionID()
		var ip string
		if addr, ok := c.RemoteAddr().(*net.TCPAddr); ok {
			ip = addr.IP.String()
		} else {
			ip = c.RemoteAddr().String()
		}

		if !server.isIPExempted(ip) {
			if !server.AddIP(ip) {
				c.Close()
				LogRequest(id, ip, "Connection rejected: Too many requests")
				continue
			}
		}

		LogDebugSession(id, ip, "Connection accepted!")
		go server.HandleConnection(id, ip, c)
	}
}

// Sends pings to active sessions
// Runs a loop indefinitely. Call in a separate routine.
// wg - The waiting group
func (server *RTMPServer) SendPings(wg *sync.WaitGroup) {
	defer wg.Done()
	for !server.closed {
		// Wait
		time.Sleep(RTMP_PING_TIME * time.Millisecond)

		func() {
			server.mutex.Lock()
			defer server.mutex.Unlock()

			for _, s := range server.sessions {
				s.SendPingRequest()
			}
		}()
	}
}

// Starts the server
func (server *RTMPServer) Start() {
	// Initialize websocket connection
	if server.websocketControlConnection != nil {
		server.websocketControlConnection.Initialize(server)
	}

	// Start RTMP server
	var wg sync.WaitGroup
	if server.listener != nil {
		wg.Add(1)
		go server.AcceptConnections(server.listener, &wg)
	}

	if server.secureListener != nil {
		wg.Add(1)
		go server.AcceptConnections(server.secureListener, &wg)
	}

	wg.Add(1)
	go server.SendPings(&wg)

	wg.Wait()
}

// Handles a connection
// id - Session ID
// ip - Client IP address
// c - The TCP connection
func (server *RTMPServer) HandleConnection(id uint64, ip string, c net.Conn) {
	s := CreateRTMPSession(server, id, ip, c)

	server.AddSession(&s)

	defer func() {
		if err := recover(); err != nil {
			switch x := err.(type) {
			case string:
				LogRequest(id, ip, "Error: "+x)
			case error:
				LogRequest(id, ip, "Error: "+x.Error())
			default:
				LogRequest(id, ip, "Connection Crashed!")
			}
		}
		s.OnClose()
		c.Close()
		server.RemoveSession(id)
		server.RemoveIP(ip)
		LogDebugSession(id, ip, "Connection closed!")
	}()

	s.HandleSession()
}

// Returns the server chunk size for outgoing packets
// Returns the chunk size in bytes
func (server *RTMPServer) getOutChunkSize() uint32 {
	r := os.Getenv("RTMP_CHUNK_SIZE")

	if r == "" {
		return RTMP_CHUNK_SIZE
	}

	n, e := strconv.Atoi(r)

	if e != nil || n <= RTMP_CHUNK_SIZE {
		return RTMP_CHUNK_SIZE
	}

	return uint32(n)
}

// Kills any sessions publishing streams
func (server *RTMPServer) KillAllActivePublishers() {
	activePublishers := make([]*RTMPSession, 0)

	server.mutex.Lock()

	for _, channel := range server.channels {
		if channel == nil || !channel.is_publishing {
			continue
		}

		session := server.sessions[channel.publisher]

		if session != nil {
			activePublishers = append(activePublishers, session)
		}
	}

	server.mutex.Unlock()

	for i := 0; i < len(activePublishers); i++ {
		activePublishers[i].Kill()
	}
}
