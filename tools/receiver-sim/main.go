// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The MDTI Authors

// Command receiver-sim pretends to be a local receiver, so the local-source
// adapters can be tried without hardware. It serves synthetic traffic (SYN
// callsigns, internal/rx/synth) in the three formats MDTI reads:
//
//   - BaseStation/SBS on a TCP port, like dump1090 / readsb on 30003;
//   - aircraft.json over HTTP, like a readsb / tar1090 web root;
//   - AIS NMEA 0183 (!AIVDM) sent over UDP, like AIS-catcher.
//
// With -teleport it makes one aircraft jump 12 NM after the given delay, so
// Tier 1 has an impossible-kinematics anomaly to find:
//
//	go run ./tools/receiver-sim -teleport 60s
//	go run ./cmd/mdti -modules=rx,tier1 -sources=sbs:127.0.0.1:30003,nmea-udp:127.0.0.1:10110
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	goais "github.com/BertoldVdb/go-ais"
	"github.com/BertoldVdb/go-ais/aisnmea"

	"github.com/b45t3rr/lattice-mdti/internal/domain"
	"github.com/b45t3rr/lattice-mdti/internal/geo"
	"github.com/b45t3rr/lattice-mdti/internal/rx/synth"
)

func main() {
	sbsAddr := flag.String("sbs", "127.0.0.1:30003", "listen address for the SBS feed (empty = off)")
	jsonAddr := flag.String("json", "127.0.0.1:8080", "listen address for /data/aircraft.json (empty = off)")
	nmeaTo := flag.String("nmea-udp", "127.0.0.1:10110", "send AIS NMEA datagrams to this address (empty = off)")
	teleport := flag.Duration("teleport", 0, "make one aircraft jump 12 NM after this delay (0 = never)")
	lat := flag.Float64("lat", -34.70, "coverage centre latitude")
	lon := flag.Float64("lon", -58.55, "coverage centre longitude")
	radius := flag.Float64("radius-nm", 60, "coverage radius")
	flag.Parse()
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, *sbsAddr, *jsonAddr, *nmeaTo, *teleport, geo.Point{Lat: *lat, Lon: *lon}, *radius, log); err != nil {
		fmt.Fprintln(os.Stderr, "receiver-sim:", err)
		os.Exit(1)
	}
}

type hub struct {
	mu      sync.Mutex
	clients map[net.Conn]*bufio.Writer
	latest  map[string]domain.Frame // aircraft.json snapshot
}

func (h *hub) broadcast(lines []string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for c, w := range h.clients {
		for _, l := range lines {
			fmt.Fprint(w, l+"\r\n")
		}
		if err := w.Flush(); err != nil {
			c.Close()
			delete(h.clients, c)
		}
	}
}

func run(ctx context.Context, sbsAddr, jsonAddr, nmeaTo string, teleport time.Duration, center geo.Point, radius float64, log *slog.Logger) error {
	h := &hub{clients: map[net.Conn]*bufio.Writer{}, latest: map[string]domain.Frame{}}
	if sbsAddr != "" {
		ln, err := net.Listen("tcp", sbsAddr)
		if err != nil {
			return err
		}
		defer ln.Close()
		go func() {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				h.mu.Lock()
				h.clients[c] = bufio.NewWriter(c)
				h.mu.Unlock()
				log.Info("SBS client connected", "from", c.RemoteAddr().String())
			}
		}()
		log.Info("SBS feed", "tcp", ln.Addr().String())
	}
	if jsonAddr != "" {
		mux := http.NewServeMux()
		mux.HandleFunc("GET /data/aircraft.json", func(w http.ResponseWriter, _ *http.Request) {
			h.mu.Lock()
			defer h.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(aircraftJSON(h.latest, time.Now()))
		})
		srv := &http.Server{Addr: jsonAddr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
		go func() { _ = srv.ListenAndServe() }()
		defer srv.Close()
		log.Info("aircraft.json", "url", "http://"+jsonAddr+"/data/aircraft.json")
	}
	var udp net.Conn
	if nmeaTo != "" {
		c, err := net.Dial("udp", nmeaTo)
		if err != nil {
			return err
		}
		defer c.Close()
		udp = c
		log.Info("AIS NMEA", "udp_to", nmeaTo)
	}
	nc := aisnmea.NMEACodecNew(goais.CodecNew(false, false))

	// Keep first fixes away from the Morón ghost-watch ring (Tier-1 D3).
	tr := &synth.Traffic{Center: center, RadiusNM: radius, Aircraft: 10, Seed: 7,
		Avoid: []geo.Point{{Lat: -34.6763, Lon: -58.6428}}, AvoidNM: 20}
	start := time.Now()
	tr.Start(start)
	var jumpKey string
	jumped := false
	lastStatic := map[string]time.Time{}
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		now := time.Now()
		for _, f := range tr.Step(now) {
			if f.Domain == domain.DomainAir {
				if jumpKey == "" {
					jumpKey = f.EntityKey
				}
				if teleport > 0 && f.EntityKey == jumpKey && now.Sub(start) >= teleport {
					p := geo.Destination(geo.Point{Lat: f.Lat, Lon: f.Lon}, 45, 12*domain.NMToM)
					f.Lat, f.Lon = p.Lat, p.Lon
					if !jumped {
						jumped = true
						log.Info("teleport injected", "aircraft", f.EntityKey, "callsign", f.Callsign)
					}
				}
				h.mu.Lock()
				h.latest[f.EntityKey] = f
				h.mu.Unlock()
				h.broadcast(sbsLines(f))
				continue
			}
			if udp == nil {
				continue
			}
			var lines []string
			if now.Sub(lastStatic[f.MMSI]) > time.Minute {
				lastStatic[f.MMSI] = now
				lines = append(lines, nc.EncodeSentence(aisnmea.VdmPacket{Channel: 1, TalkerID: "AI", MessageType: "VDM", Packet: staticPacket(f)})...)
			}
			lines = append(lines, nc.EncodeSentence(aisnmea.VdmPacket{Channel: 1, TalkerID: "AI", MessageType: "VDM", Packet: positionPacket(f)})...)
			_, _ = udp.Write([]byte(strings.Join(lines, "\r\n") + "\r\n"))
		}
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
	}
}

