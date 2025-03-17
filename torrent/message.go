package torrent

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"time"
)

type MessageID uint8

const (
	MessageChoke MessageID = iota
	MessageUnchoke
	MessageInterested
	MessageNotInterested
	MessageHave
	MessageBitfield
	MessageRequest
	MessagePiece
	MessageCancel
	MessageExtended = 20
)

type Message struct {
	ID      MessageID
	Payload []byte
}

func (m *Message) Serialize() []byte {
	if m == nil {
		return make([]byte, 4)
	}
	var b bytes.Buffer
	binary.Write(&b, binary.BigEndian, uint32(len(m.Payload)+1))
	b.WriteByte(byte(m.ID))
	b.Write(m.Payload)
	return b.Bytes()
}

func (m *Message) String() string {
	if m == nil {
		return "KeepAlive"
	}
	switch m.ID {
	case MessageChoke:
		return "Choke"
	case MessageUnchoke:
		return "Unchoke"
	case MessageInterested:
		return "Interested"
	case MessageNotInterested:
		return "Not Interested"
	case MessageHave:
		return "Have"
	case MessageBitfield:
		return "Bitfield"
	case MessageRequest:
		return "Request"
	case MessagePiece:
		return "Piece"
	case MessageCancel:
		return "Cancel"
	case MessageExtended:
		return "Extended"
	default:
		return "Unkown"
	}
}

func readMessage(conn net.Conn, timeout time.Duration) (*Message, error) {
	var length uint32
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

	// keep-alive message
	if length == 0 {
		return nil, nil
	}

	buf := make([]byte, length)

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
	msg := &Message{
		ID:      MessageID(buf[0]),
		Payload: buf[1:],
	}
	return msg, nil
}

func composeRequestMessage(pieceIndex, offset, blockSize int) *Message {
	var b bytes.Buffer
	binary.Write(&b, binary.BigEndian, uint32(pieceIndex))
	binary.Write(&b, binary.BigEndian, uint32(offset))
	binary.Write(&b, binary.BigEndian, uint32(blockSize))
	return &Message{
		ID:      MessageRequest,
		Payload: b.Bytes(),
	}
}

type HaveMessage struct {
	PieceIndex int
}

func composeHaveMessage(pieceIndex int) *Message {
	payload := make([]byte, 4)
	binary.BigEndian.PutUint32(payload, uint32(pieceIndex))
	haveMsg := &Message{
		ID:      MessageHave,
		Payload: payload,
	}
	return haveMsg
}

func parseHaveMessage(msg *Message) (*HaveMessage, error) {
	if len(msg.Payload) != 4 {
		return nil, fmt.Errorf("malformed msg %s payload must be 4 bytes", msg)
	}
	index := binary.BigEndian.Uint32(msg.Payload)
	haveMsg := &HaveMessage{
		PieceIndex: int(index),
	}
	return haveMsg, nil
}

type PieceMessage struct {
	PieceIndex  int
	BlockOffset int
	BlockData   []byte
}

func parsePieceMessage(msg *Message, pieceIndex int, pieceLength int) (*PieceMessage, error) {
	if len(msg.Payload) < 8 {
		return nil, fmt.Errorf("malformed msg %s payload must be atleast 8 bytes", msg)
	}
	index := binary.BigEndian.Uint32(msg.Payload[:4])
	if int(index) != pieceIndex {
		return nil, fmt.Errorf("malformed msg %s expected piece index: %d got %d", msg, pieceIndex, int(index))
	}
	offset := binary.BigEndian.Uint32(msg.Payload[4:8])
	if int(offset) >= pieceLength {
		return nil, fmt.Errorf("malformed msg %s block offset: %d is too high", msg, int(offset))
	}
	data := msg.Payload[8:]
	if int(offset)+len(data) > pieceLength {
		return nil, fmt.Errorf("malformed msg %s block data length: %d is too high", msg, len(data))
	}
	pieceMsg := &PieceMessage{
		PieceIndex:  int(index),
		BlockOffset: int(offset),
		BlockData:   data,
	}
	return pieceMsg, nil
}

type ExtendedMessage struct {
	ID        MessageID
	Extension byte
	Payload   []byte
}

func newExtendedMessage(extensionID byte, payload []byte) *ExtendedMessage {
	return &ExtendedMessage{
		ID:        MessageExtended,
		Extension: extensionID,
		Payload:   payload,
	}
}

func (e *ExtendedMessage) Serialize() []byte {
	if e == nil {
		return make([]byte, 4)
	}
	var b bytes.Buffer
	length := uint32(2 + len(e.Payload))
	binary.Write(&b, binary.BigEndian, length)
	b.WriteByte(byte(e.ID))
	b.WriteByte(byte(e.Extension))
	b.Write(e.Payload)
	return b.Bytes()
}

func convertMessageToExtended(msg *Message) *ExtendedMessage {
	return &ExtendedMessage{
		ID:        msg.ID,
		Extension: msg.Payload[0],
		Payload:   msg.Payload[1:],
	}
}
