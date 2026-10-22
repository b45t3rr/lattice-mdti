# Recording the demo

The demo has two views that are best recorded together:

1. **Terminal**: `make demo` records the narration to `docs/demo/demo.cast` and renders
   `docs/demo/demo.gif` automatically.
2. **Lattice map**: the ghost turning HOSTILE and SENTINEL-1 flying the intercept. This is the
   Lattice web UI, so record it with a screen recorder (for example OBS Studio).

## Before you record

- Stop any other MDTI agent (`cmd/agent`). The demo refuses to start while another instance drives
  the assets.
- Fill in `.env` (Lattice, AISStream and OpenAI credentials). The demo sets `AUTONOMY_LEVEL=autonomous`
  for its own daemon only.
- **Privacy:**
  - The Lattice URL and some UI panels contain your **environment id**. Crop the browser address bar
    and hide any panel that shows tokens or the environment hostname.
  - The terminal output already redacts the hostname (`***.env.sandboxes…`).
  - Never record `.env` or a shell history that contains tokens.

## Lattice view: what to frame

Centre the map on Morón (−34.676, −58.643) at roughly a 40 km zoom. Then:

| Demo time | What appears in Lattice |
|---|---|
| ~00:05 | SENTINEL-1 parked at Morón (friendly UAV symbol), live AMBA traffic (neutral symbols) |
| ~00:31 | The ghost `ICAO E3F00D` appears 8 NM north-west of Morón (unknown symbol) |
| ~00:35 | The ghost turns **hostile** (red symbol). Its description shows the `[MDTI]` operator log and evidence path |
| ~00:36 | A VisualId task appears for SENTINEL-1 and moves to EXECUTING |
| 00:37–02:35 | SENTINEL-1 flies the intercept (about 14 km) and orbits or trails the ghost |
| ~02:35 | Task DONE_OK; SENTINEL-1 returns to Morón |

Tip: open the ghost's entity panel at ~00:35 to show the disposition override and the operator note.

## Producing the final video

1. Start the screen recorder on the Lattice tab.
2. Run `make demo` in a terminal.
3. Stop the recorder when the terminal prints `CLEANUP`.
4. Combine the terminal GIF and the map recording side by side, for example with ffmpeg:

   ```sh
   ffmpeg -i lattice.mp4 -i docs/demo/demo.gif -filter_complex \
     "[1:v]scale=-1:720[t];[0:v]scale=-1:720[m];[m][t]hstack" -c:v libx264 -crf 23 demo.mp4
   ```

   The GIF compresses idle time (`-idle 1.2`), so for a time-aligned side-by-side render the GIF with
   `-idle 1000` (no compression).
