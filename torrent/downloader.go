package torrent

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net/url"
	"os"
	"path"
	"sync"
	"time"

	"github.com/anacrolix/utp"
	"github.com/jackpal/bencode-go"
)

type MetaDataPiece struct {
	Index int
	Data  []byte
}

type Downloader struct {
	Config  Config
	Torrent *Torrent

	ClientID [20]byte
	Port     int

	MetaDataPieceResCh         chan MetaDataPiece
	MetaDataPieceResFinishedCh chan struct{}
	MetaDataSize               int
	MetaDataPieceSize          int
	MetaDataPieceCount         int
	MetaDataSignaled           bool
	MetaDataCond               *sync.Cond

	Files  []*os.File
	Ranges map[int][]fileRange

	Done     chan struct{}
	Progress chan float64
}

func NewDownloader(config Config, torrent *Torrent) *Downloader {
	return &Downloader{
		Config:                     config,
		Torrent:                    torrent,
		ClientID:                   generateClientID(),
		Port:                       6881,
		MetaDataSignaled:           torrent.Length != 0,
		MetaDataCond:               sync.NewCond(&sync.Mutex{}),
		MetaDataPieceResFinishedCh: make(chan struct{}),
		Done:                       make(chan struct{}),
		Ranges:                     make(map[int][]fileRange),
	}
}

