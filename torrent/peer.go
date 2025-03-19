package torrent

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"log"
	"net"
	"time"
)

type Peer struct {
	Addr   string
	ID     [20]byte
	bf     BitField
	Choked bool
}

func newPeer(addr string) Peer {
	return Peer{
		Addr: addr,
	}
}

func (p Peer) String() string {
	sha := sha1.New()
	sha.Write(p.ID[:])
	sha.Write([]byte(p.Addr))
	hash := sha.Sum(nil)
	return hex.EncodeToString(hash[:8])
}

func (p *Peer) ShakeHands(conn net.Conn, readTimeout time.Duration, infoHash [20]byte, ID [20]byte) error {
	handShakeMsg := newHandshake(infoHash, ID).Serialize()
	for {
		_, err := conn.Write(handShakeMsg)
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				continue
			}
			return err
		} else {
			break
		}
	}
	peerHandShake, err := readHandshake(conn, readTimeout)
	if err != nil {
		return err
	}
	if !bytes.Equal(peerHandShake.InfoHash[:], infoHash[:]) {
		return fmt.Errorf("failed to handshake peer %s: info hash mismatch", p)
	}
	p.ID = peerHandShake.ID
	return nil
}

func (p *Peer) HandleBitFieldMessage(msg *Message) {
	p.bf = BitField(msg.Payload)
}

func (p *Peer) IsBitFieldValid(pieceCount int) error {
	payloadLength := pieceCount
	remainder := pieceCount % 8
	if remainder != 0 {
		payloadLength += (8 - remainder)
	}

	if len(p.bf)*8 != payloadLength {
		return fmt.Errorf("invalid bitfield message from peer %s: payload must be of length %d but got %d", p, payloadLength, len(p.bf)*8)
	}
	return nil
}

func (p Peer) SendUnchokeMessage(conn net.Conn) error {
	unchokeMsg := &Message{ID: MessageUnchoke}
	for {
		_, err := conn.Write(unchokeMsg.Serialize())
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				continue
			}
			return err
		} else {
			break
		}
	}
	return nil
}

func (p Peer) SendInterestedMessage(conn net.Conn) error {
	interestedMsg := &Message{ID: MessageInterested}
	for {
		_, err := conn.Write(interestedMsg.Serialize())
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				continue
			}
			return err
		} else {
			break
		}
	}
	return nil
}

func (p Peer) HasPiece(pieceIndex int) bool {
	return p.bf == nil || p.bf.IsSet(pieceIndex)
}

func (p *Peer) SetPiece(pieceIndex int) {
	p.bf.Set(pieceIndex)
}

func (p *Peer) DownloadPiece(conn net.Conn, readTimeout time.Duration, pieceIndex int, pieceLength int, pieceHash [20]byte, logger *log.Logger) ([]byte, error) {
	const MaxBlockSize = 16384
	const MaxBacklog = 5

	pieceData := make([]byte, pieceLength)
	downloaded := 0
	blockOffset := 0
	backlog := 0

	for downloaded < pieceLength {
		if !p.Choked {
			for blockOffset < pieceLength && backlog < MaxBacklog {
				blockSize := MaxBlockSize
				if pieceLength-blockOffset < blockSize {
					blockSize = pieceLength - blockOffset
				}
				for {
					_, err := conn.Write(composeRequestMessage(pieceIndex, blockOffset, blockSize).Serialize())
					if err != nil {
						if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
							continue
						}
						return nil, err
					} else {
						break
					}
				}
				blockOffset += blockSize
				backlog++
			}
		}
		msg, err := readMessage(conn, readTimeout)
		if err != nil {
			return nil, err
		}
		if msg != nil {
			if msg.ID == MessagePiece {
				pieceMsg, err := parsePieceMessage(msg, pieceIndex, pieceLength)
				if err != nil {
					logger.Println(err)
				} else {
					copy(pieceData[pieceMsg.BlockOffset:], pieceMsg.BlockData)
					downloaded += len(pieceMsg.BlockData)
					backlog--
				}
			} else {
				err := p.HandleMessage(msg)
				if err != nil {
					logger.Println(err)
				}
			}
		}
	}
	hash := sha1.Sum(pieceData)
	if !bytes.Equal(hash[:], pieceHash[:]) {
		return nil, fmt.Errorf("Piece #%d failed integrity check got hash %s should be %s", pieceIndex, hex.EncodeToString(hash[:]), hex.EncodeToString(pieceHash[:]))
	}
	return pieceData, nil
}

func (p *Peer) HandleMessage(msg *Message) error {
	switch msg.ID {
	case MessageBitfield:
		p.HandleBitFieldMessage(msg)
	case MessageUnchoke:
		p.Choked = false
	case MessageChoke:
		p.Choked = true
	case MessageHave:
		haveMsg, err := parseHaveMessage(msg)
		if err != nil {
			return err
		} else {
			p.SetPiece(haveMsg.PieceIndex)
		}
	}
	return nil
}
