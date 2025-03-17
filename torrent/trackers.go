package torrent

import (
	"bytes"
	"encoding/binary"
	"log"
	"math"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/jackpal/bencode-go"
)

func isTrackerURLSupported(url url.URL) bool {
	return url.Scheme == "http" || url.Scheme == "https" || url.Scheme == "udp"
}

func fetchPeers(d *Downloader, url url.URL, peers chan<- string) {
	if url.Scheme == "http" || url.Scheme == "https" {
		fetchPeersHTTP(d, url, peers)
	} else if url.Scheme == "udp" {
		fetchPeersUDP(d, url, peers)
	}
}

func fetchPeersHTTP(d *Downloader, trackerURL url.URL, peers chan<- string) {
	params := url.Values{
		"info_hash":  []string{string(d.Torrent.InfoHash[:])},
		"peer_id":    []string{string(d.ClientID[:])},
		"port":       []string{strconv.Itoa(d.Port)},
		"uploaded":   []string{"0"},
		"downloaded": []string{"0"},
		"compact":    []string{"1"},
		"left":       []string{strconv.Itoa(d.Torrent.Length)},
	}
	trackerURL.RawQuery = params.Encode()
	client := &http.Client{
		Timeout: d.Config.FetchPeersTimeout,
	}
	resp, err := client.Get(trackerURL.String())
	if err != nil {
		log.Println(err)
		return
	}
	defer resp.Body.Close()

	data := struct {
		Peers string `bencode:"peers"`
	}{}

	if err := bencode.Unmarshal(resp.Body, &data); err != nil {
		log.Println(err)
		return
	}

	peersStr := data.Peers
	// assuming ipv4 here
	if len(peersStr)%6 != 0 {
		log.Println("invalid peers string: len(peers) must be divisible by 6")
		return
	}

	peerCount := len(peersStr) / 6
	for i := 0; i < peerCount; i++ {
		peerData := []byte(peersStr[i*6:])

		ip := net.IP(peerData[:4])
		port := binary.BigEndian.Uint16(peerData[4:])
		addr := net.JoinHostPort(ip.String(), strconv.Itoa(int(port)))
		peers <- addr
	}
}

// https://www.bittorrent.org/beps/bep_0015.html
func fetchPeersUDP(d *Downloader, trackerURL url.URL, peers chan<- string) {
	connStr := net.JoinHostPort(trackerURL.Hostname(), trackerURL.Port())
	conn, err := net.DialTimeout("udp", connStr, d.Config.FetchPeersTimeout)
	if err != nil {
		log.Println(err)
		return
	}
	defer conn.Close()

	var (
		connectionID uint64
	)

	// connect
	{
		var req bytes.Buffer
		binary.Write(&req, binary.BigEndian, uint64(0x41727101980)) // protocol_id
		action := uint32(0)                                         // connect
		binary.Write(&req, binary.BigEndian, action)
		transactionID := rand.Uint32()
		binary.Write(&req, binary.BigEndian, transactionID)

		res, err := handleUTPRequest(conn, req.Bytes())
		if err != nil {
			log.Println(err)
			return
		}

		responseAction := binary.BigEndian.Uint32(res[:4])
		reponseTransationID := binary.BigEndian.Uint32(res[4:8])

		if reponseTransationID != transactionID {
			log.Printf("transation id mismatch send %d got %d\n", transactionID, reponseTransationID)
			return
		}

		if responseAction != 0 {
			log.Printf("action should be 0 got %d\n", action)
			if responseAction == 3 {
				msg := res[8:]
				log.Printf("error action: %v\n", msg)
			}
			return
		}

		connectionID = binary.BigEndian.Uint64(res[8:])
		log.Printf("connected to udp tracker %d\n", connectionID)
	}

	// announce
	{
		transactionID := rand.Uint32()
		action := uint32(1) // announce

		var req bytes.Buffer
		binary.Write(&req, binary.BigEndian, connectionID)
		binary.Write(&req, binary.BigEndian, action)
		binary.Write(&req, binary.BigEndian, transactionID)
		req.Write(d.Torrent.InfoHash[:])
		req.Write(d.ClientID[:])
		downloaded := uint64(0)
		binary.Write(&req, binary.BigEndian, downloaded)
		left := uint64(d.Torrent.Length)
		binary.Write(&req, binary.BigEndian, left)
		uploaded := uint64(0)
		binary.Write(&req, binary.BigEndian, uploaded)
		event := uint32(0)
		binary.Write(&req, binary.BigEndian, event)
		ip := uint32(0)
		binary.Write(&req, binary.BigEndian, ip)
		key := uint32(0)
		binary.Write(&req, binary.BigEndian, key)
		numWant := int32(-1)
		binary.Write(&req, binary.BigEndian, numWant)
		binary.Write(&req, binary.BigEndian, uint16(d.Port))

		res, err := handleUTPRequest(conn, req.Bytes())
		if err != nil {
			log.Println(err)
			return
		}

		responseAction := binary.BigEndian.Uint32(res[:4])
		if responseAction != 1 {
			log.Printf("action should be 1 got %d\n", action)
			if responseAction == 3 {
				msg := res[8:]
				log.Printf("error action: %v\n", msg)
			}
			return
		}

		reponseTransationID := binary.BigEndian.Uint32(res[4:8])
		if reponseTransationID != transactionID {
			log.Printf("transation id mismatch send %d got %d\n", transactionID, reponseTransationID)
			return
		}

		interval := binary.BigEndian.Uint32(res[8:12])
		leechers := binary.BigEndian.Uint32(res[12:16])
		_ = interval
		_ = leechers

		seeders := binary.BigEndian.Uint32(res[16:20])
		data := res[20:]
		for i := 0; i < int(seeders); i++ {
			ip := data[i*6 : i*6+4]
			port := binary.BigEndian.Uint16(data[i*6+4 : i*6+6])
			addr := net.JoinHostPort(net.IPv4(ip[0], ip[1], ip[2], ip[3]).String(), strconv.Itoa(int(port)))
			peers <- addr
		}
	}
}

func handleUTPRequest(conn net.Conn, request []byte) ([]byte, error) {
	retransmissionExponent := 0
	res := make([]byte, 8096)
	for {
		_, err := conn.Write(request)
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				continue
			}
			return nil, err
		}
		timeout := 15 * int(math.Floor(math.Pow(2.0, float64(retransmissionExponent))))
		err = conn.SetReadDeadline(time.Now().Add(time.Duration(timeout) * time.Second))
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				continue
			}
			return nil, err
		}
		n, err := conn.Read(res)
		if retransmissionExponent < 8 {
			retransmissionExponent++
		}
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				continue
			}
			return nil, err
		} else {
			return res[:n], nil
		}
	}
}
