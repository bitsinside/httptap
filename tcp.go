package main

import (
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/waiter"
)

// This file contains a "homegrown" TCP stack, which is only used with the --stack=homegrown command line argument.

// TCPState is the state of a TCP connection
type TCPState int

const (
	StateInit              TCPState = iota + 1
	StateSynReceived                // means we have received a SYN, replied with a SYN+ACK, and are awaiting an ACK from other side
	StateConnected                  // means we have received at least one ACK from other side so can send and receive data
	StateOtherSideFinished          // means we have received a FIN from other side and have responded with FIN+ACK
	StateFinished                   // means we have sent our own FIN and must not send any more data
)

func (s TCPState) String() string {
	switch s {
	case StateInit:
		return "StateInit"
	case StateSynReceived:
		return "StateSynchronizing"
	case StateConnected:
		return "StateConnected"
	case StateOtherSideFinished:
		return "StateOtherSideFinished"
	case StateFinished:
		return "StateFinished"
	default:
		return fmt.Sprintf("unknown(%d)", s)
	}
}

// AddrPort is an IP address and a port number
type AddrPort struct {
	Addr net.IP
	Port uint16
}

func (ap AddrPort) String() string {
	return ap.Addr.String() + ":" + strconv.Itoa(int(ap.Port))
}

// TCPRequest represents a request by a remote host to initiate a TCP connection. The interface provides
// a way for the application to decide whether or not to accept the connection.
type TCPRequest interface {
	// RemoteAddr is the IP address and port of the subprocess that initiated the connection
	RemoteAddr() net.Addr

	// LocalAddr is the IP address and port that the subprocess was trying to reach
	LocalAddr() net.Addr

	// Accept replies with a SYN+ACK back and returns a connection that can be read
	Accept() (net.Conn, error)

	// Reject replies with a RST and the connection is done
	Reject()
}

type tcpRequest struct {
	fr *tcp.ForwarderRequest
	wq *waiter.Queue
}

func (r *tcpRequest) RemoteAddr() net.Addr {
	addr := r.fr.ID().RemoteAddress
	return &net.TCPAddr{IP: addr.AsSlice(), Port: int(r.fr.ID().RemotePort)}
}

func (r *tcpRequest) LocalAddr() net.Addr {
	addr := r.fr.ID().LocalAddress
	return &net.TCPAddr{IP: addr.AsSlice(), Port: int(r.fr.ID().LocalPort)}
}

func (r *tcpRequest) Accept() (net.Conn, error) {
	ep, err := r.fr.CreateEndpoint(r.wq)
	if err != nil {
		r.fr.Complete(true)
		return nil, fmt.Errorf("CreateEndpoint: %v", err)
	}

	// TODO: set keepalive count, keepalive interval, receive buffer size, send buffer size, like this:
	//   https://github.com/xjasonlyu/tun2socks/blob/main/core/tcp.go#L83

	// create an adapter that makes a gvisor endpoint into a net.Conn
	conn := gonet.NewTCPConn(r.wq, ep)
	r.fr.Complete(false)
	return conn, nil
}

func (r *tcpRequest) Reject() {
	r.fr.Complete(true)
}

// TCP stream

type tcpStream struct {
	subprocess     AddrPort                 // address from which we originally intercepted packets generated by subprocess
	world          AddrPort                 // address to which the intercepted packets were addressed
	fromSubprocess chan []byte              // the stack sends packets here, we receive
	toSubprocess   chan []byte              // we send packets here, the stack receives
	serializeBuf   gopacket.SerializeBuffer // used to serialize gopacket structs to wire format
	state          TCPState                 // state of the connection
	seq            uint32                   // sequence number for packets going to the subprocess
	ack            uint32                   // the next acknowledgement number to send
}

func newTCPStream(world AddrPort, subprocess AddrPort, out chan []byte) *tcpStream {
	return &tcpStream{
		world:          world,
		subprocess:     subprocess,
		state:          StateInit,
		fromSubprocess: make(chan []byte, 1024),
		toSubprocess:   out,
		serializeBuf:   gopacket.NewSerializeBuffer(),
	}
}

