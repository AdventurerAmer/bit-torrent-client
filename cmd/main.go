package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/binary"
	"errors"
	"fmt"
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
					Port: binary.BigEndian.Uint16(peerData[4:]),
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

type PieceRequest struct {
	Index  int
	Hash   [20]byte
	Length int
}

type PieceResponce struct {
	Index int
	Data  []byte
}

func generateClientID() ([20]byte, error) {
	clientID := [20]byte{}
	n, err := rand.Read(clientID[:])
	if err != nil {
		return [20]byte{}, err
	}
	if n != len(clientID) {
		return [20]byte{}, fmt.Errorf("failed to generate full ID: generated %d bytes but need %d", n, len(clientID))
	}
	return clientID, nil
}

func main() {
	torrent, err := parseTorrentFile("sample.torrent")
	if err != nil {
		log.Fatal(err)
	}
	// fmt.Println("Info Hash:", hex.EncodeToString(torrent.InfoHash[:]))
	// fmt.Println("Length", torrent.Length)
	clientID, err := generateClientID()
	if err != nil {
		log.Fatal(err)
	}

	port := 6881 // clients will try 6881 to 6889 before giving up.
	// fmt.Printf("Starting peer %s on port %d\n", hex.EncodeToString(peerID[:]), port)
	peers := torrent.GetPeers(clientID, port)
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
			conn, err := net.DialTimeout("tcp", peer.GetAddr(), 3*time.Second)
			if err != nil {
				log.Println(err)
				return
			}
			defer conn.Close()

			err = peer.ShakeHands(conn, torrent.InfoHash, clientID)
			if err != nil {
				log.Println(err)
				return
			}
			log.Printf("Completed handshake with peer: %s\n", peer)

			err = peer.ReadBitField(conn, len(torrent.Pieces))
			if err != nil {
				log.Println(err)
				return
			}
			log.Printf("Got Bitfield from peer: %s\n", peer)

			peer.SendUnchokeMessage(conn)
			peer.SendInterestedMessage(conn)

			for {
				request, ok := <-requests
				if !ok {
					break
				}
				if !peer.HasPiece(request.Index) {
					requests <- request
					continue
				}

				data, err := peer.DownloadPiece(conn, request.Index, request.Length, request.Hash)
				if err != nil {
					requests <- request
					continue
				}
				havePayload := make([]byte, 4)
				binary.BigEndian.PutUint32(havePayload, uint32(request.Index))
				haveMsg := Message{
					ID:      MessageHave,
					Payload: havePayload,
				}
				conn.Write(haveMsg.Serialize())
				responses <- PieceResponce{
					Index: request.Index,
					Data:  data,
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
	log.Printf("Successfully downloaded...")
}
