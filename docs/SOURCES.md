# Sources: connect your receiver, or write a new adapter

MDTI reads traffic from any mix of sources, selected with `-sources` or the `SOURCES` variable
(comma-separated, the flag wins). Every source turns what it receives into the same
`domain.Frame`, so all detectors work on all of them. A detector only stays silent when a source
does not report the field it needs.

| Entry | What it reads | Fields it cannot provide |
|---|---|---|
| `adsb` | Public aggregator, chosen by `ADSB_SOURCE` (`adsbfi`, `adsblol`), around `ADSB_CENTER_*` | none |
| `aisstream` (alias `ais`) | [AISStream](https://aisstream.io) WebSocket, needs `AISSTREAM_API_KEY`, area `AIS_BBOX` | none |
| `readsb:<url>` | Your ADS-B receiver's `aircraft.json` (readsb, dump1090-fa, tar1090) | none |
| `sbs:<host:port>` | Your ADS-B receiver's BaseStation feed (dump1090, readsb, Virtual Radar Server) | GNSS altitude, NIC/NACp/SIL, emitter category |
| `nmea-udp:[host]:<port>` | AIS NMEA 0183 (`!AIVDM`) datagrams from your AIS receiver | none |
| `nmea-tcp:<host:port>` | The same over a TCP connection (MDTI connects as a client) | none |
| `synthetic` | `SYN`-labelled background traffic for the offline demo | n/a |

With `sbs`, the GNSS/baro mismatch detector (D2) and the navigation-integrity detector never
fire, because SBS does not carry those fields. Prefer `readsb:` when your receiver serves
`aircraft.json`. Every adapter reconnects on its own with backoff.

## Connect your receiver

The coverage settings describe *your antenna*: `ADSB_CENTER_LAT` / `ADSB_CENTER_LON` are its
position and `ADSB_RADIUS_NM` its typical range. The ghost-track detector uses them: a real
aircraft enters coverage at its edge, while a ghost appears in the middle.

### ADS-B: readsb, dump1090-fa or tar1090 (aircraft.json)

```sh
# On the receiver (Raspberry Pi + RTL-SDR, for example), readsb usually serves its web root on 8080:
curl http://192.168.1.50:8080/data/aircraft.json | head -c 200
```

```env
SOURCES=readsb:http://192.168.1.50:8080
ADSB_CENTER_LAT=-34.60
ADSB_CENTER_LON=-58.38
ADSB_RADIUS_NM=120
ADSB_POLL_INTERVAL=1s
```

A full file URL also works, e.g. `readsb:http://pi.local/tar1090/data/aircraft.json`.

### ADS-B: BaseStation / SBS on port 30003

```sh
# readsb / dump1090 option that enables the feed:
readsb --net --net-sbs-port 30003 ...
```

```env
SOURCES=sbs:192.168.1.50:30003
```

### AIS: AIS-catcher, rtl-ais, dAISy (NMEA over UDP)

```sh
# AIS-catcher: decode with an RTL-SDR and forward NMEA to the MDTI host
AIS-catcher -d:0 -u 192.168.1.10 10110
```

```env
SOURCES=nmea-udp::10110            # listen on all interfaces, port 10110
```

If your feed is a TCP server (OpenCPN, a network multiplexer, `kplex`), use
`nmea-tcp:<host>:<port>`. Message types 1/2/3, 5, 18, 19 and 24 are decoded. Static data (name,
type, dimensions) is joined onto positions when it arrives.

### Mixing sources

```env
SOURCES=readsb:http://192.168.1.50:8080,nmea-udp::10110,aisstream
```

### Try it without hardware

`tools/receiver-sim` pretends to be a receiver. It serves synthetic traffic as SBS, as
`aircraft.json` and as AIS NMEA, and can make one aircraft jump 12 NM so you can watch Tier 1
catch it:

```sh
go run ./tools/receiver-sim -teleport 60s
go run ./cmd/mdti -env "" -modules=rx,tier1 -sources=sbs:127.0.0.1:30003,nmea-udp:127.0.0.1:10110
# … msg=anomaly type=KINEMATIC_IMPOSSIBLE rule="D1.R2 teleport"
```

## Write a new source

A source is anything that implements:

```go
type Source interface {
	Name() string
	Run(ctx context.Context, out chan<- domain.Frame) error // until ctx ends; reconnect inside
}
```

1. **Map only what you receive.** Frame pointer fields (`AltGeomFt`, `GroundSpeedMps`, `NACp`, …)
   mean "not reported" when nil. Never fill them with zeros: a zero GNSS altitude would look
   like a 24 000 ft GNSS/baro mismatch. Convert to SI where the field says so (`GroundSpeedMps`
   in m/s, `domain.KnotToMps`).
2. **Identity:** `EntityKey` is `domain.ADSBKey(icao24 lowercase hex)` or `domain.AISKey(mmsi)`.
3. **Treat text as untrusted.** Pass callsigns and ship names through `domain.SanitizeIdent`.
   They reach Lattice, but never the model's prompt.
4. **Tag the origin.** Add a `domain.Source` value and set `Frame.Source`. It is used for statistics
   and the ground-truth writer, and `Frame.Strip()` removes it before Tier 1 and Tier 2, so
   detection never depends on where a frame came from.
5. **Validate and do not block.** Call `f.Validate(now)` and drop invalid frames. Send with
   `select { case out <- f: default: dropped++ }` so a slow consumer never stalls your reader.
6. **Register it** in `internal/rx/sources` (parse and validate the entry) and in the source
   switch in `cmd/mdti/main.go`.
7. **Test it with synthetic data only** (no third-party captures): decode cases, invalid input, a
   network round trip on a `127.0.0.1:0` listener, and one end-to-end case through `tier1`. See
   `internal/rx/sbs/sbs_test.go` (`TestTeleportOverSBSIsDetected`) as a template.

Reference implementations: `internal/rx/sbs` (line-based TCP with partial updates),
`internal/rx/ais/nmea.go` (UDP/TCP with multi-sentence reassembly), and `internal/rx/adsb`
(HTTP polling with de-duplication).
