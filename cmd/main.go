package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// TODO: use reflection to implement Unmarshal on the `Torrent` Struct
type Torrent struct {
	Announce     string
	AnnounceList []string
	Name         string
	Length       int
	Files        []map[string]any
	PieceLength  int
	Pieces       [][20]byte
	InfoHash     [20]byte
}

func (t Torrent) GetPeers(peerID [20]byte, port int) []Peer {
	announceUrls := []string{t.Announce}
	announceUrls = append(announceUrls, t.AnnounceList...)
	peerSet := make(map[string]Peer)
	// TODO: make this concurrent
	for _, announceUrl := range announceUrls {
		func() {
			base, err := url.Parse(announceUrl)
			if err != nil {
				log.Println(err)
				return
			}
			params := url.Values{
				"info_hash":  []string{string(t.InfoHash[:])},
				"peer_id":    []string{string(peerID[:])},
				"port":       []string{strconv.Itoa(port)},
				"uploaded":   []string{"0"},
				"downloaded": []string{"0"},
				"compact":    []string{"1"},
				"left":       []string{strconv.Itoa(t.Length)},
			}
			base.RawQuery = params.Encode()
			trackerURL := base.String()

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			req, err := http.NewRequestWithContext(ctx, "GET", trackerURL, nil)
			if err != nil {
				log.Println(err)
				return
			}

			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				log.Println(err)
				return
			}

			defer resp.Body.Close()
			body, err := decodeBecoding(resp.Body)
			if err != nil {
				log.Println(err, resp.Body)
				return
			}
			data, ok := body.(map[string]any)
			if !ok {
				log.Printf("expected a dictionary got %T", data)
				return
			}
			peersStr, ok := data["peers"].(string)
			if !ok {
				log.Printf("missing \"peers\" key in %v", data)
				return
			}
			// assuming ipv4 here
			if len(peersStr)%6 != 0 {
				log.Println("invalid peers string: len(peers) must be divisible by 6")
				return
			}
			peerCount := len(peersStr) / 6
			for i := 0; i < peerCount; i++ {
				peerData := []byte(peersStr[i*6:])
				peer := Peer{
					IP:   net.IP(peerData[:4]),
					Port: int(binary.BigEndian.Uint16(peerData[4:])),
				}
				peerStr := fmt.Sprintf("%s:%d", peer.IP, peer.Port)
				peerSet[peerStr] = peer
			}
		}()
	}
	peers := make([]Peer, 0, len(peerSet))
	for _, v := range peerSet {
		peers = append(peers, v)
	}
	return peers
}

