package torrent

import "time"

type Config struct {
	FetchPeersTimeout  time.Duration
	UpdateTrackersRate time.Duration
}
