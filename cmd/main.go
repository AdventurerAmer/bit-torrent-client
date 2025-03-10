package main

import (
	"flag"
	"log"

	"github.com/AdventurerAmer/bit-torrent-client/torrent"
)

func main() {

	log.SetFlags(log.LUTC | log.Llongfile)

	filePath := flag.String("file", "", "path of torrent file")
	magnet := flag.String("magnet", "", "magnet link")
	downloadPath := flag.String("path", ".", "download path")
	flag.Parse()

	var (
		t   *torrent.Torrent
		err error
	)

	if *magnet != "" {
		t, err = torrent.ParseMagnet(*magnet)
	} else {
		t, err = torrent.ParseFile(*filePath)
		if err != nil {
			log.Fatalf("failed to parse .torrent file %v: %v", *filePath, err)
		}
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