func parseTorrentFile(filepath string) (*Torrent, error) {
	p, err := os.ReadFile(filepath)
	if err != nil {
		return nil, err
	}
	data, err := decodeBecoding(bytes.NewReader(p))
	if err != nil {
		return nil, err
	}
	metainfo, ok := data.(map[string]any)
	if !ok {
		return nil, errors.New("invalid torrent file: missing \"metainfo\" map")
	}
	announce, ok := metainfo["announce"].(string)
	if !ok {
		return nil, errors.New("invalid torrent file: missing \"announce\" key")
	}
	announceUrls := []string{}
	announceList, ok := metainfo["announce-list"].([]any)
	if ok {
		for listIndex, list := range announceList {
			items, ok := list.([]any)
			if !ok {
				return nil, fmt.Errorf("invalid torrent file: invalid announce list type at index %d expected []any got %T", listIndex, items)
			}
			for itemIndex, item := range items {
				s, ok := item.(string)
				if !ok {
					return nil, fmt.Errorf("invalid torrent file: invalid announce list item type at index %d expected string got %T", itemIndex, item)
				}
				announceUrls = append(announceUrls, s)
			}
		}
	}
	info, ok := metainfo["info"].(map[string]any)
	if !ok {
		return nil, errors.New("invalid torrent file: type mismatch \"info\" key must a dictionary")
	}
	name := info["name"].(string)
	if !ok {
		return nil, errors.New("invalid torrent file: type mismatch value of key \"name\" in info dictionary must a string")
	}
	length, ok := info["length"].(int)
	files := []map[string]any{}
	if !ok {
		filesList, ok := info["files"].([]any)
		if !ok {
			return nil, errors.New(`invalid torrent file: missing both "length" and "files" keys info dictionary`)
		}
		for idx, fileMap := range filesList {
			file, ok := fileMap.(map[string]any)
			if !ok {
				return nil, fmt.Errorf(`invalid torrent file: file number %d must be of type map[string]any`, idx)
			}
			fileLength, ok := file["length"].(int)
			if !ok {
				return nil, fmt.Errorf(`invalid torrent file: "length" key of file number %d must be an int`, idx)
			}
			length += fileLength
			files = append(files, file)
		}
	}
	pieceLength, ok := info["piece length"].(int)
	if !ok {
		return nil, errors.New("invalid torrent file: type mismatch value of key \"piece length\" in info dictionary must a int")
	}
	piecesStr, ok := info["pieces"].(string)
	if !ok {
		return nil, errors.New("invalid torrent file: type mismatch value of key \"pieces\" in info dictionary must a string")
	}
	if len(piecesStr)%20 != 0 {
		return nil, errors.New("invalid torrent file: invalid piece hashes")
	}
	pieceCount := len(piecesStr) / 20
	pieces := make([][20]byte, pieceCount)
	for i := 0; i < pieceCount; i++ {
		copy(pieces[i][:], piecesStr[i*20:i*20+20])
	}
	// @Hack until i implement marshaling
	infoLiteral := "4:info"
	idx := strings.Index(string(p), infoLiteral)
	s := string(p[idx+len(infoLiteral):])
	_, remaining, _ := decode(s)
	infoStr := s[:len(s)-len(remaining)]

	hasher := sha1.New()
	hasher.Write([]byte(infoStr))
	hash := hasher.Sum(nil)
	infoHash := [20]byte(hash)
	t := &Torrent{
		Announce:     announce,
		AnnounceList: announceUrls,
		Name:         name,
		Length:       length,
		Files:        files,
		PieceLength:  pieceLength,
		Pieces:       pieces,
		InfoHash:     infoHash,
	}
	return t, nil
}

type Peer struct {
	IP   net.IP
	Port int
}

type Handshake struct {
	Str      string
	InfoHash [20]byte
	PeerID   [20]byte
}

func NewHandshake(t *Torrent, peerID [20]byte) *Handshake {
	return &Handshake{
		Str:      "BitTorrent protocol",
		InfoHash: t.InfoHash,
		PeerID:   peerID,
	}
}

func (h *Handshake) Serialize() []byte {
	var b bytes.Buffer
	b.WriteByte(byte(len(h.Str)))
	b.Write([]byte(h.Str))
	extensions := make([]byte, 8)
	b.Write(extensions)
	b.Write(h.InfoHash[:])
	b.Write(h.PeerID[:])
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
	peerID := make([]byte, 20)
	_, err = io.ReadFull(r, peerID)
	if err != nil {
		return nil, err
	}
	return &Handshake{
		Str:      string(str),
		InfoHash: [20]byte(infoHash),
		PeerID:   [20]byte(peerID),
	}, nil
}

type messageID uint8

const (
	MsgChoke         messageID = 0
	MsgUnchoke       messageID = 1
	MsgInterested    messageID = 2
	MsgNotInterested messageID = 3
	MsgHave          messageID = 4
	MsgBitfield      messageID = 5
	MsgRequest       messageID = 6
	MsgPiece         messageID = 7
	MsgCancel        messageID = 8
)