// for net.Conn interface
func (s *tcpStream) SetDeadline(t time.Time) error {
	verbose("SetDeadline not implemented for TCP streams, ignoring")
	return nil
}

// for net.Conn interface
func (s *tcpStream) SetReadDeadline(t time.Time) error {
	verbose("SetReadDeadline not implemented for TCP streams, ignoring")
	return nil
}

// for net.Conn interface
func (s *tcpStream) SetWriteDeadline(t time.Time) error {
	verbose("SetWriteDeadline not implemented for TCP streams, ignoring")
	return nil
}

// for net.Conn interface
func (s *tcpStream) LocalAddr() net.Addr {
	return &net.TCPAddr{
		IP:   s.world.Addr,
		Port: int(s.world.Port),
	}
}

// for net.Conn interface
func (s *tcpStream) RemoteAddr() net.Addr {
	return &net.TCPAddr{
		IP:   s.subprocess.Addr,
		Port: int(s.subprocess.Port),
	}
}

// Accept sends a SYN+ACK packet. It is only valid to call this once, when the stream state is SynReceived
func (s *tcpStream) Accept() (net.Conn, error) {
	// reply to the subprocess as if the connection were already good to go
	replytcp := layers.TCP{
		SrcPort: layers.TCPPort(s.world.Port),
		DstPort: layers.TCPPort(s.subprocess.Port),
		SYN:     true,
		ACK:     true,
		Seq:     atomic.AddUint32(&s.seq, 1) - 1,
		Ack:     atomic.LoadUint32(&s.ack) + 1,
		Window:  64240, // number of bytes we are willing to receive
	}

	replyipv4 := layers.IPv4{
		Version:  4, // indicates IPv4
		TTL:      ttl,
		Protocol: layers.IPProtocolTCP,
		SrcIP:    s.world.Addr,
		DstIP:    s.subprocess.Addr,
	}

	replytcp.SetNetworkLayerForChecksum(&replyipv4)

	// log
	verbosef("sending SYN+ACK to subprocess: %s", summarizeTCP(&replyipv4, &replytcp, nil))

	// serialize the packet
	serialized, err := serializeTCP(&replyipv4, &replytcp, nil, s.serializeBuf)
	if err != nil {
		return nil, fmt.Errorf("error serializing reply TCP: %w, dropping", err)
	}

	// make a copy of the data
	cp := make([]byte, len(serialized))
	copy(cp, serialized)

	// send to the channel that goes to the subprocess
	select {
	case s.toSubprocess <- cp:
	default:
		verbosef("channel for sending to subprocess would have blocked, dropping %d bytes", len(cp))
	}

	// return the tcp stream, now exposed as a net.Conn
	return s, nil
}

// Reject sends a RST packet. It is only valid to call this once, when the stream state is SynReceived
func (s *tcpStream) Reject() {
	// reply to the subprocess as if the connection were already good to go
	replytcp := layers.TCP{
		SrcPort: layers.TCPPort(s.world.Port),
		DstPort: layers.TCPPort(s.subprocess.Port),
		RST:     true,
		ACK:     true,
		Seq:     atomic.AddUint32(&s.seq, 1) - 1,
		Ack:     atomic.LoadUint32(&s.ack) + 1,
		Window:  64240, // number of bytes we are willing to receive
	}

	replyipv4 := layers.IPv4{
		Version:  4, // indicates IPv4
		TTL:      ttl,
		Protocol: layers.IPProtocolTCP,
		SrcIP:    s.world.Addr,
		DstIP:    s.subprocess.Addr,
	}

	replytcp.SetNetworkLayerForChecksum(&replyipv4)

	// log
	verbosef("sending RST to subprocess: %s", summarizeTCP(&replyipv4, &replytcp, nil))

	// serialize the packet
	serialized, err := serializeTCP(&replyipv4, &replytcp, nil, s.serializeBuf)
	if err != nil {
		errorf("error serializing reply TCP: %w, dropping", err)
		return
	}

	// make a copy of the data
	cp := make([]byte, len(serialized))
	copy(cp, serialized)

	// send to the channel that goes to the subprocess
	select {
	case s.toSubprocess <- cp:
	default:
		verbosef("channel for sending to subprocess would have blocked, dropping %d bytes", len(cp))
	}
}

