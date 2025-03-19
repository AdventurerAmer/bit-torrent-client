package torrent

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/url"
	"os"
	"path"
	"sync/atomic"
	"time"

	"github.com/anacrolix/utp"
	"github.com/jackpal/bencode-go"
)

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

type MetaDataPiece struct {
	Index int
	Data  []byte
}

type Downloader struct {
	Config  Config
	Torrent *Torrent
	Logger  *log.Logger

	ClientID [20]byte
	Port     int

	MetaDataSize       int
	MetaDataPieceSize  int
	MetaDataPieceCount int

	MetaDataSizeCh     chan int
	MetaDataSizeDoneCh chan struct{}

	MetaDataPieceCh      chan MetaDataPiece
	MetaDataPiecesDoneCh chan struct{}
	GotMetaData          atomic.Bool

	Files  []*os.File
	Ranges map[int][]fileRange

	PieceRequestsCh chan pieceRequest
	PieceResponseCh chan pieceResponce

	InFlightPeers map[string]bool
	FetchPeerCh   chan string
	RetryPeerCh   chan string

	Closed   chan struct{}
	Done     chan struct{}
	Progress chan float64
}

func NewDownloader(config Config, torrent *Torrent, logger *log.Logger) *Downloader {
	d := &Downloader{
		Config:               config,
		Torrent:              torrent,
		ClientID:             generateClientID(),
		Port:                 6881,
		MetaDataSizeDoneCh:   make(chan struct{}),
		MetaDataPiecesDoneCh: make(chan struct{}),
		InFlightPeers:        make(map[string]bool),
		FetchPeerCh:          make(chan string, 8192),
		RetryPeerCh:          make(chan string, 8192),
		Closed:               make(chan struct{}),
		Done:                 make(chan struct{}),
		Ranges:               make(map[int][]fileRange),
		Logger:               logger,
	}
	if torrent.Length == 0 {
		d.MetaDataSizeCh = make(chan int)
		d.MetaDataPieceCh = make(chan MetaDataPiece)
		d.GotMetaData.Store(false)
	} else {
		close(d.MetaDataSizeDoneCh)
		close(d.MetaDataPiecesDoneCh)
		d.GotMetaData.Store(true)
	}
	return d
}

func formatByteSize(bytes int64) (float64, string) {
	units := []string{"B", "KB", "MB", "GB", "TB"}
	size := float64(bytes)
	unitIndex := 0

	for size >= 1024 && unitIndex < len(units)-1 {
		size /= 1024
		unitIndex++
	}

	return size, units[unitIndex]
}

