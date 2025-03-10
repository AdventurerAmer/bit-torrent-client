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
	"time"

	"github.com/anacrolix/utp"
	"github.com/jackpal/bencode-go"
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
			log.Println(u)
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

	func() {
		d.Torrent.MetaDataCond.L.Lock()
		defer d.Torrent.MetaDataCond.L.Unlock()
		for !d.Torrent.MetaDataSignaled {
			d.Torrent.MetaDataCond.Wait()
		}
	}()

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

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
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

	type M struct {
		UtMetaData int `bencode:"ut_metadata"`
	}

	type ExtendedHandShakeRequest struct {
		MetadataSize int `bencode:"metadata_size"`
		M            M   `bencode:"m"`
	}
	request := ExtendedHandShakeRequest{
		MetadataSize: 0,
		M: M{
			UtMetaData: 2,
		},
	}
	var requestBuf bytes.Buffer
	if err := bencode.Marshal(&requestBuf, request); err != nil {
		log.Println(err)
		return
	}
	extendedHandShake := NewExtendedMessage(0, requestBuf.Bytes())
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

			type M struct {
				UtMetaData int `bencode:"ut_metadata"`
			}

			type ExtendedHandShakeResponse struct {
				MetadataSize int `bencode:"metadata_size"`
				M            M   `bencode:"m"`
			}

			r := ExtendedHandShakeResponse{}
			if err := bencode.Unmarshal(bytes.NewReader(extended.Payload), &r); err != nil {
				log.Println(err)
				return
			}
			metaDataSize = r.MetadataSize
			utMetadataID = r.M.UtMetaData
			break loop0
		}
	}

	const PieceSize = 16384
	metadataPieces := make([][]byte, (metaDataSize+PieceSize-1)/PieceSize)
	log.Println(peer, "Meta Data Size:", metaDataSize)
	log.Println(peer, "UtMetaData:", utMetadataID)
	log.Println(peer, "Meta Data Piece Count:", len(metadataPieces))

	metaDataPieceCount := 0
	for i := 0; i < len(metadataPieces); i++ {
	loop1:
		for {
			{
				var payload bytes.Buffer
				r := struct {
					MessageType int `bencode:"msg_type"`
					PieceIndex  int `bencode:"piece"`
				}{
					MessageType: 0,
					PieceIndex:  i,
				}
				if err := bencode.Marshal(&payload, r); err != nil {
					log.Println("Marshal", err)
					continue
				}
				msg := NewExtendedMessage(byte(utMetadataID), payload.Bytes())
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
						continue
					}
				case MessageExtended:
					extended := ConvertMessageToExtended(msg)
					if extended.Extension != byte(utMetadataID) {
						break
					}
					type resp struct {
						MessageType int `bencode:"msg_type"`
						PieceIndex  int `bencode:"piece"`
					}
					r := resp{}
					if err := bencode.Unmarshal(bytes.NewReader(extended.Payload), &r); err != nil {
						log.Println("unmarshal", err)
						break
					}
					msgType := r.MessageType
					pieceIndex := r.PieceIndex
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
		log.Println("metaDataPieceCount mismatch")
		return
	}

	metaDataBuf := make([]byte, metaDataSize)
	for i := 0; i < len(metadataPieces); i++ {
		copy(metaDataBuf[i*PieceSize:], metadataPieces[i])
	}

	info := TorrentInfo{}
	if err := bencode.Unmarshal(bytes.NewReader(metaDataBuf), &info); err != nil {
		log.Println("unmarshal", err)
		return
	}

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
		log.Println("invalid torrent file: size of pieces hashes string must be divisable by 20")
		return
	}

	pieceCount := len(info.Pieces) / 20
	pieces := make([][20]byte, pieceCount)
	for i := 0; i < pieceCount; i++ {
		copy(pieces[i][:], info.Pieces[i*20:i*20+20])
	}

	log.Println(peer, "have meta data")

	func() {
		t.MetaDataCond.L.Lock()
		defer t.MetaDataCond.L.Unlock()
		if !t.MetaDataSignaled {
			t.MetaData = MetaData{
				Length:      length,
				Files:       files,
				PieceLength: info.PieceLength,
				Pieces:      pieces,
			}
			t.MetaDataSignaled = true
			t.MetaDataCond.Signal()
		}
	}()

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
