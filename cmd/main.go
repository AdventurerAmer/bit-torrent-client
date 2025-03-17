package main

import (
	"flag"
	"log"
	"time"

	"github.com/AdventurerAmer/bit-torrent-client/torrent"
)

func main() {
	log.SetFlags(log.LUTC | log.Llongfile)

	cfg := torrent.Config{}
	flag.DurationVar(&cfg.FetchPeersTimeout, "fetch-peers-timeout", 10*time.Second, "fetch peers timeout")
	flag.DurationVar(&cfg.UpdateTrackersRate, "update-tracker-rate", 1*time.Minute, "update trackers rate")
	flag.DurationVar(&cfg.ReadMessageTimeout, "read message timeout", 10*time.Second, "read message timeout")

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
	}

	if err != nil {
		log.Fatalf("failed to parse torrent %v: %v", *filePath, err)
	}

	d := torrent.NewDownloader(cfg, t)
	err = d.Start(*downloadPath)
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
