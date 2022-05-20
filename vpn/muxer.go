package vpn

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net"
)

//
// OpenVPN Multiplexer
//

/*
 muxer is the VPN transport multiplexer.

 One important limitation of the implementation at this moment is that the
 processing of incoming packets needs to be driven by reads from the user of
 the library. This means that if you don't do reads during some time, any packets
 on the control channel that the server sends us (e.g., openvpn-pings) will not
 be processed (and so, not acknowledged) until triggered by a muxer.Read().

 From the original documentation:
 https://community.openvpn.net/openvpn/wiki/SecurityOverview

 "OpenVPN multiplexes the SSL/TLS session used for authentication and key
 exchange with the actual encrypted tunnel data stream. OpenVPN provides the
 SSL/TLS connection with a reliable transport layer (as it is designed to
 operate over). The actual IP packets, after being encrypted and signed with an
 HMAC, are tunnelled over UDP without any reliability layer. So if --proto udp
 is used, no IP packets are tunneled over a reliable transport, eliminating the
 problem of reliability-layer collisions -- Of course, if you are tunneling a
 TCP session over OpenVPN running in UDP mode, the TCP protocol itself will
 provide the reliability layer."

SSL/TLS -> Reliability Layer -> \
           --tls-auth HMAC       \
                                  \
                                   > Multiplexer ----> UDP/TCP
                                  /                    Transport
IP        Encrypt and HMAC       /
Tunnel -> using OpenSSL EVP --> /
Packets   interface.

"This model has the benefit that SSL/TLS sees a reliable transport layer while
the IP packet forwarder sees an unreliable transport layer -- exactly what both
components want to see. The reliability and authentication layers are
completely independent of one another, i.e. the sequence number is embedded
inside the HMAC-signed envelope and is not used for authentication purposes."
*/
type muxer struct {

	// A net.Conn that has access to the "wire" transport. this can
	// represent an UDP/TCP socket, or a net.Conn coming from a Pluggable
	// Transport etc.
	conn net.Conn

	// After completing the TLS handshake, we get a tls transport that implements
	// net.Conn. All the control packets from that moment on are read from
	// and written to the tls Conn.
	tls net.Conn

	// control and data are the handlers for the control and data channels.
	// they implement the methods needed for the handshake and handling of
	// packets.
	control controlHandler
	data    dataHandler

	// bufReader is used to buffer data channel reads. We only write to
	// this buffer when we have correctly decrypted an incoming
	bufReader *bytes.Buffer

	// Mutable state tied to a concrete session.
	session *session

	// Mutable state tied to a particular vpn run.
	tunnel *tunnel

	// Options are OpenVPN options that come from parsing a subset of the OpenVPN
	// configuration directives, plus some non-standard config directives.
	options *Options
}

var _ vpnMuxer = &muxer{} // Ensure that we implement the vpnMuxer interface.

//
// Interfaces we depend on (we could make muxer an interface too).
//

// controlHandler manages the control "channel".
type controlHandler interface {
	SendHardReset(net.Conn, *session)
	ParseHardReset([]byte) (sessionID, error)
	SendACK(net.Conn, *session, packetID) error
	PushRequest() []byte
	ReadPushResponse([]byte) string
	ControlMessage(*session, *Options) ([]byte, error)
	ReadControlMessage([]byte) (*keySource, string, error)
	// ...
}

// dataHandler manages the data "channel".
type dataHandler interface {
	SetupKeys(*dataChannelKey) error
	WritePacket(net.Conn, []byte) (int, error)
	ReadPacket(*packet) ([]byte, error)
	DecodeEncryptedPayload([]byte, *dataChannelState) (*encryptedData, error)
	EncryptAndEncodePayload([]byte, *dataChannelState) ([]byte, error)
}

// vpnMuxer contains all the behavior expected by the muxer.
type vpnMuxer interface {
	Handshake() error
	Reset() error
	InitDataWithRemoteKey() error
	Write([]byte) (int, error)
	Read([]byte) (int, error)
}

//
// muxer: initialization
//

