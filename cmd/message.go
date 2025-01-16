package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
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
	length := make([]byte, 4)
	binary.BigEndian.PutUint32(length, uint32(len(m.Payload)+1))
	b.Write(length)
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
	default:
		return "Unkown"
	}
}

func ReadMessage(r io.Reader) (*Message, error) {
	lengthBuf := make([]byte, 4)
	_, err := io.ReadFull(r, lengthBuf)
	if err != nil {
		return nil, err
	}
	length := binary.BigEndian.Uint32(lengthBuf)
	// keep-alive message
	if length == 0 {
		return nil, nil
	}
	buf := make([]byte, length)
	_, err = io.ReadFull(r, buf)
	if err != nil {
		return nil, err
	}
	msg := &Message{
		ID:      MessageID(buf[0]),
		Payload: buf[1:],
	}
	return msg, nil
}

func ComposeRequestMessage(pieceIndex, offset, blockSize int) *Message {
	payload := make([]byte, 12)
	binary.BigEndian.PutUint32(payload[:4], uint32(pieceIndex))
	binary.BigEndian.PutUint32(payload[4:8], uint32(offset))
	binary.BigEndian.PutUint32(payload[8:12], uint32(blockSize))
	return &Message{
		ID:      MessageRequest,
		Payload: payload,
	}
}

type HaveMessage struct {
	PieceIndex int
}

func ComposeHaveMessage(pieceIndex int) *Message {
	payload := make([]byte, 4)
	binary.BigEndian.PutUint32(payload, uint32(pieceIndex))
	haveMsg := &Message{
		ID:      MessageHave,
		Payload: payload,
	}
	return haveMsg
}

func ParseHaveMessage(msg *Message) (*HaveMessage, error) {
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

func ParsePieceMessage(msg *Message, pieceIndex int, pieceLength int) (*PieceMessage, error) {
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