// Read reads packets sent by the subprocess and intercepted by us
func (s *tcpStream) Read(buf []byte) (int, error) {
	// read packets from the channel until we get a non-empty one
	for packet := range s.fromSubprocess {
		if len(packet) == 0 {
			continue // we must not return zero bytes according to io.Reader interface
		}

		if len(packet) > len(buf) {
			// TODO: deliver first part of this packet, store the rest for next Read()
			return 0, fmt.Errorf("received packet with %d bytes but receive buffer only has size %d", len(packet), len(buf))
		}

		// copy bytes into the buffer and return
		return copy(buf, packet), nil
	}

	// if we fell through then the channel is closed
	return 0, io.EOF
}

// Write writes payloads to the subprocess as if they came from the address that the subprocess
// was trying to reach when this stream was first intercepted.
func (s *tcpStream) Write(payload []byte) (int, error) {
	sz := uint32(len(payload))

	replytcp := layers.TCP{
		SrcPort: layers.TCPPort(s.world.Port),
		DstPort: layers.TCPPort(s.subprocess.Port),
		Seq:     atomic.AddUint32(&s.seq, sz) - sz, // sequence number on our side
		Ack:     atomic.LoadUint32(&s.ack),         // laste sequence number we saw on their side
		ACK:     true,                              // this indicates that we are acknolwedging some bytes
		Window:  64240,                             // number of bytes we are willing to receive (copied from sender)
	}

	replyipv4 := layers.IPv4{
		Version:  4, // indicates IPv4
		TTL:      ttl,
		Protocol: layers.IPProtocolTCP,
		SrcIP:    s.world.Addr,
		DstIP:    s.subprocess.Addr,
	}

	err := replytcp.SetNetworkLayerForChecksum(&replyipv4)
	if err != nil {
		return 0, fmt.Errorf("error setting network-layer TCP checksums: %w", err)
	}

	// log
	verbosef("sending tcp packet to subprocess: %s", summarizeTCP(&replyipv4, &replytcp, payload))

	// serialize the data
	packet, err := serializeTCP(&replyipv4, &replytcp, payload, s.serializeBuf)
	if err != nil {
		return 0, fmt.Errorf("error serializing TCP packet: %w", err)
	}

	// make a copy because the same buffer will be re-used
	cp := make([]byte, len(packet))
	copy(cp, packet)

	// send to the subprocess channel non-blocking
	select {
	case s.toSubprocess <- cp:
	default:
		return 0, fmt.Errorf("channel would have blocked")
	}

	// return number of bytes sent to us, not number of bytes written to underlying network
	return len(payload), nil
}

// Close the connection by sending a FIN packet
func (s *tcpStream) Close() error {
	switch s.state {
	case StateInit:
		errorf("application tried tp close a TCP stream in state %v, returning error", s.state)
		return fmt.Errorf("cannot close TCP stream in state %v", s.state)
	case StateFinished:
		verbosef("application tried tp close a TCP stream in state %v, ignoring", s.state)
		return nil
	}

	// TODO: s.state should be protected with a mutex
	s.state = StateFinished
	seq := atomic.AddUint32(&s.seq, 1) - 1

	// make a FIN packet to send to the subprocess
	tcp := layers.TCP{
		SrcPort: layers.TCPPort(s.world.Port),
		DstPort: layers.TCPPort(s.subprocess.Port),
		FIN:     true,
		ACK:     true,
		Seq:     seq,
		Ack:     atomic.LoadUint32(&s.ack),
		Window:  64240, // number of bytes we are willing to receive (copied from sender)
	}

	ipv4 := layers.IPv4{
		Version:  4, // indicates IPv4
		TTL:      ttl,
		Protocol: layers.IPProtocolTCP,
		SrcIP:    s.world.Addr,
		DstIP:    s.subprocess.Addr,
	}

	tcp.SetNetworkLayerForChecksum(&ipv4)

	// log
	verbosef("sending FIN to subprocess: %s", summarizeTCP(&ipv4, &tcp, nil))

	// serialize the packet (TODO: guard serializeBuf with a mutex)
	serialized, err := serializeTCP(&ipv4, &tcp, nil, s.serializeBuf)
	if err != nil {
		errorf("error serializing reply TCP: %v, dropping", err)
		return fmt.Errorf("error serializing reply TCP: %w, dropping", err)
	}

	// make a copy of the data
	cp := make([]byte, len(serialized))
	copy(cp, serialized)

	// send to the channel that goes to the subprocess
	select {
	case s.toSubprocess <- cp:
	default:
		verbosef("channel for sending to subprocess would have blocked, dropping %d bytes", len(cp))
	}

	return nil
}