// newMuxerFromOptions returns a configured muxer, and any error if the
// operation could not be completed.
func newMuxerFromOptions(conn net.Conn, options *Options, tunnel *tunnel) (*muxer, error) {
	control := &control{}
	session, err := newSession()
	if err != nil {
		return &muxer{}, err
	}
	data, err := newDataFromOptions(options, session)
	if err != nil {
		return &muxer{}, err
	}
	br := bytes.NewBuffer(nil)

	m := &muxer{
		conn:      conn,
		session:   session,
		options:   options,
		control:   control,
		data:      data,
		tunnel:    tunnel,
		bufReader: br,
	}
	return m, nil
}

//
// muxer: handshake
//

// Handshake performs the OpenVPN "handshake" operations serially. It returns
// any error that is raised at any of the underlying steps.
func (m *muxer) Handshake() error {

	// 1. control channel sends reset, parse response.

	if err := m.Reset(); err != nil {
		return err
	}

	// 2. TLS handshake.

	// XXX this step can now be moved before dial/reset; we can store the conf
	// in the muxer.
	tlsConf, err := initTLS(m.session, m.options)
	if err != nil {
		return err
	}
	tlsConn, err := NewTLSConn(m.conn, m.session)
	if err != nil {
		return fmt.Errorf("%w: %s", ErrBadTLSHandshake, err)
	}
	tls, err := tlsHandshake(tlsConn, tlsConf)
	if err != nil {
		return err
	}
	m.tls = tls
	logger.Info("TLS handshake done")

	// 3. data channel init (auth, push, data initialization).

	if err := m.InitDataWithRemoteKey(); err != nil {
		return err
	}

	logger.Info("VPN handshake done")
	return nil
}

// Reset sends a hard-reset packet to the server, and awaits the server
// confirmation.
func (m *muxer) Reset() error {
	m.control.SendHardReset(m.conn, m.session)
	resp, err := readPacket(m.conn)
	if err != nil {
		return err
	}

	remoteSessionID, err := m.control.ParseHardReset(resp)

	// here we could check if we have received a remote session id but
	// our session.remoteSessionID is != from all zeros
	if err != nil {
		return fmt.Errorf("%s: %w", ErrBadHandshake, err)
	}
	m.session.RemoteSessionID = remoteSessionID

	logger.Infof("Remote session ID: %x", m.session.RemoteSessionID)
	logger.Infof("Local session ID:  %x", m.session.LocalSessionID)

	// we assume id is 0, this is the first packet we ack.
	// XXX I could parse the real packet id from server instead. this
	// _might_ be important when re-keying?
	m.control.SendACK(m.conn, m.session, packetID(0))
	return nil
}

//
// muxer: read and handle packets
//

// handleIncoming packet reads the next packet available in the underlying
// socket. It returns true if the packet was a data packet; otherwise it will
// process it but return false.
func (m *muxer) handleIncomingPacket() bool {
	data, err := readPacket(m.conn)
	if err != nil {
		logger.Error(err.Error())
		return false
	}
	p, err := parsePacketFromBytes(data)
	if err != nil {
		logger.Error(err.Error())
		return false
	}
	if p.isACK() {
		logger.Warn("muxer: got ACK (ignored)")
		return false
	}
	if p.isControl() {
		logger.Infof("Got control packet: %d", len(data))
		// Here the server might be requesting us to reset, or to
		// re-key (but I keep ignoring that case for now).
		// we're doing nothing for now.
		fmt.Println(hex.Dump(p.payload))
		return false
	}
	if !p.isData() {
		logger.Warnf("unhandled data. (op: %d)", p.opcode)
		fmt.Println(hex.Dump(data))
		return false
	}
	if isPing(data) {
		m.handleDataPing()
		return false
	}

	// at this point, the incoming packet should be
	// a data packet that needs to be processed
	// (decompress+decrypt)

	plaintext, err := m.data.ReadPacket(p)
	if err != nil {
		logger.Errorf("bad decryption: %s", err.Error())
		// XXX I'm not sure returning false is the right thing to do here.
		return false
	}

	// all good! we write the plaintext into the read buffer.
	// the caller is responsible for reading from there.
	m.bufReader.Write(plaintext)
	return true
}

// handleDataPing replies to an openvpn-ping with a canned response.
func (m *muxer) handleDataPing() error {
	log.Println("openvpn-ping, sending reply")
	m.data.WritePacket(m.conn, pingPayload)
	return nil
}

