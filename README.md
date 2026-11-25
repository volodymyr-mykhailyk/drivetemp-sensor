# drivetemp-sensor

Publishes the temperature of the hottest hard drive and the hottest SSD as plain sensor files, so
a fan controller can drive a fan from them.

It reads every SATA/SAS drive through the kernel's
[`drivetemp`](https://docs.kernel.org/hwmon/drivetemp.html) hwmon driver, splits them into HDDs and
SSDs, and every cycle writes two files:

```
/run/drivetemp-sensor/hdd-max   41000
/run/drivetemp-sensor/ssd-max   35000
```

[CoolerControl](https://gitlab.com/coolercontrol/coolercontrol),
[fan2go](https://github.com/markusressel/fan2go),
[afancontrol](https://github.com/KostyaEsmukov/afancontrol) and anything else that can read a
`temp*_input`-style file can use them as a sensor. drivetemp-sensor does not control fans itself.

## Why

Fan controllers usually select drive sensors by hwmon number, device name or a fixed list. Those
change when drives are added, removed, re-cabled or the kernel is upgraded, and the fan then
follows nothing (or a stale value) without anyone noticing.

drivetemp-sensor instead finds drives **by rule on every cycle**: any block device with a
`drivetemp` sensor is a drive, `queue/rotational` decides HDD or SSD. Nothing needs to change
when the set of drives does. And when it cannot know a temperature, it reports **hot, never
cold**, so the fans speed up instead of quietly stopping.

## Output contract

- One file per sensor in `--output-dir` (default `/run/drivetemp-sensor`):
  - `hdd-max` — max over drives with `queue/rotational` = `1`
  - `ssd-max` — max over drives with `queue/rotational` = `0`
- Content: an integer in **millidegrees Celsius** and a newline (`41000\n`), the hwmon
  `temp*_input` format.
- Files are replaced atomically (temp file, `fsync`, `rename`) and are mode `0644`. Consumers
  must mount the **directory**, not the file: a single-file bind mount keeps pointing at the
  replaced inode and never sees an update.
- Files are rewritten every `--interval` (default 10 s), even when the value has not changed.
- A disabled sensor (`--sensors hdd-max`) writes no file.

### Values

Each cycle, per sensor:

| Situation | Value |
|---|---|
| At least one drive with a fresh reading | Max of the fresh readings and of any last reading still within `--stale-after` |
| A drive's read failed or is pending, its last good reading is younger than `--stale-after` (default 60 s) | That last reading takes part in the max |
| A drive's read failed or is pending and it has no good reading younger than `--stale-after` | Failsafe |
| Fewer drives than `--min-hdd` / `--min-ssd` (default 1), or no drives of the class at all | Failsafe |
| All drives of the class asleep (`--standby=skip` only) | `--standby-temp` (default 30 °C) |
| Discovery fails, or the file cannot be written, or any other internal error | Failsafe |

- The failsafe value is `--failsafe-temp`, default **100 °C** (`100000`): inside CoolerControl's
  valid range (0–120 °C) and at the top of any sane fan curve.
- A read that has not returned within `--read-timeout` (default 5 s) is *pending*, not failed —
  `drivetemp` can block for many seconds while a drive spins up. No second read of that drive is
  started while one is pending.
- Readings outside 1–150 °C are firmware placeholders, not measurements, and count as failed reads.
- Excluded drives (`--exclude`) do not count towards `--min-hdd` / `--min-ssd`.

### Lifecycle

- **On start**, before the first read, every enabled sensor is set to the failsafe value, so a
  consumer never sees a number left over from an earlier run.
- **On SIGTERM / SIGINT**, every enabled sensor is set to the failsafe value and the process exits 0
  (`--on-exit=failsafe`, the default). With `--on-exit=keep` the last values stay. Fans running
  fast while the tool is stopped is intended.
- **What it cannot cover:** if the process is killed with `SIGKILL` (or by the OOM killer) and not
  restarted, the files keep their last value, and no fan controller checks a file's age. Mitigate
  with a restart policy (`restart: unless-stopped`) and the healthcheck below.
- **Healthcheck:** `drivetemp-sensor healthcheck` exits 0 if every enabled sensor file exists,
  holds an integer and was written within 3 × `--interval`; 1 otherwise. The image uses it as its
  `HEALTHCHECK`. It reads the same flags and environment variables as the daemon, so configure the
  container with **environment variables** (not `command:` flags) to keep both in agreement.
- **Logs** go to stderr, one line per state change: drive appeared, disappeared, renamed,
  excluded, read failed, read pending, asleep, recovered; sensor entered or left failsafe.
  `--log-level=debug` adds every value written.

## The drivetemp standby caveat

With the default `--standby=read`, every drive is read every cycle, asleep or not. Many drives
answer a temperature query in standby without spinning up, but this is **firmware-dependent**:
the kernel documentation notes that some drives reset their spin-down timer when the temperature
is read, which keeps them from ever spinning down. Check your drives: let them spin down
(`hdparm -C /dev/sdX` reports `standby`), run drivetemp-sensor for a few intervals, and check
again.

If your drives do wake up, `--standby=skip` (below) avoids reading them while they sleep, or
increase `--interval` to beyond the drives' spin-down time.

## Running it

### Requirements

- Linux with the `drivetemp` module loaded (`modprobe drivetemp`; add it to your modules-load
  configuration to make it permanent). Check with
  `cat /sys/block/sd*/device/hwmon/hwmon*/name` — you should see `drivetemp` once per drive.
- In the default mode, nothing else: `/sys` is readable from an unprivileged container, and the
  image runs as UID 65534 with no capabilities.

### Docker Compose

Share a tmpfs-backed volume between drivetemp-sensor and the fan controller:

```yaml
services:
  drivetemp-sensor:
    image: ghcr.io/volodymyr-mykhailyk/drivetemp-sensor:0.1
    restart: unless-stopped
    read_only: true
    cap_drop: [ALL]
    security_opt: ["no-new-privileges:true"]
    environment:
      DRIVETEMP_SENSOR_INTERVAL: 10s
      # DRIVETEMP_SENSOR_EXCLUDE: "Elements*,naa.5000c500deadbeef"
      # DRIVETEMP_SENSOR_SENSORS: hdd-max
    volumes:
      - sensors:/run/drivetemp-sensor

  coolercontrol:
    # ... your existing CoolerControl service ...
    volumes:
      - sensors:/sensors:ro

volumes:
  sensors:
    driver: local
    driver_opts:
      type: tmpfs
      device: tmpfs
      o: "uid=65534,gid=65534,mode=0755"
```

If the fan controller runs on the host instead, bind-mount a host directory, e.g.
`/run/drivetemp-sensor:/run/drivetemp-sensor`, owned by UID 65534 (`install -d -o 65534 -g 65534
/run/drivetemp-sensor`), and point the controller at `/run/drivetemp-sensor/hdd-max`.

### Binary

Static binaries for `linux/amd64` and `linux/arm64` are attached to each
[release](https://github.com/volodymyr-mykhailyk/drivetemp-sensor/releases) with a `checksums.txt`.

```sh
drivetemp-sensor --output-dir /run/drivetemp-sensor --exclude 'Elements*'
```

### `--standby=skip` (experimental)

Before reading an HDD, ask it for its power state with ATA CHECK POWER MODE — the command
`hdparm -C` sends, which does not wake the drive. Drives in standby are not read and count as
*asleep*; when every drive of a class is asleep, the sensor reads `--standby-temp`. If the power
query fails, the temperature is read anyway. SSDs are never queried.

This needs access to the block devices and `CAP_SYS_RAWIO`:

```yaml
services:
  drivetemp-sensor:
    image: ghcr.io/volodymyr-mykhailyk/drivetemp-sensor:0.1
    user: "0:0"
    cap_add: [SYS_RAWIO]
    devices:
      - /dev/sda
      - /dev/sdb
      - /dev/sdc
    environment:
      DRIVETEMP_SENSOR_STANDBY: skip
    volumes:
      - sensors:/run/drivetemp-sensor
```

The device is opened as `/dev/<name>` using the kernel name found in sysfs, so the `devices:`
list has to match the current `sdX` names. **Experimental:** the power query is unit-tested
against recorded ATA responses but has not yet been verified on real hardware.

## Configuration

Every flag can also be set as an environment variable `DRIVETEMP_SENSOR_<FLAG>` (upper case,
`-` → `_`), e.g. `DRIVETEMP_SENSOR_READ_TIMEOUT=5s`. A command-line flag overrides its variable.
`--exclude` is repeatable; its variable takes a comma-separated list, and values from both are
combined.

| Flag | Default | |
|---|---|---|
| `--output-dir` | `/run/drivetemp-sensor` | Directory for the sensor files |
| `--sensors` | `hdd-max,ssd-max` | Sensors to publish |
| `--interval` | `10s` | Time between cycles |
| `--read-timeout` | `5s` | Wait for a read before it counts as pending (≤ interval) |
| `--stale-after` | `60s` | How long a last good reading stands in for a failed/pending read |
| `--failsafe-temp` | `100` | °C written when the temperature is unknown |
| `--standby` | `read` | `read` every drive, or `skip` drives in standby (experimental) |
| `--standby-temp` | `30` | °C written when all drives of a class are asleep (`skip` only) |
| `--min-hdd`, `--min-ssd` | `1` | Fail safe when fewer drives of the class are found |
| `--exclude` | none | Glob ([`path.Match`](https://pkg.go.dev/path#Match) syntax) matched against model, serial and WWID |
| `--on-exit` | `failsafe` | On SIGTERM/SIGINT: `failsafe` or `keep` |
| `--status-file` | none | Also write a JSON status file (per-drive state, value, age; per-sensor value and reason) |
| `--sysfs-root` | `/sys` | sysfs mount point (for tests) |
| `--log-level` | `info` | `debug`, `info`, `warn`, `error` |

Temperatures on the command line are in °C; files are in millidegrees.

Subcommands: `drivetemp-sensor healthcheck`, `drivetemp-sensor version`.

### Drive identity

Drives are identified, for logs, exclusions and the last-reading cache, by `device/wwid` if
present, else the serial number from `device/vpd_pg80`, else model and kernel name. The `hwmonN`
number and `sdX` name alone are never used. Use `--status-file` or `--log-level=debug` to see the
identities of your drives, or read them directly:

```sh
for d in /sys/block/sd*; do echo "$d $(cat $d/device/model) $(cat $d/device/wwid 2>/dev/null)"; done
```

## Consumer examples

### CoolerControl 5.x

1. Make the sensor directory visible to the CoolerControl daemon (`coolercontrold`), for example
   the `sensors` volume mounted at `/sensors` as in the compose example above.
2. In CoolerControl, add a **Custom Sensor** of type **File**, with the path
   `/sensors/hdd-max`. Repeat for `/sensors/ssd-max` if you need it.
3. Use the custom sensor as the temperature source of the drive fan's profile (graph/curve).

CoolerControl reads the file as millidegrees and accepts 0–120 °C. If the file becomes unreadable,
CoolerControl keeps the last value for a few polls and then assumes 100 °C.

### fan2go

```yaml
sensors:
  - id: hdd_max
    file:
      path: /run/drivetemp-sensor/hdd-max

curves:
  - id: drive_curve
    linear:
      sensor: hdd_max
      steps:
        - 30: 60
        - 40: 120
        - 45: 200
        - 50: 255

fans:
  - id: drive_fan
    hwmon:
      platform: nct6775-isa-0290
      rpmChannel: 2
    curve: drive_curve
```

### afancontrol

```ini
[temp: hdd_max]
type = file
path = /run/drivetemp-sensor/hdd-max
min = 35
max = 50
panic = 55

[fan: drive_fan]
type = linux
pwm = /sys/class/hwmon/hwmon2/pwm2
fan_input = /sys/class/hwmon/hwmon2/fan2_input

[mapping: drives]
fans = drive_fan
temps = hdd_max
```

The failsafe value (100 °C) is above `panic`, so afancontrol runs all fans at full speed while
drivetemp-sensor does not know the temperature.

## Development

```sh
go vet ./...
go test -race ./...
golangci-lint run
docker build -t drivetemp-sensor:dev . && ./scripts/smoke-image.sh drivetemp-sensor:dev
```

Tests run against a fake sysfs tree in [`testdata/sysfs`](testdata/sysfs) via `--sysfs-root`.

Layout: [`cmd/drivetemp-sensor`](cmd/drivetemp-sensor) (CLI) and small packages under
[`internal/`](internal): `discovery` (finds and classifies drives), `reader` (reads with timeout
and pending tracking), `power` (ATA CHECK POWER MODE), `aggregate` (the value rules above),
`output` (atomic writes, healthcheck), `daemon` (the cycle, startup and shutdown), `config`.

CI runs vet, race tests, golangci-lint, an image build and a smoke test of the image on every pull
request. Pushes to `main` publish `ghcr.io/volodymyr-mykhailyk/drivetemp-sensor:edge` and
`:sha-<short>`; tags `vX.Y.Z` publish `:X.Y.Z`, `:X.Y` and `:latest` and attach binaries to the
GitHub release.

## Not (yet) included

NVMe drives (their temperature is not under `drivetemp`), aggregations other than max, a
Prometheus `/metrics` endpoint, SMART attributes.

## License

[MIT](LICENSE)
