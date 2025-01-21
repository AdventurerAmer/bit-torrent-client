package torrent

import (
	"bytes"
	"crypto/rand"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net"
	"net/url"
	"os"
	"path"
	"time"
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

	requests := make(chan pieceRequest, len(d.Torrent.Pieces))
	responses := make(chan pieceResponce, len(d.Torrent.Pieces))
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
	conn, err := net.DialTimeout("tcp", peer.GetAddr(), 10*time.Second)
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

	err = peer.ReadBitField(conn, len(t.Pieces))
	if err != nil {
		log.Printf("ecountered an error with peer %s: %s\n", peer, err)
		return
	}

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
