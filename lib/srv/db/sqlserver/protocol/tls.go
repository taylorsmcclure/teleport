package protocol

import (
	"bytes"
	"crypto/tls"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net"
	"time"

	"github.com/gravitational/trace"
)

func DoTLSHandshake(conn net.Conn, conf *tls.Config) (*tls.Conn, error) {
	handshakeConn := &tlsHandshakeConn{c: conn}

	passConn := &passthroughConn{handshakeConn}

	tlsConn := tls.Server(passConn, conf)

	if err := tlsConn.Handshake(); err != nil {
		return nil, trace.Wrap(err)
	}

	fmt.Printf("==> Connection state: %#v\n", tlsConn.ConnectionState())

	passConn.c = conn

	return tlsConn, nil
}

type tlsHandshakeConn struct {
	c net.Conn

	packetWriteInProgress bool
	packetReadInProgress  bool
	b                     bytes.Buffer
}

func (c *tlsHandshakeConn) Read2(b []byte) (int, error) {
	// If we've been writing a packet, flush it first before starting a read.
	if c.packetWriteInProgress {
		fmt.Println("=== TLS DEBUG === FLUSHING BUFFER")
		pkt := c.b.Bytes()

		// Update packet length.
		binary.BigEndian.PutUint16(pkt[2:], uint16(len(pkt)))

		fmt.Printf("=== WRITING TLS PACKET LEN: %v ===\n", len(pkt))
		fmt.Println(hex.Dump(pkt))
		fmt.Println("=======================")

		// Write to the connection.
		_, err := c.c.Write(pkt)
		if err != nil {
			return 0, trace.Wrap(err)
		}

		// Reset the flag so when the next write comes we'll start a new packet.
		c.packetWriteInProgress = false
		c.packetReadInProgress = false
	}

	// Read a new packet.
	if !c.packetReadInProgress {
		fmt.Println("=== TLS DEBUG === STARTING NEW READ PACKET")
		pkt, err := ReadPacket(c.c)
		if err != nil {
			return 0, trace.Wrap(err)
		}
		if pkt.Type != PacketTypePreLogin {
			return 0, trace.BadParameter("expected PRELOGIN packet, got: %#v", pkt.Type)
		}
		c.b.Reset()
		c.b.Write(pkt.Data)
		c.packetReadInProgress = true
	}
	fmt.Println("=== TLS DEBUG === READING FROM BUFFER")
	return c.b.Read(b)
}

func (c *tlsHandshakeConn) Read(b []byte) (int, error) {
	if c.b.Len() > 0 {
		fmt.Println("=== TLS DEBUG === READING REMAINDER FROM BUFFER")
		return c.b.Read(b)
	}

	// Read a new packet.
	fmt.Println("=== TLS DEBUG === STARTING NEW READ PACKET")
	pkt, err := ReadPacket(c.c)
	if err != nil {
		return 0, trace.Wrap(err)
	}
	if pkt.Type != PacketTypePreLogin {
		return 0, trace.BadParameter("expected PRELOGIN packet, got: %#v", pkt.Type)
	}
	c.b.Write(pkt.Data) // TODO handle error

	fmt.Println("=== TLS DEBUG === READING FROM BUFFER")
	return c.b.Read(b)
}

func (c *tlsHandshakeConn) Write(b []byte) (int, error) {
	// TLS payload should be sent as PRELOGIN packets.
	pkt := append([]byte{
		PacketTypePreLogin,
		0x01,
		0, 0, // length
		0, 0,
		0,
		0,
	}, b...)

	// Update packet length.
	binary.BigEndian.PutUint16(pkt[2:], uint16(len(pkt)))

	fmt.Printf("=== WRITING TLS PACKET LEN: %v ===\n", len(pkt))
	fmt.Println(hex.Dump(pkt))
	fmt.Println("=======================")

	return c.c.Write(pkt)
}

func (c *tlsHandshakeConn) Write2(b []byte) (int, error) {
	// Start a new PRELOGIN packet unless we're already writing one.
	if !c.packetWriteInProgress {
		fmt.Println("=== TLS DEBUG === STARTING NEW WRITE PACKET")
		c.b.Write([]byte{
			// TLS payload should be sent as PRELOGIN packets.
			PacketTypePreLogin,
			0x01,
			0, 0, // length
			0, 0,
			0,
			0,
		})
		c.packetWriteInProgress = true
	}
	fmt.Println("=== TLS DEBUG === WRITING TO BUFFER")
	return c.b.Write(b)
}

func (c *tlsHandshakeConn) Close() error {
	return c.c.Close()
}

func (c *tlsHandshakeConn) LocalAddr() net.Addr {
	return nil
}

func (c *tlsHandshakeConn) RemoteAddr() net.Addr {
	return nil
}

func (c *tlsHandshakeConn) SetDeadline(_ time.Time) error {
	return nil
}

func (c *tlsHandshakeConn) SetReadDeadline(_ time.Time) error {
	return nil
}

func (c *tlsHandshakeConn) SetWriteDeadline(_ time.Time) error {
	return nil
}

type passthroughConn struct {
	c net.Conn
}

func (c passthroughConn) Read(b []byte) (n int, err error) {
	return c.c.Read(b)
}

func (c passthroughConn) Write(b []byte) (n int, err error) {
	return c.c.Write(b)
}

func (c passthroughConn) Close() error {
	return c.c.Close()
}

func (c passthroughConn) LocalAddr() net.Addr {
	return c.c.LocalAddr()
}

func (c passthroughConn) RemoteAddr() net.Addr {
	return c.c.RemoteAddr()
}

func (c passthroughConn) SetDeadline(t time.Time) error {
	return c.c.SetDeadline(t)
}

func (c passthroughConn) SetReadDeadline(t time.Time) error {
	return c.c.SetReadDeadline(t)
}

func (c passthroughConn) SetWriteDeadline(t time.Time) error {
	return c.c.SetWriteDeadline(t)
}