// readTLSPacket reads a packet over the TLS connection.
func (m *muxer) readTLSPacket() ([]byte, error) {
	data := make([]byte, 4096)
	_, err := m.tls.Read(data)
	return data, err
}

// readAndLoadRemoteKey reads one incoming TLS packet, and tries to parse the
// response contained in it. If the server response is the right kind of
// packet, it will store the remote key and the parts of the remote options
// that will be of use later.
func (m *muxer) readAndLoadRemoteKey() error {
	data, err := m.readTLSPacket()
	if err != nil {
		return err
	}
	if !isControlMessage(data) {
		return fmt.Errorf("%w:%s", errBadControlMessage, "expected null header")
	}

	// Parse the received data: we expect remote key and remote options.
	remoteKey, opts, err := m.control.ReadControlMessage(data)
	if err != nil {
		logger.Errorf("cannot parse control message")
		return fmt.Errorf("%w:%s", ErrBadHandshake, err)
	}

	// Store the remote key.
	key, err := m.session.ActiveKey()
	if err != nil {
		logger.Errorf("cannot get active key")
		return fmt.Errorf("%w:%s", ErrBadHandshake, err)
	}
	key.addRemoteKey(remoteKey)

	// Parse and store the useful parts of the remote options.
	m.tunnel = parseRemoteOptions(m.tunnel, opts)
	return nil
}

// sendPushRequest sends a push request over the TLS channel.
func (m *muxer) sendPushRequest() (int, error) {
	return m.tls.Write(m.control.PushRequest())
}

// readPushReply reads one incoming TLS packet, where we expect to find the
// response to our push request. If the server response is the right kind of
// packet, it will store the parts of the pushed options that will be of use
// later.
func (m *muxer) readPushReply() error {
	resp, err := m.readTLSPacket()
	if err != nil {
		return err
	}

	logger.Info("Server pushed options")

	if isBadAuthReply(resp) {
		return errBadAuth
	}

	if !isPushReply(resp) {
		return fmt.Errorf("%w:%s", errBadServerReply, "expected push reply")
	}

	ip := m.control.ReadPushResponse(resp)
	m.tunnel.ip = ip
	logger.Infof("Tunnel IP: %s", ip)
	return nil
}

// sendControl message sends a control message over the TLS channel.
func (m *muxer) sendControlMessage() error {
	cm, err := m.control.ControlMessage(m.session, m.options)
	if err != nil {
		return err
	}
	if _, err := m.tls.Write(cm); err != nil {
		return err
	}
	return nil
}

// InitDataWithRemoteKey initializes the internal data channel. To do that, it sends a
// control packet, parses the response, and derives the cryptographic material
// that will be used to encrypt and decrypt data through the tunnel. At the end
// of this exchange, the data channel is ready to be used.
func (m *muxer) InitDataWithRemoteKey() error {

	// 1. first we send a control message.

	if err := m.sendControlMessage(); err != nil {
		return err
	}

	// 2. then we read the server response and load the remote key.

	if err := m.readAndLoadRemoteKey(); err != nil {
		return err
	}

	// 3. now we can initialize the data channel.

	key0, err := m.session.ActiveKey()
	if err != nil {
		return err
	}

	err = m.data.SetupKeys(key0) //, m.session) TODO session already in data
	if err != nil {
		return err
	}

	// 4. finally, we ask the server to push remote options to us. we parse
	// them and keep some useful info.

	if _, err := m.sendPushRequest(); err != nil {
		return err
	}
	if err := m.readPushReply(); err != nil {
		return err
	}
	return nil
}

// TODO(ainghazal, bassosimone): it probably makes sense to return an error
// from read/write if the data channel is not initialized. Another option would
// be to read from a channel and block if there's nothing.

// Write sends user bytes as encrypted packets in the data channel.
func (m *muxer) Write(b []byte) (int, error) {
	return m.data.WritePacket(m.conn, b)
}

// Read reads bytes after decrypting packets from the data channel. This is the
// user-view of the VPN connection reads.
func (m *muxer) Read(b []byte) (int, error) {
	for !m.handleIncomingPacket() {
	}
	return m.bufReader.Read(b)
}

var (
	ErrBadHandshake = errors.New("bad vpn handshake")
)
