# BTC

## Description

	BTC is an educational cli bit torrent client to download .torrent files and magnets links.

## Goal

	The goal of the project was to practice Golang’s concurrency along the way,
    I learned a ton about network programming with the TCP protocol, the bencode encoding,
    the BitTorrent protocol, and the peer-to-peer architecture.

## Features

- UDP trackers.
- Resuming downloads.
- Magnet links.

## Quick Start

  ```bash
  go run ./cmd -file=sample.torrent -path=./download 
  ```
## Usage

- for torrent files
```bash
go run ./cmd -file [path to .torrent file] -path [download path] 
```

- for magnet links
```bash
go run ./cmd -magnet [magnet link] -path [download path]
```

## Limitations

- we don't handle the case of having no trackers in the magnet link or .torrent file.
- you can only download one file at a time.