type Message struct {
	ID      messageID
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
	case MsgChoke:
		return "Choke"
	case MsgUnchoke:
		return "Unchoke"
	case MsgInterested:
		return "Interested"
	case MsgNotInterested:
		return "Not Interested"
	case MsgHave:
		return "Have"
	case MsgBitfield:
		return "Bitfield"
	case MsgRequest:
		return "Request"
	case MsgPiece:
		return "Piece"
	case MsgCancel:
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
	msg := make([]byte, length)
	_, err = io.ReadFull(r, msg)
	if err != nil {
		return nil, err
	}
	return &Message{
		ID:      messageID(msg[0]),
		Payload: msg[1:],
	}, nil
}

type BitField []byte

func (b BitField) IsSet(index int) bool {
	byteIndex := index / 8
	offset := index % 8
	return b[byteIndex]>>(7-offset)&1 != 0
}

func (b BitField) Set(index int) {
	byteIndex := index / 8
	offset := index % 8
	b[byteIndex] |= 1 << (7 - offset)
}

func (b BitField) String() string {
	if b == nil {
		return "[]"
	}
	var builder strings.Builder
	builder.WriteString("[")
	for i := 0; i < len(b); i++ {
		builder.WriteString(fmt.Sprintf("%08b", b[i]))
	}
	builder.WriteString("]")
	return builder.String()
}

type PieceRequest struct {
	Index  int
	Hash   [20]byte
	Length int
}

type PieceResponce struct {
	Index int
	Data  []byte
}

