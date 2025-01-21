package main

import (
	"flag"
	"log"

	"github.com/harlequingg/bit-torrent-client/torrent"
)

func main() {
	filePath := flag.String("file", "", "path of torrent file")
	downloadPath := flag.String("path", ".", "download path")
	flag.Parse()

	t, err := torrent.Parse(*filePath)
	if err != nil {
		log.Fatal(err)
	}

	d := torrent.NewDownloader(t)
	err = d.Download(*downloadPath)
	if err != nil {
		log.Fatal(err)
	}
loop:
	for {
		select {
		case <-d.Done:
			break loop
		case p := <-d.Progress:
			log.Printf("download progress %.2f%%\n", p)
		}
	}
}
