package main

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
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
	infoStr := fmt.Sprintf("d6:pieces%d:%s12:piece lengthi%de6:lengthi%de4:name%d:%se", len(piecesStr), piecesStr, pieceLength, length, len(name), name)
	hasher := sha1.New()
	infoHash := hasher.Sum([]byte(infoStr))
	t := &Torrent{
		Announce:    announce,
		Name:        name,
		Length:      length,
		PieceLength: pieceLength,
		Pieces:      pieces,
		InfoHash:    [20]byte(infoHash),
	}
	return t, nil
}

func main() {
	torrent, err := parseTorrentFile("sample.torrent")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(torrent)
	fmt.Println("InfoHash", hex.EncodeToString(torrent.InfoHash[:]))
}