func hex(key string) string { return strings.ToUpper(strings.TrimPrefix(key, "adsb:")) }

// sbsLines renders an aircraft frame as an airborne position (MSG,3) and an
// airborne velocity (MSG,4) line, as dump1090 would.
func sbsLines(f domain.Frame) []string {
	t := time.Now().Format("2006/01/02,15:04:05.000")
	alt := strconv.FormatFloat(*f.AltBaroFt, 'f', 0, 64)
	gs := strconv.FormatFloat(*f.GroundSpeedMps/domain.KnotToMps, 'f', 0, 64)
	trk := strconv.FormatFloat(*f.TrackDeg, 'f', 0, 64)
	return []string{
		fmt.Sprintf("MSG,1,1,1,%s,1,%s,%s,%s,,,,,,,,,,,", hex(f.EntityKey), t, t, f.Callsign),
		fmt.Sprintf("MSG,3,1,1,%s,1,%s,%s,,%s,,,%.5f,%.5f,,%s,0,0,0,0", hex(f.EntityKey), t, t, alt, f.Lat, f.Lon, f.Squawk),
		fmt.Sprintf("MSG,4,1,1,%s,1,%s,%s,,,%s,%s,,,0,,,,,0", hex(f.EntityKey), t, t, gs, trk),
	}
}

func aircraftJSON(latest map[string]domain.Frame, now time.Time) map[string]any {
	var ac []map[string]any
	for k, f := range latest {
		if now.Sub(f.ObservedAt) > time.Minute {
			continue
		}
		ac = append(ac, map[string]any{
			"hex": strings.ToLower(hex(k)), "flight": f.Callsign, "lat": f.Lat, "lon": f.Lon,
			"alt_baro": *f.AltBaroFt, "alt_geom": *f.AltGeomFt, "gs": *f.GroundSpeedMps / domain.KnotToMps, "track": *f.TrackDeg,
			"squawk": f.Squawk, "category": f.Category, "nic": *f.NIC, "nac_p": *f.NACp, "sil": *f.SIL,
			"seen_pos": now.Sub(f.ObservedAt).Seconds(),
		})
	}
	return map[string]any{"now": float64(now.UnixMilli()) / 1000, "messages": len(ac), "aircraft": ac}
}

func mmsi(f domain.Frame) uint32 {
	v, _ := strconv.ParseUint(f.MMSI, 10, 32)
	return uint32(v)
}

func positionPacket(f domain.Frame) goais.Packet {
	return goais.PositionReport{Header: goais.Header{MessageID: 1, UserID: mmsi(f)}, Valid: true,
		NavigationalStatus: uint8(*f.NavStatus), Sog: goais.Field10(*f.GroundSpeedMps / domain.KnotToMps),
		Longitude: goais.FieldLatLonFine(f.Lon), Latitude: goais.FieldLatLonFine(f.Lat),
		Cog: goais.Field10(*f.TrackDeg), TrueHeading: uint16(*f.HeadingDeg)}
}

func staticPacket(f domain.Frame) goais.Packet {
	l := uint16(*f.LengthM)
	return goais.ShipStaticData{Header: goais.Header{MessageID: 5, UserID: mmsi(f)}, Valid: true,
		Name: f.ShipName, Type: uint8(*f.ShipType), Dimension: goais.FieldDimension{A: l * 2 / 3, B: l - l*2/3, C: 5, D: 5}}
}
