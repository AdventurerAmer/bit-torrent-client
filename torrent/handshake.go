package torrent

import (
	"bytes"
	"encoding/binary"
	"io"
	"net"
	"time"
)

type Handshake struct {
	Str      string
	InfoHash [20]byte
	ID       [20]byte
}

func newHandshake(infoHash [20]byte, ID [20]byte) *Handshake {
	return &Handshake{
		Str:      "BitTorrent protocol",
		InfoHash: infoHash,
		ID:       ID,
	}
}

func (h *Handshake) Serialize() []byte {
	var b bytes.Buffer
	b.WriteByte(byte(len(h.Str)))
	b.Write([]byte(h.Str))
	extensions := make([]byte, 8)
	extensions[5] = 0x10 // extended handshake
	b.Write(extensions)
	b.Write(h.InfoHash[:])
	b.Write(h.ID[:])
	return b.Bytes()
}

func readHandshake(conn net.Conn, timeout time.Duration) (*Handshake, error) {
	length := uint8(0)

	for {
		err := conn.SetReadDeadline(time.Now().Add(timeout))
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				continue
			}
			return nil, err
		}
		err = binary.Read(conn, binary.BigEndian, &length)
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				continue
			}
			return nil, err
		} else {
			break
		}
	}

	str := make([]byte, length)

	for {
		err := conn.SetReadDeadline(time.Now().Add(timeout))
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				continue
			}
			return nil, err
		}
		_, err = io.ReadFull(conn, str)
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				continue
			}
			return nil, err
		} else {
			break
		}
	}

	buf := make([]byte, 8+20+20)
	for {
		err := conn.SetReadDeadline(time.Now().Add(timeout))
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				continue
			}
			return nil, err
		}
		_, err = io.ReadFull(conn, buf)
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				continue
			}
			return nil, err
		} else {
			break
		}
	}

	extensions := buf[:8]
	_ = extensions
	infoHash := buf[8:28]
	id := buf[28:]
	h := &Handshake{
		Str:      string(str),
		InfoHash: [20]byte(infoHash),
		ID:       [20]byte(id),
	}
	return h, nil
}
