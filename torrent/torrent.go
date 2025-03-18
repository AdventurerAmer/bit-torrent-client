package torrent

import (
	"bytes"
	"crypto/rand"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path"
	"strings"

	bencode "github.com/jackpal/bencode-go"
)

type TorrentFileInfo struct {
	Length int      `bencode:"length"`
	Path   []string `bencode:"path"`
}

type TorrentInfo struct {
	Length      int               `bencode:"length,omitempty"`
	Name        string            `bencode:"name"`
	Files       []TorrentFileInfo `bencode:"files,omitempty"`
	PieceLength int               `bencode:"piece length"`
	Pieces      string            `bencode:"pieces"`
}

type TorrentMetaInfo struct {
	Announce     string      `bencode:"announce"`
	AnnounceList [][]string  `bencode:"announce-list"`
	Info         TorrentInfo `bencode:"info"`
}

type FileInfo struct {
	Length int
	Path   string
}

type MetaData struct {
	Length      int
	Files       []FileInfo
	PieceLength int
	Pieces      [][20]byte
}

type Torrent struct {
	MetaData
	AnnounceUrls []string
	Name         string
	InfoHash     [20]byte
}

func ParseFile(filename string) (*Torrent, error) {
	p, err := os.ReadFile(filename)
	if err != nil {
		return nil, err
	}

	metaInfo := TorrentMetaInfo{}
	err = bencode.Unmarshal(bytes.NewReader(p), &metaInfo)
	if err != nil {
		return nil, err
	}

	announceUrls := []string{metaInfo.Announce}
	for _, tierUrls := range metaInfo.AnnounceList {
		announceUrls = append(announceUrls, tierUrls...)
	}

	info := metaInfo.Info

	length := 0
	var files []FileInfo

	if info.Length != 0 && len(info.Files) == 0 { // torrent file contains only one file
		length = info.Length
		files = []FileInfo{{Length: length, Path: info.Name}}
	} else {
		for _, file := range info.Files {
			length += file.Length
			fileInfo := FileInfo{Length: file.Length, Path: path.Join(file.Path...)}
			files = append(files, fileInfo)
		}
	}

	if len(info.Pieces)%20 != 0 {
		return nil, errors.New("invalid torrent file: size of pieces hashes string must be divisable by 20")
	}

	pieceCount := len(info.Pieces) / 20
	pieces := make([][20]byte, pieceCount)
	for i := 0; i < pieceCount; i++ {
		copy(pieces[i][:], info.Pieces[i*20:i*20+20])
	}

	infoLiteral := []byte("4:info")
	infoIdx := bytes.Index(p, infoLiteral)
	infoData, err := bencode.Decode(bytes.NewReader(p[infoIdx+len(infoLiteral):]))
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	err = bencode.Marshal(&b, infoData)
	if err != nil {
		return nil, err
	}

	infoHash := sha1.Sum(b.Bytes())
	t := &Torrent{
		AnnounceUrls: announceUrls,
		Name:         info.Name,
		MetaData: MetaData{
			Length:      length,
			Files:       files,
			PieceLength: info.PieceLength,
			Pieces:      pieces,
		},
		InfoHash: [20]byte(infoHash),
	}
	return t, nil
}

func ParseMagnet(magnet string) (*Torrent, error) {
	u, err := url.Parse(magnet)
	if err != nil {
		return nil, err
	}
	query := u.Query()

	xt := query.Get("xt")
	if xt == "" {
		return nil, errors.New("invalid magnet link: missing xt")
	}
	parts := strings.Split(xt, ":")

	if len(parts) != 3 {
		return nil, fmt.Errorf("invalid magnet link: unsupported extact topic %v", xt)
	}
	if parts[0] != "urn" {
		return nil, fmt.Errorf("invalid magnet link: unsupported extact topic %v", xt)
	}

	if parts[1] != "btih" && parts[1] != "btmh" {
		return nil, fmt.Errorf("invalid magnet link: unsupported extact topic %v", xt)
	}
	infoHash, err := hex.DecodeString(parts[2])
	if err != nil {
		return nil, err
	}
	name := query.Get("dn")
	if name == "" {
		return nil, errors.New("invalid magnet link: missing name (dn)")
	}
	trackers := query["tr"]
	if len(trackers) == 0 {
		return nil, errors.New("invalid magnet link: missing trackers (tr)")
	}
	t := &Torrent{
		AnnounceUrls: trackers,
		Name:         name,
		InfoHash:     [20]byte(infoHash),
	}
	return t, nil
}

func (t Torrent) CalculatePieceLength(index int) int {
	start := index * t.PieceLength
	end := start + t.PieceLength
	if end > t.Length {
		end = t.Length
	}
	return end - start
}

func generateClientID() [20]byte {
	clientID := [20]byte{}
	_, _ = rand.Read(clientID[:])
	return clientID
}
