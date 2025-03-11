package torrent

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
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

	bencode "github.com/jackpal/bencode-go"
)

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

func ParseFile(filepath string) (*Torrent, error) {
	p, err := os.ReadFile(filepath)
	if err != nil {
		return nil, err
	}

	metaInfo := TorrentMetaInfo{}
	err = bencode.Unmarshal(bytes.NewReader(p), &metaInfo)
	if err != nil {
		return nil, err
	}

	announceUrls := []string{metaInfo.Announce}
	for _, tier := range metaInfo.AnnounceList {
		announceUrls = append(announceUrls, tier...)
	}

	info := metaInfo.Info

	length := 0
	var files []FileInfo

	if info.Length != 0 && len(info.Files) == 0 {
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

	hasher := sha1.New()
	hasher.Write(b.Bytes())
	hash := hasher.Sum(nil)
	infoHash := [20]byte(hash)

	t := &Torrent{
		AnnounceUrls: announceUrls,
		Name:         info.Name,
		MetaData: MetaData{
			Length:      length,
			Files:       files,
			PieceLength: info.PieceLength,
			Pieces:      pieces,
		},
		InfoHash: infoHash,
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
	log.Println("infoHash", string(infoHash))
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
	log.Printf("torrent: %v", t)
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
		log.Printf("unsupported scheme %v: %v\n", announceUrl.Scheme, announceUrl)
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

	r := struct {
		Peers string `bencode:"peers"`
	}{}

	if err := bencode.Unmarshal(resp.Body, &r); err != nil {
		log.Println(err)
		return
	}

	peersStr := r.Peers
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
	res = make([]byte, 8096)
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
