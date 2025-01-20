package main

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
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

func (t Torrent) GetPeers(clientID [20]byte, port int) []Peer {
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
				"peer_id":    []string{string(clientID[:])},
				"port":       []string{strconv.Itoa(port)},
				"uploaded":   []string{"0"},
				"downloaded": []string{"0"},
				"compact":    []string{"1"},
				"left":       []string{strconv.Itoa(t.Length)},
			}
			base.RawQuery = params.Encode()
			trackerURL := base.String()
			if base.Scheme == "http" || base.Scheme == "https" {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
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
			} else if base.Scheme == "udp" {
				base, err := url.Parse(announceUrl)
				if err != nil {
					log.Println(err)
					return
				}
				dialUrl := fmt.Sprintf("%s:%s", base.Hostname(), base.Port())
				serverAddr, err := net.ResolveUDPAddr("udp", dialUrl)
				if err != nil {
					log.Println(err)
					return
				}
				localAddr := &net.UDPAddr{
					IP:   net.IPv4zero,
					Port: 0,
				}
				conn, err := net.DialUDP("udp", localAddr, serverAddr)
				if err != nil {
					log.Println(err)
					return
				}
				defer conn.Close()
				sendRecv := func(req []byte, resp []byte, timeout time.Duration) bool {
					// n := 0
					// waitTime := 15 * int(math.Floor(math.Pow(2.0, float64(n))))
					_, err := conn.Write(req)
					if err != nil {
						log.Println(err)
						return false
					}
					conn.SetReadDeadline(time.Now().Add(timeout))
					defer conn.SetReadDeadline(time.Time{})
					_, err = conn.Read(resp)
					if err != nil {
						log.Println(err)
						return false
					}
					return true
				}
				// connect request response
				req := make([]byte, 16)
				transactionID := rand.Uint32()
				binary.BigEndian.PutUint64(req[:8], 0x41727101980)
				binary.BigEndian.PutUint32(req[8:12], 0)
				binary.BigEndian.PutUint32(req[12:16], uint32(transactionID))

				res := make([]byte, 16)
				recived := sendRecv(req, res, 5*time.Second)
				if !recived {
					log.Println("timeout")
					return
				}
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
				// connect request response
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
				sendRecv(r.Bytes(), res, 5*time.Second)
				if !recived {
					log.Println("timeout")
					return
				}
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
					peerStr := fmt.Sprintf("%s:%d", peer.IP, peer.Port)
					peerSet[peerStr] = peer
					log.Printf("Got Peer %s from Tracker %d\n", peer.String(), connectionID)
				}
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

func handleClientPeer(torrent *Torrent, clientID [20]byte, peer Peer, requestsCh chan PieceRequest, responsesCh chan<- PieceResponce, retryCh chan<- string) {
	defer func() {
		retryCh <- peer.GetAddr()
	}()
	log.Printf("trying to connect to peer: %s", peer)
	conn, err := net.DialTimeout("tcp", peer.GetAddr(), 5*time.Second)
	if err != nil {
		log.Printf("ecountered an error: %s with peer: %s\n", err, peer)
		return
	}
	defer func() {
		conn.Close()
		log.Printf("peer: %s disconnected", peer)
	}()

	err = peer.ShakeHands(conn, torrent.InfoHash, clientID)
	if err != nil {
		log.Printf("ecountered an error: %s with peer: %s\n", err, peer)
		return
	}
	log.Printf("Completed handshake with peer: %s\n", peer)

	err = peer.ReadBitField(conn, len(torrent.Pieces))
	if err != nil {
		log.Printf("ecountered an error: %s with peer: %s\n", err, peer)
		return
	}
	log.Printf("Got Bitfield from peer: %s\n", peer)

	err = peer.SendUnchokeMessage(conn)
	if err != nil {
		log.Printf("ecountered an error: %s with peer: %s\n", err, peer)
		return
	}
	err = peer.SendInterestedMessage(conn)
	if err != nil {
		log.Printf("ecountered an error: %s with peer: %s\n", err, peer)
		return
	}
	for {
		request, ok := <-requestsCh
		if !ok {
			break
		}
		if !peer.HasPiece(request.Index) {
			requestsCh <- request
			continue
		}
		data, err := peer.DownloadPiece(conn, request.Index, request.Length, request.Hash)
		if err != nil {
			// log.Println(err)
			requestsCh <- request
			continue
		}

		haveMsg := ComposeHaveMessage(request.Index)
		_, err = conn.Write(haveMsg.Serialize())
		if err != nil {
			log.Println(err)
			requestsCh <- request
			continue
		}
		responsesCh <- PieceResponce{
			Index: request.Index,
			Data:  data,
		}
	}
}

