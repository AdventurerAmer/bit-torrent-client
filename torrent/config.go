package torrent

import "time"

type Config struct {
	FetchPeersTimeout     time.Duration
	UpdateTrackersRate    time.Duration
	ReadMessageTimeout    time.Duration
	PeerConnectionTimeout time.Duration
	RetryPeersRate        time.Duration
}
