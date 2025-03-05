package torrent

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net/url"
	"os"
	"path"
	"strings"
	"time"

	"github.com/anacrolix/utp"
)

type Downloader struct {
	Torrent  *Torrent
	Done     chan struct{}
	Progress chan float64
	files    []*os.File
	ranges   map[int][]fileRange
}

func NewDownloader(torrent *Torrent) *Downloader {
	return &Downloader{Torrent: torrent, Done: make(chan struct{}), ranges: make(map[int][]fileRange)}
}

func (d *Downloader) Download(downloadPath string) error {
	fmt.Println("InfoHash:", hex.EncodeToString(d.Torrent.InfoHash[:]))
	fmt.Printf("Length: %d bytes\n", d.Torrent.Length)

	clientID, err := generateClientID()
	if err != nil {
		return err
	}
	fmt.Printf("Starting downloader with id: %s\n", hex.EncodeToString(clientID[:]))

	var (
		requests  = make(chan pieceRequest, 4096)
		responses = make(chan pieceResponce, 4096)
	)
	go func() {
		peerSet := make(map[string]bool)
		peersCh := make(chan Peer, 4096)
		urlsCh := make(chan url.URL, len(d.Torrent.AnnounceUrls))
		retryPeerCh := make(chan string, 4096)
		for _, u := range d.Torrent.AnnounceUrls {
			base, err := url.Parse(u)
			if err != nil {
				log.Println(err)
				continue
			}
			urlsCh <- *base
		}
		port := 6881 // clients will try to listen on ports 6881 to 6889 before giving up.
	loop:
		for {
			select {
			case u := <-urlsCh:
				d.Torrent.FetchPeers(u, clientID, port, urlsCh, peersCh)
			case p := <-peersCh:
				key := p.GetAddr()
				ok := peerSet[key]
				if !ok {
					peerSet[key] = true
					go handlePeer(d.Torrent, clientID, p, requests, responses, retryPeerCh)
				}
			case r := <-retryPeerCh:
				delete(peerSet, r)
			case <-d.Done:
				break loop
			}
		}
	}()

	md := <-d.Torrent.InfoCh
	d.Torrent.InfoCh = nil
	d.Torrent.MetaData = md

	log.Println("Got Info...")

	for _, info := range d.Torrent.Files {
		filePath := path.Join(downloadPath, info.Path)
		err := os.MkdirAll(path.Dir(filePath), os.ModePerm)
		if err != nil {
			return err
		}

		file, err := os.OpenFile(filePath, os.O_CREATE, os.ModePerm)
		if err != nil {
			return err
		}

		stat, err := file.Stat()
		if err != nil {
			return err
		}

		if stat.Size() != int64(info.Length) {
			err = file.Truncate(int64(info.Length))
			if err != nil {
				return err
			}
		}

		d.files = append(d.files, file)
	}

	pieceIndex := 0
	pieceLength := d.Torrent.CalculatePieceLength(0)

	for _, file := range d.files {
		stat, err := file.Stat()
		if err != nil {
			log.Fatal(err)
		}
		fileSize := int(stat.Size())
		fileOffset := 0
		for fileSize >= pieceLength {
			fg := fileRange{file: file, start: fileOffset, end: fileOffset + pieceLength}
			d.ranges[pieceIndex] = append(d.ranges[pieceIndex], fg)
			pieceIndex++
			fileOffset += pieceLength
			fileSize -= pieceLength
			pieceLength = d.Torrent.CalculatePieceLength(pieceIndex)
		}
		if fileSize != 0 && fileSize < pieceLength {
			fg := fileRange{file: file, start: fileOffset, end: fileOffset + fileSize}
			d.ranges[pieceIndex] = append(d.ranges[pieceIndex], fg)
			pieceLength -= fileSize
		}
	}

	downloadedPieces := 0

	for i := 0; i < len(d.Torrent.Pieces); i++ {
		pieceLength := d.Torrent.CalculatePieceLength(i)
		piece := make([]byte, pieceLength)
		offset := 0
		for _, r := range d.ranges[i] {
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
		if bytes.Equal(pieceHash[:], d.Torrent.Pieces[i][:]) {
			log.Printf("piece #%d is checkedout\n", i)
			downloadedPieces++
			continue
		}
		requests <- pieceRequest{
			Index:  i,
			Hash:   d.Torrent.Pieces[i],
			Length: pieceLength,
		}
		log.Printf("queuing piece #%d\n", i)
	}

	piecesToDownload := len(d.Torrent.Pieces) - downloadedPieces
	d.Progress = make(chan float64, piecesToDownload)

	go func(downloadedPieces int, piecesToDownload int) {
	loop:
		for i := 0; i < piecesToDownload; i++ {
			select {
			case resp := <-responses:
				offset := 0
				for _, r := range d.ranges[resp.Index] {
					rangeLength := r.end - r.start
					_, err := r.file.WriteAt(resp.Data[offset:offset+rangeLength], int64(r.start))
					offset += rangeLength
					if err != nil {
						if err != io.EOF {
							log.Fatal(err)
						}
					}
				}
				downloadedPieces++
				d.Progress <- float64(downloadedPieces) / float64(len(d.Torrent.Pieces)) * 100.0
			case <-d.Done:
				break loop
			}
		}
		close(requests)
		close(responses)
		close(d.Done)
		close(d.Progress)
		for _, file := range d.files {
			file.Close()
		}
	}(downloadedPieces, piecesToDownload)

	return nil
}

type fileRange struct {
	file  *os.File
	start int
	end   int
}

type pieceRequest struct {
	Index  int
	Hash   [20]byte
	Length int
}

type pieceResponce struct {
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

func handlePeer(t *Torrent, clientID [20]byte, peer Peer, requestsCh chan pieceRequest, responsesCh chan<- pieceResponce, retryCh chan<- string) {
	defer func() {
		retryCh <- peer.GetAddr()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	conn, err := utp.DialContext(ctx, peer.GetAddr())

	if err != nil {
		log.Printf("failed to connect to peer %s: %s\n", peer, err)
		return
	}
	defer func() {
		conn.Close()
		log.Printf("peer %s disconnected", peer)
	}()

	err = peer.ShakeHands(conn, t.InfoHash, clientID)
	if err != nil {
		log.Printf("ecountered an error with peer %s: %s\n", peer, err)
		return
	}
	request := "d1:md11:ut_metadatai2ee13:metadata_sizei0ee"
	extendedHandShake := NewExtendedMessage(0, []byte(request))
	_, err = conn.Write(extendedHandShake.Serialize())
	if err != nil {
		return
	}
	var utMetadataID int
	var metaDataSize int
loop0:
	for {
		msg, err := ReadMessage(conn)
		if err != nil {
			log.Println(err)
			return
		}
		if msg == nil {
			continue
		}
		switch msg.ID {
		case MessageBitfield:
			err = peer.HandleBitFieldMessage(msg, len(t.Pieces))
			if err != nil {
				log.Printf("ecountered an error with peer %s: %s\n", peer, err)
				return
			}
		case MessageExtended:
			extended := ConvertMessageToExtended(msg)
			if extended.Extension != 0 {
				log.Printf("ecountered an error with peer %s: extention must be zero\n", peer)
				return
			}
			shake, err := decodeBecoding(strings.NewReader(string(extended.Payload)))
			if err != nil {
				return
			}
			root, ok := shake.(map[string]any)
			if !ok {
				return
			}
			metaDataSize, ok = root["metadata_size"].(int)
			if !ok {
				return
			}
			m, ok := root["m"].(map[string]any)
			if !ok {
				return
			}
			utMetadataID, ok = m["ut_metadata"].(int)
			if !ok {
				return
			}
			break loop0
		}
	}

	const PieceSize = 16384
	metadataPieces := make([][]byte, (metaDataSize+PieceSize-1)/PieceSize)
	log.Println(peer, "Meta Data Piece Count:", len(metadataPieces))

	metaDataPieceCount := 0
	for i := 0; i < len(metadataPieces); i++ {
	loop1:
		for {
			{
				payload := fmt.Sprintf("d8:msg_typei0e5:piecei%dee", i)
				msg := NewExtendedMessage(byte(utMetadataID), []byte(payload))
				_, err := conn.Write(msg.Serialize())
				if err != nil {
					log.Println(err)
					continue
				}
			}
			{
				msg, err := ReadMessage(conn)
				if err != nil {
					log.Println(err)
					return
				}
				if msg == nil {
					continue
				}
				switch msg.ID {
				case MessageBitfield:
					err = peer.HandleBitFieldMessage(msg, len(t.Pieces))
					if err != nil {
						log.Printf("ecountered an error with peer %s: %s\n", peer, err)
						return
					}
				case MessageExtended:
					extended := ConvertMessageToExtended(msg)
					if extended.Extension != byte(utMetadataID) {
						break
					}
					data, err := decodeBecoding(strings.NewReader(string(extended.Payload)))
					if err != nil {
						log.Printf("ecountered an error with peer %s: %s\n", peer, err)
						break
					}
					m, ok := data.(map[string]any)
					if !ok {
						break
					}
					msgType, ok := m["msg_type"].(int)
					if !ok {
						log.Println("msg_type not found")
						break
					}
					pieceIndex, ok := m["piece"].(int)
					if !ok {
						log.Println("piece not found")
						break
					}
					if pieceIndex != i {
						break
					}
					if msgType == 1 {
						log.Printf("Peer %v Got MetaData Piece %v\n", peer, pieceIndex)
						s := fmt.Sprintf("d8:msg_typei1e5:piecei%de10:total_sizei%dee", pieceIndex, metaDataSize)
						pieceData := extended.Payload[len(s):]
						metadataPieces[pieceIndex] = pieceData
						metaDataPieceCount++
					} else if msgType == 2 {
						log.Printf("Peer %v MetaData Piece %v got rejected", peer, pieceIndex)
					}
					break loop1
				}
			}
		}
	}

	if metaDataPieceCount != len(metadataPieces) {
		return
	}
	metaData := make([]byte, metaDataSize)
	for i := 0; i < len(metadataPieces); i++ {
		copy(metaData[i*PieceSize:], metadataPieces[i])
	}
	m, err := decodeBecoding(strings.NewReader(string(metaData)))
	if err != nil {
		log.Println(err)
		return
	}

	info := m.(map[string]any)
	name, ok := info["name"].(string)
	if !ok {
		log.Println("name is not found...")
		return
	}

	files := []FileInfo{}

	length, ok := info["length"].(int)
	if ok { // single file case
		files = append(files, FileInfo{Length: length, Path: name})
	} else { // multiple files case
		filesList, ok := info["files"].([]any)
		if !ok {
			log.Println(`invalid torrent file: missing both "length" and "files" keys info dictionary`)
		}
		for idx, fileMap := range filesList {
			file, ok := fileMap.(map[string]any)
			if !ok {
				log.Printf(`invalid torrent file: file number %d must be of type map[string]any`, idx)
			}
			fileLength, ok := file["length"].(int)
			if !ok {
				log.Printf(`invalid torrent file: "length" key of file number %d must be an int`, idx)
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
		log.Println("invalid torrent file: type mismatch value of key \"piece length\" in info dictionary must a int")
		return
	}

	piecesStr, ok := info["pieces"].(string)
	if !ok {
		log.Println("invalid torrent file: type mismatch value of key \"pieces\" in info dictionary must a string")
		return
	}

	if len(piecesStr)%20 != 0 {
		log.Println("invalid torrent file: size of pieces hashes string must be divisable by 20")
		return
	}

	pieceCount := len(piecesStr) / 20
	pieces := make([][20]byte, pieceCount)
	for i := 0; i < pieceCount; i++ {
		copy(pieces[i][:], piecesStr[i*20:i*20+20])
	}
	md := MetaData{
		Length:      length,
		Files:       files,
		PieceLength: pieceLength,
		Pieces:      pieces,
	}

	select {
	case t.InfoCh <- md:
		log.Println(peer, "Sending Meta Info")
	case <-t.InfoCh:
		log.Println(peer, "Already Read")
	}
	log.Println(peer, "Ready to download")

	err = peer.SendUnchokeMessage(conn)
	if err != nil {
		log.Printf("ecountered an error with peer %s: %s\n", peer, err)
		return
	}
	err = peer.SendInterestedMessage(conn)
	if err != nil {
		log.Printf("ecountered an error with peer %s: %s\n", peer, err)
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
		responsesCh <- pieceResponce{
			Index: request.Index,
			Data:  data,
		}
	}
}