func (d *Downloader) Start(downloadPath string) error {
	fmt.Println("InfoHash:", hex.EncodeToString(d.Torrent.InfoHash[:]))
	fmt.Printf("Length: %d bytes\n", d.Torrent.Length)
	fmt.Printf("Starting downloader with id: %s\n", hex.EncodeToString(d.ClientID[:]))

	var (
		requests  = make(chan pieceRequest, 4096)
		responses = make(chan pieceResponce, 4096)
	)
	go func() {
		trackerURLs := make([]url.URL, 0, len(d.Torrent.AnnounceUrls))

		for _, trackerURL := range d.Torrent.AnnounceUrls {
			u, err := url.Parse(trackerURL)
			if err != nil {
				log.Printf("failed to parse tracker url '%v': %v\n", trackerURL, err)
				continue
			}
			if !isTrackerURLSupported(*u) {
				log.Printf("unsupported tracker url '%v'", trackerURL)
				continue
			}
			trackerURLs = append(trackerURLs, *u)
		}

		peerSet := make(map[string]bool)
		peersCh := make(chan Peer, 4096)
		retryPeerCh := make(chan string, 4096)

		for _, trackerURL := range trackerURLs {
			go fetchPeers(d, trackerURL, peersCh)
		}

		trackerTicker := time.NewTicker(d.Config.UpdateTrackersRate)

	loop:
		for {
			select {
			case <-trackerTicker.C:
				for _, trackerURL := range trackerURLs {
					go fetchPeers(d, trackerURL, peersCh)
				}
			case p := <-peersCh:
				key := p.GetAddr()
				ok := peerSet[key]
				if !ok {
					peerSet[key] = true
					go handlePeer(d, d.ClientID, p, requests, responses, retryPeerCh)
				}
			case r := <-retryPeerCh:
				delete(peerSet, r)
			case <-d.Done:
				break loop
			}
		}
	}()

	func() {
		d.MetaDataCond.L.Lock()
		defer d.MetaDataCond.L.Unlock()
		for !d.MetaDataSignaled {
			d.MetaDataCond.Wait()
		}
	}()

	if d.MetaDataPieceResCh != nil {
		metaDataPieces := make([][]byte, d.MetaDataPieceCount)
		metaDataPieceCount := 0
		for res := range d.MetaDataPieceResCh {
			if metaDataPieces[res.Index] == nil {
				log.Printf("Downloader Got Meta Piece #%v out of %v\n", res.Index, d.MetaDataPieceCount)
				metaDataPieces[res.Index] = res.Data
				metaDataPieceCount++
			}
			if metaDataPieceCount == d.MetaDataPieceCount {
				break
			}
		}

		close(d.MetaDataPieceResFinishedCh)

		metaDataBuf := make([]byte, d.MetaDataSize)
		for i := 0; i < len(metaDataPieces); i++ {
			copy(metaDataBuf[i*d.MetaDataPieceSize:], metaDataPieces[i])
		}

		hasher := sha1.New()
		hasher.Write(metaDataBuf)
		infoHash := hasher.Sum(nil)
		if !bytes.Equal(infoHash, d.Torrent.InfoHash[:]) {
			return errors.New("invalid magnent link")
		}

		info := TorrentInfo{}
		if err := bencode.Unmarshal(bytes.NewReader(metaDataBuf), &info); err != nil {
			log.Println("unmarshal", err)
			return err
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
			return errors.New("invalid torrent file: size of pieces hashes string must be divisable by 20")
		}

		pieceCount := len(info.Pieces) / 20
		pieces := make([][20]byte, pieceCount)
		for i := 0; i < pieceCount; i++ {
			copy(pieces[i][:], info.Pieces[i*20:i*20+20])
		}

		d.Torrent.MetaData = MetaData{
			Length:      length,
			Files:       files,
			PieceLength: info.PieceLength,
			Pieces:      pieces,
		}
		d.Torrent.Name = info.Name
	}

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

		d.Files = append(d.Files, file)
	}

	pieceIndex := 0
	pieceLength := d.Torrent.CalculatePieceLength(0)

	for _, file := range d.Files {
		stat, err := file.Stat()
		if err != nil {
			log.Fatal(err)
		}
		fileSize := int(stat.Size())
		fileOffset := 0
		for fileSize >= pieceLength {
			fg := fileRange{file: file, start: fileOffset, end: fileOffset + pieceLength}
			d.Ranges[pieceIndex] = append(d.Ranges[pieceIndex], fg)
			pieceIndex++
			fileOffset += pieceLength
			fileSize -= pieceLength
			pieceLength = d.Torrent.CalculatePieceLength(pieceIndex)
		}
		if fileSize != 0 && fileSize < pieceLength {
			fg := fileRange{file: file, start: fileOffset, end: fileOffset + fileSize}
			d.Ranges[pieceIndex] = append(d.Ranges[pieceIndex], fg)
			pieceLength -= fileSize
		}
	}

	downloadedPieces := 0

	for i := 0; i < len(d.Torrent.Pieces); i++ {
		pieceLength := d.Torrent.CalculatePieceLength(i)
		piece := make([]byte, pieceLength)
		offset := 0
		for _, r := range d.Ranges[i] {
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
				for _, r := range d.Ranges[resp.Index] {
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
		for _, file := range d.Files {
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

func generateClientID() [20]byte {
	clientID := [20]byte{}
	_, _ = rand.Read(clientID[:])
	return clientID
}

func handlePeer(d *Downloader, clientID [20]byte, peer Peer, requestsCh chan pieceRequest, responsesCh chan<- pieceResponce, retryCh chan<- string) {
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

	err = peer.shakeHands(conn, d.Config.FetchPeersTimeout, d.Torrent.InfoHash, clientID)
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
	extendedHandShake := newExtendedMessage(0, requestBuf.Bytes())
	_, err = conn.Write(extendedHandShake.Serialize())
	if err != nil {
		log.Println(err)
		return
	}
	var utMetadataID int
	var metaDataSize int
loop0:
	for {
		msg, err := readMessage(conn, d.Config.ReadMessageTimeout)
		if err != nil {
			log.Println(err)
			return
		}
		if msg == nil {
			continue
		}
		switch msg.ID {
		case MessageBitfield:
			peer.handleBitFieldMessage(msg)
		case MessageExtended:
			extended := convertMessageToExtended(msg)
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

	func() {
		d.MetaDataCond.L.Lock()
		defer d.MetaDataCond.L.Unlock()
		if !d.MetaDataSignaled {
			const PieceSize = 16384
			d.MetaDataSize = metaDataSize
			d.MetaDataPieceCount = (metaDataSize + PieceSize - 1) / PieceSize
			d.MetaDataPieceSize = PieceSize
			d.MetaDataPieceResCh = make(chan MetaDataPiece, d.MetaDataPieceCount)
			d.MetaDataSignaled = true
			d.MetaDataCond.Signal()
			log.Println("Signaling MetaData Piece Count is: ", d.MetaDataPieceCount)
		}
	}()

	if d.MetaDataPieceResCh != nil {
		pieceIndex := 0

	loop1:
		for pieceIndex < d.MetaDataPieceCount {
			should_advance := true
			log.Println(peer, "requesting metadata piece", pieceIndex)
			var reqPayload bytes.Buffer
			req := struct {
				MessageType int `bencode:"msg_type"`
				PieceIndex  int `bencode:"piece"`
			}{
				MessageType: 0,
				PieceIndex:  pieceIndex,
			}
			log.Println(peer, "Marshaling request", pieceIndex)
			if err := bencode.Marshal(&reqPayload, req); err != nil {
				log.Println(err)
				continue
			}
			reqMsg := newExtendedMessage(byte(utMetadataID), reqPayload.Bytes())
			conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			_, err := conn.Write(reqMsg.Serialize())
			if err != nil {
				log.Println(peer, "failed to write", err)
				return
			}
			conn.SetWriteDeadline(time.Time{})

			msg, err := readMessage(conn, d.Config.ReadMessageTimeout)
			if err != nil {
				log.Println(peer, "failed to read", err)
				return
			}
			if msg == nil {
				should_advance = false
				continue
			}
			switch msg.ID {
			case MessageBitfield:
				peer.handleBitFieldMessage(msg)
				should_advance = false
			case MessageExtended:
				extended := convertMessageToExtended(msg)
				if extended.Extension != byte(utMetadataID) {
					should_advance = false
					break
				}
				type Resp struct {
					MessageType int `bencode:"msg_type"`
					PieceIndex  int `bencode:"piece"`
				}
				resp := Resp{}
				if err := bencode.Unmarshal(bytes.NewReader(extended.Payload), &resp); err != nil {
					log.Println(peer, err)
					should_advance = false
					break
				}
				if resp.PieceIndex != pieceIndex {
					log.Println(peer, "wanted piece", pieceIndex, "got", resp.PieceIndex)
					should_advance = false
					break
				}

				if resp.MessageType == 1 {
					// log.Printf("Peer %v Got MetaData Piece %v\n", peer, pieceIndex)
					s := fmt.Sprintf("d8:msg_typei1e5:piecei%de10:total_sizei%dee", pieceIndex, metaDataSize)
					data := extended.Payload[len(s):]
					select {
					case <-d.MetaDataPieceResFinishedCh:
						break loop1
					case d.MetaDataPieceResCh <- MetaDataPiece{Index: pieceIndex, Data: data}:
					}

					log.Println(peer, "Sending Piece", pieceIndex)
				} else if resp.MessageType == 2 {
					log.Printf("Peer %v MetaData Piece %v got rejected", peer, pieceIndex)
				}
			}

			if should_advance {
				pieceIndex++
			}
		}
	}

	log.Println(peer, "Ready to download")

	err = peer.sendUnchokeMessage(conn)
	if err != nil {
		log.Printf("ecountered an error with peer %s: %s\n", peer, err)
		return
	}
	err = peer.sendInterestedMessage(conn)
	if err != nil {
		log.Printf("ecountered an error with peer %s: %s\n", peer, err)
		return
	}

	for {
		request, ok := <-requestsCh

		if !ok {
			break
		}

		if !peer.hasPiece(request.Index) {
			requestsCh <- request
			continue
		}

		data, err := peer.downloadPiece(conn, d, request.Index, request.Length, request.Hash)
		if err != nil {
			requestsCh <- request
			continue
		}

		haveMsg := composeHaveMessage(request.Index)
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
