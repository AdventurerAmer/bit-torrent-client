package torrent

type Downloader struct {
	torrent *Torrent
}

func (d *Downloader) Download(path string) {
}

func NewDownloader(torrent *Torrent) *Downloader {
	return &Downloader{torrent: torrent}
}
