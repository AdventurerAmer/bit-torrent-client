package torrent

import (
	"bytes"
	"io"
)

type Handshake struct {
	Str      string
	InfoHash [20]byte
	ID       [20]byte
}

func NewHandshake(infoHash [20]byte, ID [20]byte) *Handshake {
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
	b.Write(extensions)
	b.Write(h.InfoHash[:])
	b.Write(h.ID[:])
	return b.Bytes()
}

func ReadHandshake(r io.Reader) (*Handshake, error) {
	length := make([]byte, 1)
	_, err := io.ReadFull(r, length)
	if err != nil {
		return nil, err
	}
	str := make([]byte, length[0])
	_, err = io.ReadFull(r, str)
	if err != nil {
		return nil, err
	}
	extensions := make([]byte, 8)
	_, err = io.ReadFull(r, extensions)
	if err != nil {
		return nil, err
	}
	infoHash := make([]byte, 20)
	_, err = io.ReadFull(r, infoHash)
	if err != nil {
		return nil, err
	}
	ID := make([]byte, 20)
	_, err = io.ReadFull(r, ID)
	if err != nil {
		return nil, err
	}
	h := &Handshake{
		Str:      string(str),
		InfoHash: [20]byte(infoHash),
		ID:       [20]byte(ID),
	}
	return h, nil
}
