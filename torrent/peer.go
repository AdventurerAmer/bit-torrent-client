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
	IP     net.IP
	Port   uint16
	ID     [20]byte
	bf     BitField
	Choked bool
}

func (p Peer) String() string {
	return fmt.Sprintf("%s:%d", p.IP, p.Port)
}

func (p Peer) GetAddr() string {
	return fmt.Sprintf("%s:%d", p.IP, p.Port)
}

func (p *Peer) ShakeHands(conn net.Conn, infoHash [20]byte, ID [20]byte) error {
	handShake := NewHandshake(infoHash, ID)
	handShakeMsg := handShake.Serialize()
	_, err := conn.Write(handShakeMsg)
	if err != nil {
		return err
	}
	peerHandShake, err := ReadHandshake(conn)
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
	_, err := conn.Write(unchokeMsg.Serialize())
	return err
}

func (p Peer) SendInterestedMessage(conn net.Conn) error {
	interestedMsg := &Message{ID: MessageInterested}
	_, err := conn.Write(interestedMsg.Serialize())
	return err
}

func (p Peer) HasPiece(pieceIndex int) bool {
	return p.bf == nil || p.bf.IsSet(pieceIndex)
}

func (p *Peer) SetPiece(pieceIndex int) {
	p.bf.Set(pieceIndex)
}

const MaxBlockSize = 16384
const MaxBacklog = 5

func (p *Peer) DownloadPiece(conn net.Conn, pieceIndex int, pieceLength int, pieceHash [20]byte) ([]byte, error) {
	conn.SetDeadline(time.Now().Add(30 * time.Second))
	defer conn.SetDeadline(time.Time{})

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
				requestMsg := ComposeRequestMessage(pieceIndex, blockOffset, blockSize)
				_, err := conn.Write(requestMsg.Serialize())
				if err != nil {
					return nil, err
				}
				blockOffset += blockSize
				backlog++
			}
		}
		msg, err := ReadMessage(conn)
		if err != nil {
			return nil, err
		}
		if msg != nil {
			switch msg.ID {
			case MessageBitfield:
				p.HandleBitFieldMessage(msg)
			case MessageUnchoke:
				p.Choked = false
			case MessageChoke:
				p.Choked = true
			case MessageHave:
				haveMsg, err := ParseHaveMessage(msg)
				if err != nil {
					log.Println(err)
				} else {
					p.SetPiece(haveMsg.PieceIndex)
				}
			case MessagePiece:
				pieceMsg, err := ParsePieceMessage(msg, pieceIndex, pieceLength)
				if err != nil {
					log.Println(err)
				} else {
					copy(pieceData[pieceMsg.BlockOffset:], pieceMsg.BlockData)
					downloaded += len(pieceMsg.BlockData)
					backlog--
				}
			}
		}
	}
	hash := sha1.Sum(pieceData)
	if !bytes.Equal(hash[:], pieceHash[:]) {
		return nil, fmt.Errorf("piece %d failed integrity check got hash %s should be %s", pieceIndex, hex.EncodeToString(hash[:]), hex.EncodeToString(pieceHash[:]))
	}
	return pieceData, nil
}
