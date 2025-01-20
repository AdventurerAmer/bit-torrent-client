package main

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"net/url"
	"os"
	"path"
	"time"

	"github.com/harlequingg/bit-torrent-client/torrent"
)

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

func handleClientPeer(t *torrent.Torrent, clientID [20]byte, peer torrent.Peer, requestsCh chan PieceRequest, responsesCh chan<- PieceResponce, retryCh chan<- string) {
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

	err = peer.ShakeHands(conn, t.InfoHash, clientID)
	if err != nil {
		log.Printf("ecountered an error: %s with peer: %s\n", err, peer)
		return
	}
	log.Printf("Completed handshake with peer: %s\n", peer)

	err = peer.ReadBitField(conn, len(t.Pieces))
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

		haveMsg := torrent.ComposeHaveMessage(request.Index)
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

	t, err := torrent.Parse(*filePath)
	if err != nil {
		log.Fatal(err)
	}

	files := []*os.File{}
	defer func() {
		for _, f := range files {
			f.Close()
		}
	}()

	for _, info := range t.Files {
		filePath := path.Join(*downloadPath, info.Path)
		file, err := os.OpenFile(filePath, os.O_CREATE, os.ModePerm)
		if err != nil {
			log.Fatal(err)
		}
		stat, err := file.Stat()
		if err != nil {
			log.Fatal(err)
		}
		if stat.Size() != int64(info.Length) {
			err = file.Truncate(int64(info.Length))
			if err != nil {
				log.Fatal(err)
			}
		}
		files = append(files, file)
	}

	fmt.Println("Info Hash:", hex.EncodeToString(t.InfoHash[:]))
	fmt.Println("Length:", t.Length)
	clientID, err := generateClientID()
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("Starting client %s\n", hex.EncodeToString(clientID[:]))

	requests := make(chan PieceRequest, len(t.Pieces))
	responses := make(chan PieceResponce, len(t.Pieces))

	type FileRange struct {
		file  *os.File
		start int
		end   int
	}

	ranges := make(map[int][]FileRange)
	pieceIndex := 0
	pieceLength := t.CalculatePieceLength(0)

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
			pieceLength = t.CalculatePieceLength(pieceIndex)
		}
		if fileSize != 0 && fileSize < pieceLength {
			fg := FileRange{file: file, start: fileOffset, end: fileOffset + fileSize}
			ranges[pieceIndex] = append(ranges[pieceIndex], fg)
			pieceLength -= fileSize
		}
	}

	downloadedPieces := 0

	for i := 0; i < len(t.Pieces); i++ {
		pieceLength := t.CalculatePieceLength(i)
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
		if bytes.Equal(pieceHash[:], t.Pieces[i][:]) {
			log.Printf("We have Piece %d\n", i)
			downloadedPieces++
			continue
		}
		requests <- PieceRequest{
			Index:  i,
			Hash:   t.Pieces[i],
			Length: pieceLength,
		}
	}

	done := make(chan struct{})

	go func() {
		peerSet := make(map[string]bool)
		peers := make(chan torrent.Peer, 4096)
		urls := make(chan url.URL, len(t.AnnounceUrls))
		for _, u := range t.AnnounceUrls {
			base, err := url.Parse(u)
			if err != nil {
				log.Println(err)
				continue
			}
			urls <- *base
		}
		retryCh := make(chan string, 4096)
		port := 6881 // clients will try to listen on ports 6881 to 6889 before giving up.
	loop:
		for {
			select {
			case u := <-urls:
				t.FetchPeers(u, clientID, port, urls, peers)
			case p := <-peers:
				key := p.GetAddr()
				ok := peerSet[key]
				if !ok {
					peerSet[key] = true
					go handleClientPeer(t, clientID, p, requests, responses, retryCh)
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

	downloadCount := len(t.Pieces) - downloadedPieces
	for i := 0; i < downloadCount; i++ {
		resp := <-responses
		downloadedPieces++
		progress := float64(downloadedPieces) / float64(len(t.Pieces)) * 100
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
