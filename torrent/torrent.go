package torrent

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/binary"
	"errors"
	"fmt"
	"log"
	"math"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"time"
)

type Torrent struct {
	AnnounceUrls []string
	Name         string
	Length       int
	InfoHash     [20]byte
	Files        []FileInfo
	PieceLength  int
	Pieces       [][20]byte
}

type FileInfo struct {
	Length int
	Path   string
}

func Parse(filepath string) (*Torrent, error) {
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

	announceUrls := []string{announce}
	announceList, ok := metainfo["announce-list"].([]any)

	if ok {
		for listIndex, list := range announceList {
			items, ok := list.([]any)
			if !ok {
				return nil, fmt.Errorf(`invalid torrent file: type mismatch at key "announce-list" at index: %d expected []any got %T`, listIndex, items)
			}
			for itemIndex, item := range items {
				strItem, ok := item.(string)
				if !ok {
					return nil, fmt.Errorf(`invalid torrent file: type mismatch at key "announce-list" at index: %d expected string got %T`, itemIndex, item)
				}
				announceUrls = append(announceUrls, strItem)
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

	files := []FileInfo{}

	length, ok := info["length"].(int)
	if ok { // single file case
		files = append(files, FileInfo{Length: length, Path: name})
	} else { // multiple files case
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

			pathList := file["path"].([]any)
			paths := make([]string, len(pathList))
			for i, item := range pathList {
				paths[i] = item.(string)
			}
			info := FileInfo{Length: fileLength, Path: path.Join(paths...)}
			files = append(files, info)
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
		return nil, errors.New("invalid torrent file: size of pieces hashes string must be divisable by 20")
	}

	pieceCount := len(piecesStr) / 20
	pieces := make([][20]byte, pieceCount)
	for i := 0; i < pieceCount; i++ {
		copy(pieces[i][:], piecesStr[i*20:i*20+20])
	}

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
		AnnounceUrls: announceUrls,
		Name:         name,
		Length:       length,
		Files:        files,
		PieceLength:  pieceLength,
		Pieces:       pieces,
		InfoHash:     infoHash,
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

func (t Torrent) FetchPeers(announceUrl url.URL, clientID [20]byte, port int, urls chan<- url.URL, results chan<- Peer) {
	if announceUrl.Scheme == "http" || announceUrl.Scheme == "https" {
		go fetchPeersHTTP(announceUrl, t, clientID, port, urls, results)
	} else if announceUrl.Scheme == "udp" {
		go fetchPeersUDP(announceUrl, t, clientID, port, urls, results)
	} else {
		log.Printf("unsupported scheme: %s\n", announceUrl.Scheme)
	}
}

func fetchPeersHTTP(u url.URL, t Torrent, clientID [20]byte, port int, urls chan<- url.URL, results chan<- Peer) {
	defer func() {
		time.Sleep(10 * time.Second)
		urls <- u
	}()
	params := url.Values{
		"info_hash":  []string{string(t.InfoHash[:])},
		"peer_id":    []string{string(clientID[:])},
		"port":       []string{strconv.Itoa(port)},
		"uploaded":   []string{"0"},
		"downloaded": []string{"0"},
		"compact":    []string{"1"},
		"left":       []string{strconv.Itoa(t.Length)},
	}
	tempUrl := u
	tempUrl.RawQuery = params.Encode()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", tempUrl.String(), nil)
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
		results <- peer
	}
}

func fetchPeersUDP(u url.URL, t Torrent, clientID [20]byte, port int, urls chan<- url.URL, results chan<- Peer) {
	defer func() {
		time.Sleep(10 * time.Second)
		urls <- u
	}()
	dialUrl := fmt.Sprintf("%s:%s", u.Hostname(), u.Port())
	conn, err := net.DialTimeout("udp", dialUrl, 10*time.Second)
	if err != nil {
		log.Println(err)
		return
	}
	defer conn.Close()

	sendRecv := func(req []byte, res []byte) {
		n := 0
		sent := false
		for !sent {
			_, err := conn.Write(req)
			if err != nil {
				log.Println(err)
				continue
			}
			func() {
				timeout := 15 * int(math.Floor(math.Pow(2.0, float64(n))))
				now := time.Now()

				conn.SetReadDeadline(now.Add(time.Duration(timeout) * time.Second))
				defer conn.SetReadDeadline(time.Time{})

				_, err = conn.Read(res)

				if err != nil {
					log.Println(err)
				} else {
					sent = true
				}
			}()
			if n < 15 {
				n++
			}
		}
	}

	// connect request response
	req := make([]byte, 16)
	transactionID := rand.Uint32()
	binary.BigEndian.PutUint64(req[:8], 0x41727101980)
	binary.BigEndian.PutUint32(req[8:12], 0)
	binary.BigEndian.PutUint32(req[12:16], uint32(transactionID))

	res := make([]byte, 16)
	sendRecv(req, res)
	action := binary.BigEndian.Uint32(res[:4])
	tid := binary.BigEndian.Uint32(res[4:8])
	connectionID := binary.BigEndian.Uint64(res[8:])
	if action != 0 {
		log.Printf("action should be 0 got %d\n", action)
		return
	}
	if tid != transactionID {
		log.Printf("transation id mismatch send %d got %d\n", transactionID, tid)
		return
	}
	log.Printf("connected to udp tracker %d\n", connectionID)

	// announce request response
	// Offset  Size    Name    Value
	// 0       64-bit integer  connection_id
	// 8       32-bit integer  action          1 // announce
	// 12      32-bit integer  transaction_id
	// 16      20-byte string  info_hash
	// 36      20-byte string  peer_id
	// 56      64-bit integer  downloaded
	// 64      64-bit integer  left
	// 72      64-bit integer  uploaded
	// 80      32-bit integer  event           0 // 0: none; 1: completed; 2: started; 3: stopped
	// 84      32-bit integer  IP address      0 // default
	// 88      32-bit integer  key
	// 92      32-bit integer  num_want        -1 // default
	// 96      16-bit integer  port
	// 98
	transactionID = rand.Uint32()
	action = 1 // announce
	var r bytes.Buffer
	binary.Write(&r, binary.BigEndian, connectionID)
	binary.Write(&r, binary.BigEndian, action)
	binary.Write(&r, binary.BigEndian, transactionID)
	r.Write(t.InfoHash[:])
	r.Write(clientID[:])
	downloaded := uint64(0)
	binary.Write(&r, binary.BigEndian, downloaded)
	left := uint64(t.Length)
	binary.Write(&r, binary.BigEndian, left)
	uploaded := uint64(0)
	binary.Write(&r, binary.BigEndian, uploaded)
	event := uint32(0)
	binary.Write(&r, binary.BigEndian, event)
	ip := uint32(0)
	binary.Write(&r, binary.BigEndian, ip)
	key := uint32(0)
	binary.Write(&r, binary.BigEndian, key)
	numWant := int32(-1)
	binary.Write(&r, binary.BigEndian, numWant)
	binary.Write(&r, binary.BigEndian, uint16(port))
	res = make([]byte, 4096)
	sendRecv(r.Bytes(), res)
	// Offset      Size            Name            Value
	// 0           32-bit integer  action          1 // announce
	// 4           32-bit integer  transaction_id
	// 8           32-bit integer  interval
	// 12          32-bit integer  leechers
	// 16          32-bit integer  seeders
	// 20 + 6 * n  32-bit integer  IP address
	// 24 + 6 * n  16-bit integer  TCP port
	// 20 + 6 * N
	action = binary.BigEndian.Uint32(res[:4])
	if action != 1 {
		log.Printf("action should be 1 got %d\n", action)
		return
	}
	tid = binary.BigEndian.Uint32(res[4:8])
	if tid != transactionID {
		log.Printf("transation id mismatch send %d got %d\n", transactionID, tid)
		return
	}
	interval := binary.BigEndian.Uint32(res[8:12])
	_ = interval
	leechers := binary.BigEndian.Uint32(res[12:16])
	_ = leechers
	seeders := binary.BigEndian.Uint32(res[16:20])
	data := res[20:]
	for i := 0; i < int(seeders); i++ {
		ip := data[i*6 : i*6+4]
		port := binary.BigEndian.Uint16(data[i*6+4 : i*6+6])
		peer := Peer{
			IP:   net.IPv4(ip[0], ip[1], ip[2], ip[3]),
			Port: port,
		}
		results <- peer
	}
}