func main() {
	filePath := flag.String("file", "", "path of torrent file")
	downloadPath := flag.String("path", ".", "download path")
	flag.Parse()

	err := os.MkdirAll(*downloadPath, os.ModeDir)
	if err != nil {
		log.Fatal(err)
	}

	torrent, err := parseTorrentFile(*filePath)
	if err != nil {
		log.Fatal(err)
	}

	files := []*os.File{}
	defer func() {
		for _, f := range files {
			f.Close()
		}
	}()

	if len(torrent.Files) == 0 {
		filePath := path.Join(*downloadPath, torrent.Name)
		file, err := os.OpenFile(filePath, os.O_CREATE, os.ModePerm)
		if err != nil {
			log.Fatal(err)
		}
		stat, err := file.Stat()
		if err != nil {
			log.Fatal(err)
		}
		if stat.Size() != int64(torrent.Length) {
			err = file.Truncate(int64(torrent.Length))
			if err != nil {
				log.Fatal(err)
			}
		}
		files = append(files, file)
	} else {
		for _, fileEntry := range torrent.Files {
			fileLength := fileEntry["length"].(int)
			pathList := fileEntry["path"].([]any)
			paths := make([]string, len(pathList))
			for i, item := range pathList {
				paths[i] = item.(string)
			}
			filePath := path.Join(*downloadPath, path.Join(paths...))
			file, err := os.OpenFile(filePath, os.O_CREATE, os.ModePerm)
			if err != nil {
				log.Fatal(err)
			}
			stat, err := file.Stat()
			if err != nil {
				log.Fatal(err)
			}
			if stat.Size() != int64(fileLength) {
				err = file.Truncate(int64(fileLength))
				if err != nil {
					log.Fatal(err)
				}
			}
			files = append(files, file)
		}
	}
	fmt.Println("Info Hash:", hex.EncodeToString(torrent.InfoHash[:]))
	fmt.Println("Length:", torrent.Length)
	clientID, err := generateClientID()
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("Starting client %s\n", hex.EncodeToString(clientID[:]))

	requests := make(chan PieceRequest, len(torrent.Pieces))
	responses := make(chan PieceResponce, len(torrent.Pieces))
	type FileRange struct {
		file  *os.File
		start int
		end   int
	}

	calculatePieceLength := func(pieceIndex int) int {
		pieceStart := pieceIndex * torrent.PieceLength
		pieceEnd := pieceStart + torrent.PieceLength
		if pieceEnd > torrent.Length {
			pieceEnd = torrent.Length
		}
		return pieceEnd - pieceStart
	}

	ranges := make(map[int][]FileRange)
	pieceIndex := 0
	pieceLength := calculatePieceLength(0)

	for _, file := range files {
		stat, err := file.Stat()
		if err != nil {
			log.Fatal(err)
		}
		fileSize := int(stat.Size())
		fileOffset := 0
		for fileSize >= pieceLength {
			fg := FileRange{file: file, start: fileOffset, end: fileOffset + pieceLength}
			ranges[pieceIndex] = append(ranges[pieceIndex], fg)
			pieceIndex++
			fileOffset += pieceLength
			fileSize -= pieceLength
			pieceLength = calculatePieceLength(pieceIndex)
		}
		if fileSize != 0 && fileSize < pieceLength {
			fg := FileRange{file: file, start: fileOffset, end: fileOffset + fileSize}
			ranges[pieceIndex] = append(ranges[pieceIndex], fg)
			pieceLength -= fileSize
		}
	}

	downloadedPieces := 0

	for i := 0; i < len(torrent.Pieces); i++ {
		pieceLength := calculatePieceLength(i)
		piece := make([]byte, pieceLength)
		offset := 0
		for _, r := range ranges[i] {
			rangeLength := r.end - r.start
			_, err := r.file.ReadAt(piece[offset:offset+rangeLength], int64(r.start))
			offset += rangeLength
			if err != nil {
				if err != io.EOF {
					log.Fatal(err)
				}
			}
		}
		pieceHash := sha1.Sum(piece)
		if bytes.Equal(pieceHash[:], torrent.Pieces[i][:]) {
			log.Printf("We have Piece %d\n", i)
			downloadedPieces++
			continue
		}
		requests <- PieceRequest{
			Index:  i,
			Hash:   torrent.Pieces[i],
			Length: pieceLength,
		}
	}

	done := make(chan struct{})

	go func() {
		peerSet := make(map[string]bool)
		retryCh := make(chan string, 4096)
		port := 6881 // clients will try to listen on ports 6881 to 6889 before giving up.
		ticker := time.NewTicker(time.Second * 10)
	loop:
		for {
			select {
			case t := <-ticker.C:
				log.Printf("checking for peers %v", t)
				availablePeers := torrent.GetPeers(clientID, port)
				for _, peer := range availablePeers {
					ok := peerSet[peer.GetAddr()]
					if !ok {
						peerSet[peer.GetAddr()] = true
						go handleClientPeer(torrent, clientID, peer, requests, responses, retryCh)
					}
				}
			case r := <-retryCh:
				log.Printf("peer %s added to retry queue\n", r)
				delete(peerSet, r)
			case <-done:
				log.Printf("done")
				break loop
			}
		}
	}()

	downloadCount := len(torrent.Pieces) - downloadedPieces
	for i := 0; i < downloadCount; i++ {
		resp := <-responses
		downloadedPieces++
		progress := float64(downloadedPieces) / float64(len(torrent.Pieces)) * 100
		offset := 0
		for _, r := range ranges[resp.Index] {
			rangeLength := r.end - r.start
			_, err := r.file.WriteAt(resp.Data[offset:offset+rangeLength], int64(r.start))
			offset += rangeLength
			if err != nil {
				if err != io.EOF {
					log.Fatal(err)
				}
			}
		}
		log.Printf("Downloaded Piece %d successfully progress %.2f%%", resp.Index, progress)
	}
	close(requests)
	close(responses)
	close(done)
	log.Printf("Successfully downloaded...")
}
