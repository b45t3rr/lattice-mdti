# Recording the demo

The demo runs four acts in detection-only mode: no asset is dispatched. Each act is injected at
the data layer, then detected, decided and drawn on the map with the evidence of the
inconsistency.

| Act | Threat | Detected at about | Where to look |
|---|---|---|---|
| 1 | Ghost aircraft `ICAO E3F00D` | 00:28 | Morón (SADM), inside the orange 15 NM watch ring |
| 2 | Impossible physics, airliner `ARG1234` jumps 12 NM | 01:48 | South of Ezeiza towards La Plata: red jump line |
| 3 | GPS spoofing, airliner `ARG5678` | 03:10 | Río de la Plata |
| 4 | AIS switched off, fishing vessel | 08:40 | Milla 201, ~46°S 61°W: cyan 200 NM line, AIS OFF marker, dashed estimated track |

## Terminal recording (the README GIF)

```sh
make build
./bin/demo -offline -open=false -cast docs/demo/demo.cast   # no accounts needed
go run ./tools/cast2gif -in docs/demo/demo.cast -out docs/demo/demo.gif -idle 1.2
```

`make demo` does the same against a real Lattice Sandbox (it needs `.env`) and renders the GIF.

## Map recording

- **Offline:** the sandbox map at `http://127.0.0.1:8740/`. Leave "Follow alerts" on and it flies
  to each alert. The Alerts panel lists every inconsistency with its evidence.
- **Lattice C2:** run `make demo` and record the Lattice web UI with a screen recorder (for example
  OBS Studio). Click a track to show the disposition override (Data Overrides). Use the timeline
  ("Last Hour") to replay the ghost appearing and the jump.

Pass `-hold 10m` to keep everything on screen after the last act.

## Privacy

- The Lattice URL and some UI panels contain your **environment id**. Crop the browser address bar
  and hide any panel that shows tokens or the environment hostname.
- The terminal output already redacts the hostname (`***.env.sandboxes…`).
- Never record `.env` or a shell history that contains tokens.

## Producing a side-by-side video

```sh
ffmpeg -i map.mp4 -i docs/demo/demo.gif -filter_complex \
  "[1:v]scale=-1:720[t];[0:v]scale=-1:720[m];[m][t]hstack" -c:v libx264 -crf 23 demo.mp4
```

The GIF compresses idle time (`-idle 1.2`). For a time-aligned side-by-side, render it with
`-idle 1000` (no compression).