func (d *Downloader) Start(downloadPath string) error {
	fmt.Printf("Starting downloader %s\n", hex.EncodeToString(d.ClientID[:]))
	fmt.Println("InfoHash:", hex.EncodeToString(d.Torrent.InfoHash[:]))

	if d.Torrent.Length != 0 {
		size, unit := formatByteSize(int64(d.Torrent.Length))
		fmt.Printf("Length: %.2f %s bytes\n", size, unit)
	}

	go func() {
		trackerURLs := make([]url.URL, 0, len(d.Torrent.AnnounceUrls))

		for _, trackerURL := range d.Torrent.AnnounceUrls {
			u, err := url.Parse(trackerURL)
			if err != nil {
				d.Logger.Printf("Failed to parse tracker url '%v': %v\n", trackerURL, err)
				continue
			}
			if !isTrackerURLSupported(*u) {
				d.Logger.Printf("Unsupported tracker url '%v'", trackerURL)
				continue
			}
			trackerURLs = append(trackerURLs, *u)
		}

		log.Println("Fetching peers from trackers")
		for _, trackerURL := range trackerURLs {
			go fetchPeers(d, trackerURL)
		}

		trackerTicker := time.NewTicker(d.Config.UpdateTrackersRate)
		retryPeersTicker := time.NewTicker(d.Config.RetryPeersRate)
	loop:
		for {
			select {
			case <-trackerTicker.C:
				log.Println("Fetching peers from trackers")
				for _, trackerURL := range trackerURLs {
					go fetchPeers(d, trackerURL)
				}
			case addr := <-d.FetchPeerCh:
				inflight := d.InFlightPeers[addr]
				if !inflight {
					d.InFlightPeers[addr] = true
					go handlePeer(d, addr)
				}
			case addr := <-d.RetryPeerCh:
				d.InFlightPeers[addr] = false
			case <-retryPeersTicker.C:
				for addr, inflight := range d.InFlightPeers {
					if !inflight {
						d.InFlightPeers[addr] = true
						go handlePeer(d, addr)
					}
				}
			case <-d.Done:
				break loop
			case <-d.Closed:
				break loop
			}
		}
	}()

	if d.MetaDataSizeCh != nil {
		const PieceSize = 16384

		select {
		case metaDataSize := <-d.MetaDataSizeCh:
			close(d.MetaDataSizeDoneCh)

			d.MetaDataSize = metaDataSize
			d.MetaDataPieceCount = (metaDataSize + PieceSize - 1) / PieceSize
			d.MetaDataPieceSize = PieceSize

			log.Printf("Fetching meta data info (%d) pieces\n", d.MetaDataPieceCount)

			metaDataPieces := make([][]byte, d.MetaDataPieceCount)
			metaDataPieceCount := 0
			for res := range d.MetaDataPieceCh {
				if metaDataPieces[res.Index] == nil {
					log.Printf("Fetched meta data info piece #%v\n", res.Index+1)
					metaDataPieces[res.Index] = res.Data
					metaDataPieceCount++
				}
				if metaDataPieceCount == d.MetaDataPieceCount {
					break
				}
			}

			metaDataBuf := make([]byte, d.MetaDataSize)
			for i := 0; i < len(metaDataPieces); i++ {
				copy(metaDataBuf[i*d.MetaDataPieceSize:], metaDataPieces[i])
			}

			infoHash := sha1.Sum(metaDataBuf)
			if !bytes.Equal(infoHash[:], d.Torrent.InfoHash[:]) {
				return errors.New("invalid magnent link")
			}

			info := TorrentInfo{}
			if err := bencode.Unmarshal(bytes.NewReader(metaDataBuf), &info); err != nil {
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

			d.GotMetaData.Store(true)

			log.Println("Meta data info fetched successfully")

			size, unit := formatByteSize(int64(d.Torrent.Length))
			fmt.Printf("Length: %.2f %s bytes\n", size, unit)
		case <-d.Done:
			return nil
		case <-d.Closed:
			return nil
		}
	}

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
			return err
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
	d.PieceRequestsCh = make(chan pieceRequest, len(d.Torrent.Pieces))
	d.PieceResponseCh = make(chan pieceResponce, len(d.Torrent.Pieces))

	log.Println("Queuing pieces for download")

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
					return err
				}
			}
		}
		pieceHash := sha1.Sum(piece)
		if bytes.Equal(pieceHash[:], d.Torrent.Pieces[i][:]) {
			downloadedPieces++
			continue
		}
		d.PieceRequestsCh <- pieceRequest{
			Index:  i,
			Hash:   d.Torrent.Pieces[i],
			Length: pieceLength,
		}
	}

	piecesToDownload := len(d.Torrent.Pieces) - downloadedPieces
	d.Progress = make(chan float64, piecesToDownload)

	log.Printf("Downloaded (%v) pieces before\n", downloadedPieces)

	go func(downloadedPieces int, piecesToDownload int) {
	loop:
		for i := 0; i < piecesToDownload; i++ {
			select {
			case resp := <-d.PieceResponseCh:
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
			case <-d.Closed:
				break loop
			}
		}
		close(d.Done)
		close(d.Progress)
		for _, file := range d.Files {
			file.Close()
		}
	}(downloadedPieces, piecesToDownload)

	return nil
}

func (d *Downloader) Close() {
	close(d.Closed)
}

func handlePeer(d *Downloader, addr string) {
	defer func() {
		d.RetryPeerCh <- addr
	}()

	ctx, cancel := context.WithTimeout(context.Background(), d.Config.PeerConnectionTimeout)
	defer cancel()

	peer := newPeer(addr)
	conn, err := utp.DialContext(ctx, peer.Addr)

	if err != nil {
		log.Printf("Couldn't connect to peer %s\n", peer)
		d.Logger.Printf("Failed to connect to peer %s-%s: %s\n", peer, peer.Addr, err)
		return
	}

	log.Printf("Connected to peer %v\n", peer)

	defer func() {
		log.Printf("Peer %v disconnected\n", peer)
		d.Logger.Printf("Peer %v-%v disconnected\n", peer, peer.Addr)
		conn.Close()
	}()

	err = peer.ShakeHands(conn, d.Config.FetchPeersTimeout, d.Torrent.InfoHash, d.ClientID)
	if err != nil {
		d.Logger.Printf("Ecountered an error with peer %s-%s: %s\n", peer, peer.Addr, err)
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
		d.Logger.Println(err)
		return
	}
	extendedHandShake := newExtendedMessage(0, requestBuf.Bytes())
	extendedHandShakeMsg := extendedHandShake.Serialize()

	for {
		_, err = conn.Write(extendedHandShakeMsg)
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				continue
			} else {
				d.Logger.Println(err)
				return
			}
		} else {
			break
		}
	}

	var (
		utMetadataID int
		metaDataSize int
	)

