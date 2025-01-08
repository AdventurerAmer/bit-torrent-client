package main

import (
	"bytes"
	"crypto/rand"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
)

// TODO: use reflection to implement Unmarshal on the `Torrent` Struct
type Torrent struct {
	Announce    string
	Name        string
	Length      int
	PieceLength int
	Pieces      [][20]byte
	InfoHash    [20]byte
}

type Peer struct {
	IP   net.IP
	Port int
}

func (t Torrent) GetPeers(peerID [20]byte, port int) ([]Peer, error) {
	base, err := url.Parse(t.Announce)
	if err != nil {
		return nil, err
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
	resp, err := http.Get(trackerURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := decodeBecoding(resp.Body)
	if err != nil {
		return nil, err
	}
	data, ok := body.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("expected a dictionary got %T", data)
	}
	peersStr, ok := data["peers"].(string)
	if !ok {
		return nil, fmt.Errorf("missing \"peers\" key in %v", data)
	}
	// assuming ipv4 here
	if len(peersStr)%6 != 0 {
		return nil, fmt.Errorf("invalid peers string: len(peers) must be divisible by 6")
	}
	peerCount := len(peersStr) / 6
	peers := make([]Peer, peerCount)
	for i := 0; i < peerCount; i++ {
		peer := []byte(peersStr[i*6:])
		peers[i] = Peer{
			IP:   net.IP(peer[:4]),
			Port: int(binary.BigEndian.Uint16(peer[4:])),
		}
	}
	return peers, nil
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
	announce := metainfo["announce"].(string)
	if announce == "" {
		return nil, errors.New("invalid torrent file: missing \"announce\" key")
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
	if !ok {
		return nil, errors.New("invalid torrent file: type mismatch value of key \"length\" in info dictionary must a int")
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
	// TODO: this only works for a torrent file that has a single file.
	// 6:lengthi%de
	// 4:name%d:%s
	// 12:piece lengthi%de
	// 6:pieces%d:%s
	infoStr := fmt.Sprintf("d6:lengthi%de4:name%d:%s12:piece lengthi%de6:pieces%d:%se", length, len(name), name, pieceLength, len(piecesStr), piecesStr)
	hasher := sha1.New()
	hasher.Write([]byte(infoStr))
	hash := hasher.Sum(nil)
	t := &Torrent{
		Announce:    announce,
		Name:        name,
		Length:      length,
		PieceLength: pieceLength,
		Pieces:      pieces,
		InfoHash:    [20]byte(hash),
	}
	return t, nil
}

func main() {
	torrent, err := parseTorrentFile("sample.torrent")
	if err != nil {
		log.Fatal(err)
	}
	peerID := [20]byte{}
	n, err := rand.Read(peerID[:])
	if err != nil {
		log.Fatal(err)
	}
	if n != len(peerID) {
		log.Fatal("failed to generate peerID")
	}
	// try 6881 to 6889.
	port := 6881
	fmt.Printf("Starting peer %s on port %d\n", hex.EncodeToString(peerID[:]), port)
	peers, err := torrent.GetPeers(peerID, port)
	if err != nil {
		log.Fatal(err)
	}
	// fmt.Println(peers)
	_ = peers
}