func (s *tcpStream) deliverToApplication(payload []byte) {
	// copy the payload because it may be overwritten before the write loop gets to it
	cp := make([]byte, len(payload))
	copy(cp, payload)

	verbosef("stream enqueing %d bytes to send to world", len(payload))

	// send to channel unless it would block
	select {
	case s.fromSubprocess <- cp:
	default:
		verbosef("channel to world would block, dropping %d bytes", len(payload))
	}
}

// tcpStack accepts raw packets and handles TCP connections
type tcpStack struct {
	streamsBySrcDst map[string]*tcpStream
	toSubprocess    chan []byte // data sent to this channel goes to subprocess as raw IPv4 packet
	app             *mux
}

func newTCPStack(app *mux, link chan []byte) *tcpStack {
	return &tcpStack{
		streamsBySrcDst: make(map[string]*tcpStream),
		toSubprocess:    link,
		app:             app,
	}
}

func (s *tcpStack) handlePacket(ipv4 *layers.IPv4, tcp *layers.TCP, payload []byte) {
	// it happens that a process will connect to the same remote service multiple times in from
	// different source ports so we must key by a descriptor that includes both endpoints
	dst := AddrPort{Addr: ipv4.DstIP, Port: uint16(tcp.DstPort)}
	src := AddrPort{Addr: ipv4.SrcIP, Port: uint16(tcp.SrcPort)}

	// put source address, source port, destination address, and destination port into a human-readable string
	srcdst := src.String() + " => " + dst.String()
	stream, found := s.streamsBySrcDst[srcdst]
	if !found {
		// create a new stream no matter what kind of packet this is
		// later we will reject everything other than SYN packets sent to a fresh stream
		stream = newTCPStream(dst, src, s.toSubprocess)
		stream.ack = tcp.Seq
		s.streamsBySrcDst[srcdst] = stream
	}

	// handle connection establishment
	if tcp.SYN && stream.state == StateInit {
		stream.state = StateSynReceived
		atomic.StoreUint32(&stream.ack, tcp.Seq+1)
		verbosef("got SYN to %v:%v, now state is %v", ipv4.DstIP, tcp.DstPort, stream.state)
		s.app.notifyTCP(stream)
	}

	// handle connection teardown
	if tcp.FIN && stream.state != StateInit {
		// according to the tcp spec we are always allowed to ack the other side's FIN, even if we have
		// already sent our own FIN
		stream.state = StateOtherSideFinished
		seq := atomic.AddUint32(&stream.seq, 1) - 1
		atomic.StoreUint32(&stream.ack, tcp.Seq+1)
		verbosef("got FIN to %v:%v, now state is %v", ipv4.DstIP, tcp.DstPort, stream.state)

		// make a FIN+ACK reply to send to the subprocess
		replytcp := layers.TCP{
			SrcPort: tcp.DstPort,
			DstPort: tcp.SrcPort,
			FIN:     true,
			ACK:     true,
			Seq:     seq,
			Ack:     tcp.Seq + 1,
			Window:  64240, // number of bytes we are willing to receive (copied from sender)
		}

		replyipv4 := layers.IPv4{
			Version:  4, // indicates IPv4
			TTL:      ttl,
			Protocol: layers.IPProtocolTCP,
			SrcIP:    ipv4.DstIP,
			DstIP:    ipv4.SrcIP,
		}

		replytcp.SetNetworkLayerForChecksum(&replyipv4)

		// log
		verbosef("sending FIN+ACK to subprocess: %s", summarizeTCP(&replyipv4, &replytcp, nil))

		// serialize the packet
		serialized, err := serializeTCP(&replyipv4, &replytcp, nil, stream.serializeBuf)
		if err != nil {
			errorf("error serializing reply TCP: %v, dropping", err)
			return
		}

		// make a copy of the data
		cp := make([]byte, len(serialized))
		copy(cp, serialized)

		// send to the channel that goes to the subprocess
		select {
		case s.toSubprocess <- cp:
		default:
			verbosef("channel for sending to subprocess would have blocked, dropping %d bytes", len(cp))
		}
	}

	if tcp.ACK && stream.state == StateSynReceived {
		stream.state = StateConnected
		verbosef("got ACK to %v:%v, now state is %v", ipv4.DstIP, tcp.DstPort, stream.state)

		// nothing more to do here -- if there is a payload then it will be forwarded
		// to the subprocess in the block below
	}

	// payload packets will often have ACK set, which acknowledges previously sent bytes
	if !tcp.SYN && len(tcp.Payload) > 0 && stream.state == StateConnected {
		verbosef("got %d tcp bytes to %v:%v, forwarding to application", len(tcp.Payload), ipv4.DstIP, tcp.DstPort)

		// update TCP sequence number -- this is not an increment but an overwrite, so no race condition here
		atomic.StoreUint32(&stream.ack, tcp.Seq+uint32(len(tcp.Payload)))

		// deliver the payload to application-level listeners
		stream.deliverToApplication(tcp.Payload)
	}
}