loop0:
	for {
		msg, err := readMessage(conn, d.Config.ReadMessageTimeout)
		if err != nil {
			d.Logger.Printf("Ecountered an error with peer %s-%s: %s\n", peer, peer.Addr, err)
			return
		}
		if msg == nil {
			continue
		}
		switch msg.ID {
		case MessageBitfield:
			peer.HandleBitFieldMessage(msg)
		case MessageExtended:
			extended := convertMessageToExtended(msg)
			if extended.Extension != 0 {
				d.Logger.Printf("Ecountered an error with peer %s-%s: extention must be zero\n", peer, peer.Addr)
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
				d.Logger.Printf("Ecountered an error with peer %s-%s: %s\n", peer, peer.Addr, err)
				return
			}
			metaDataSize = r.MetadataSize
			utMetadataID = r.M.UtMetaData
			break loop0
		}
	}

	if d.MetaDataSizeCh != nil {
		select {
		case <-d.MetaDataSizeDoneCh:
			break
		case d.MetaDataSizeCh <- metaDataSize:
			break
		}
	}

	if d.MetaDataPieceCh != nil {
		pieceIndex := 0

	loop1:
		for pieceIndex < d.MetaDataPieceCount && !d.GotMetaData.Load() {
			success := true

			var reqPayload bytes.Buffer
			req := struct {
				MessageType int `bencode:"msg_type"`
				PieceIndex  int `bencode:"piece"`
			}{
				MessageType: 0,
				PieceIndex:  pieceIndex,
			}

			if err := bencode.Marshal(&reqPayload, req); err != nil {
				continue
			}

			reqMsg := newExtendedMessage(byte(utMetadataID), reqPayload.Bytes()).Serialize()

			for {
				_, err := conn.Write(reqMsg)
				if err != nil {
					if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
						continue
					} else {
						d.Logger.Printf("Ecountered an error with peer %s-%s: %s\n", peer, peer.Addr, err)
						return
					}
				} else {
					break
				}
			}

			msg, err := readMessage(conn, d.Config.ReadMessageTimeout)
			if err != nil {
				d.Logger.Printf("Ecountered an error with peer %s-%s: %s\n", peer, peer.Addr, err)
				return
			}
			if msg == nil {
				success = false
				continue
			}
			switch msg.ID {
			case MessageBitfield:
				peer.HandleBitFieldMessage(msg)
				success = false
			case MessageExtended:
				extended := convertMessageToExtended(msg)
				if extended.Extension != byte(utMetadataID) {
					success = false
					break
				}
				type Resp struct {
					MessageType int `bencode:"msg_type"`
					PieceIndex  int `bencode:"piece"`
				}
				resp := Resp{}
				if err := bencode.Unmarshal(bytes.NewReader(extended.Payload), &resp); err != nil {
					success = false
					break
				}
				if resp.PieceIndex != pieceIndex {
					success = false
					break
				}

				if resp.MessageType == 1 {
					s := fmt.Sprintf("d8:msg_typei1e5:piecei%de10:total_sizei%dee", pieceIndex, metaDataSize)
					data := extended.Payload[len(s):]
					select {
					case <-d.MetaDataPiecesDoneCh:
						break loop1
					case d.MetaDataPieceCh <- MetaDataPiece{Index: pieceIndex, Data: data}:
					}
				} else if resp.MessageType == 2 {
					d.Logger.Printf("Peer %v-%v meta data info Piece #%v got rejected", peer, peer.Addr, pieceIndex)
				}
			}

			if success {
				pieceIndex++
			}
		}
	}

	log.Printf("Peer %v is ready\n", peer)

	err = peer.SendUnchokeMessage(conn)
	if err != nil {
		d.Logger.Printf("Ecountered an error with peer %s-%s: %s\n", peer, peer.Addr, err)
		return
	}

	err = peer.SendInterestedMessage(conn)
	if err != nil {
		d.Logger.Printf("Ecountered an error with peer %s-%s: %s\n", peer, peer.Addr, err)
		return
	}

loop2:
	for {
		select {
		case request, ok := <-d.PieceRequestsCh:
			if !ok {
				break loop2
			}

			if !peer.HasPiece(request.Index) {
				d.PieceRequestsCh <- request
				continue
			}

			data, err := peer.DownloadPiece(conn, d.Config.ReadMessageTimeout, request.Index, request.Length, request.Hash, d.Logger)
			if err != nil {
				d.Logger.Printf("Ecountered an error with peer %s-%s: %s\n", peer, peer.Addr, err)
				d.PieceRequestsCh <- request
				continue
			}

			haveMsg := composeHaveMessage(request.Index).Serialize()
			for {
				_, err = conn.Write(haveMsg)
				if err != nil {
					if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
						continue
					} else {
						d.Logger.Printf("Ecountered an error with peer %s-%s: %s\n", peer, peer.Addr, err)
						d.PieceRequestsCh <- request
					}
				} else {
					break
				}
			}
			d.PieceResponseCh <- pieceResponce{
				Index: request.Index,
				Data:  data,
			}
		case <-d.Done:
			break loop2
		}
	}
}