func main() {
	torrent, err := parseTorrentFile("sample.torrent")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("Info Hash:", hex.EncodeToString(torrent.InfoHash[:]))
	fmt.Println("Length", torrent.Length)

	peerID := [20]byte{}
	n, err := rand.Read(peerID[:])
	if err != nil {
		log.Fatal(err)
	}
	if n != len(peerID) {
		log.Fatal("failed to generate peerID")
	}
	port := 6881 // clients will try 6881 to 6889 before giving up.
	fmt.Printf("Starting peer %s on port %d\n", hex.EncodeToString(peerID[:]), port)
	peers := torrent.GetPeers(peerID, port)
	if len(peers) == 0 {
		log.Fatal("failed to get peers")
	}
	requests := make(chan PieceRequest, len(torrent.Pieces))
	responses := make(chan PieceResponce, len(torrent.Pieces))
	for i := 0; i < len(torrent.Pieces); i++ {
		start := i * torrent.PieceLength
		end := start + torrent.PieceLength
		if end > torrent.Length {
			end = torrent.Length
		}
		requests <- PieceRequest{
			Index:  i,
			Hash:   torrent.Pieces[i],
			Length: end - start,
		}
	}
	for _, peer := range peers {
		go func(peer Peer, requestCh chan PieceRequest, responses chan<- PieceResponce) {
			addr := fmt.Sprintf("%s:%d", peer.IP, peer.Port)
			conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
			if err != nil {
				log.Println(err)
				return
			}
			defer conn.Close()
			handShake := NewHandshake(torrent, peerID)
			handShakeMsg := handShake.Serialize()
			_, err = conn.Write(handShakeMsg)
			if err != nil {
				log.Println(err)
				return
			}
			buf := make([]byte, 4096)
			n, err := conn.Read(buf[:])
			if err != nil {
				log.Println(err)
				return
			}
			peerHandShake, err := ReadHandshake(bytes.NewReader(buf[:n]))
			if err != nil {
				log.Println(err)
				return
			}

			if !bytes.Equal(peerHandShake.InfoHash[:], torrent.InfoHash[:]) {
				log.Printf("failed to handshake peer: %s", addr)
				return
			}

			log.Printf("Completed handshake with %s\n", addr)
			bitFieldMsg, err := ReadMessage(conn)
			if err != nil {
				log.Println(err)
				return
			}

			if bitFieldMsg == nil {
				return
			}

			if bitFieldMsg.ID != MsgBitfield {
				log.Printf("couldn't get bitfield from peer: %s", addr)
				return
			}

			bitField := BitField(bitFieldMsg.Payload)
			log.Printf("got bitfield %s from peer: %s", bitField.String(), addr)

			unchokeMsg := &Message{ID: MsgUnchoke}
			conn.Write(unchokeMsg.Serialize())

			interestedMsg := &Message{ID: MsgInterested}
			conn.Write(interestedMsg.Serialize())

			Choked := true

			for {
				request, ok := <-requests
				if !ok {
					break
				}
				if !bitField.IsSet(request.Index) {
					requests <- request
					continue
				}
				requestBuf := make([]byte, request.Length)
				const MaxBlockSize = 16384
				const MaxBacklog = 5
				downloaded := 0
				offset := 0
				backlog := 0
			loop:
				for downloaded < request.Length {
					if !Choked {
						for offset < request.Length && backlog < MaxBacklog {
							blockSize := MaxBlockSize
							if request.Length-offset < blockSize {
								blockSize = request.Length - offset
							}
							payload := make([]byte, 12)
							binary.BigEndian.PutUint32(payload[:4], uint32(request.Index))
							binary.BigEndian.PutUint32(payload[4:8], uint32(offset))
							binary.BigEndian.PutUint32(payload[8:12], uint32(blockSize))
							requestMsg := &Message{
								ID:      MsgRequest,
								Payload: payload,
							}
							_, err := conn.Write(requestMsg.Serialize())
							if err != nil {
								log.Println(err)
								requests <- request
								break loop
							}
							offset += blockSize
							backlog++
						}
					}
					msg, err := ReadMessage(conn)
					if err != nil {
						log.Println(err)
						requests <- request
						break loop
					}
					switch msg.ID {
					case MsgUnchoke:
						Choked = false
					case MsgChoke:
						Choked = true
					case MsgHave:
						if len(msg.Payload) != 4 {
							log.Printf("malformed msg %s payload must be 4 bytes", msg)
							break
						}
						index := binary.BigEndian.Uint32(msg.Payload)
						bitField.Set(int(index))
					case MsgPiece:
						if len(msg.Payload) < 8 {
							log.Printf("malformed msg %s payload must be atleast 8 bytes", msg)
							break
						}
						index := binary.BigEndian.Uint32(msg.Payload[:4])
						if index != uint32(request.Index) {
							log.Printf("malformed msg %s expected piece index: %d got %d", msg, request.Index, int(index))
							break
						}
						offset := binary.BigEndian.Uint32(msg.Payload[4:8])
						if int(offset) >= request.Length {
							log.Printf("malformed msg %s offset: %d is too high", msg, int(offset))
							break
						}
						data := msg.Payload[8:]
						if int(offset)+len(data) > request.Length {
							log.Printf("malformed msg %s data length: %d is too high", msg, len(data))
							break
						}
						downloaded += len(data)
						backlog--
						copy(requestBuf[offset:], data)
					}
				}

				hash := sha1.Sum(requestBuf)
				if !bytes.Equal(hash[:], request.Hash[:]) {
					log.Printf("Piece %d failed integrity check\n", request.Index)
					requests <- request
					continue
				}

				havePayload := make([]byte, 4)
				binary.BigEndian.PutUint32(havePayload, uint32(request.Index))
				haveMsg := Message{
					ID:      MsgHave,
					Payload: havePayload,
				}
				conn.Write(haveMsg.Serialize())
				responses <- PieceResponce{
					Index: request.Index,
					Data:  requestBuf,
				}
			}
		}(peer, requests, responses)
	}
	downloadedPices := 0
	for i := 0; i < len(torrent.Pieces); i++ {
		resp := <-responses
		downloadedPices++
		progress := float64(downloadedPices) / float64(len(torrent.Pieces)) * 100
		log.Printf("Downloaded Piece %d successfully progress %.2f%%", resp.Index, progress)
	}
	close(requests)
	close(responses)
	log.Printf("Success")
}