// serializeTCP serializes a TCP packet
func serializeTCP(ipv4 *layers.IPv4, tcp *layers.TCP, payload []byte, tmp gopacket.SerializeBuffer) ([]byte, error) {
	opts := gopacket.SerializeOptions{
		FixLengths:       true,
		ComputeChecksums: true,
	}

	tmp.Clear()

	// each layer is *prepended*, treating the current buffer data as payload
	p, err := tmp.AppendBytes(len(payload))
	if err != nil {
		return nil, fmt.Errorf("error appending TCP payload to packet (%d bytes): %w", len(payload), err)
	}
	copy(p, payload)

	err = tcp.SerializeTo(tmp, opts)
	if err != nil {
		return nil, fmt.Errorf("error serializing TCP part of packet: %w", err)
	}

	err = ipv4.SerializeTo(tmp, opts)
	if err != nil {
		errorf("error serializing IP part of packet: %v", err)
	}

	return tmp.Bytes(), nil
}

// summarizeTCP summarizes a TCP packet into a single line for logging
func summarizeTCP(ipv4 *layers.IPv4, tcp *layers.TCP, payload []byte) string {
	var flags []string
	if tcp.FIN {
		flags = append(flags, "FIN")
	}
	if tcp.SYN {
		flags = append(flags, "SYN")
	}
	if tcp.RST {
		flags = append(flags, "RST")
	}
	if tcp.ACK {
		flags = append(flags, "ACK")
	}
	if tcp.URG {
		flags = append(flags, "URG")
	}
	if tcp.ECE {
		flags = append(flags, "ECE")
	}
	if tcp.CWR {
		flags = append(flags, "CWR")
	}
	if tcp.NS {
		flags = append(flags, "NS")
	}
	// ignore PSH flag

	flagstr := strings.Join(flags, "+")
	return fmt.Sprintf("TCP %v:%d => %v:%d %s - Seq %d - Ack %d - Len %d",
		ipv4.SrcIP, tcp.SrcPort, ipv4.DstIP, tcp.DstPort, flagstr, tcp.Seq, tcp.Ack, len(tcp.Payload))
}
