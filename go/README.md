# sugar — Accu-Chek Guide Me glucose sync (Go)

Single-file Go daemon that continuously syncs glucose records from an
Accu-Chek Guide Me to a TSV using the standard BLE Glucose Profile
(service `0x1808`).

Everything goes through BlueZ D-Bus:
- in-process `org.bluez.Agent1` for passkey pairing (PIN 538425)
- `Adapter1.StartDiscovery` + `PropertiesChanged` signals to wait for
  the meter to advertise (it only advertises briefly after a button press
  or a new reading)
- `Device1.Connect` → `GattCharacteristic1.StartNotify` on Glucose
  Measurement (handle 7) and RACP (handle 12 — the meter also exposes
  a RACP under a Roche vendor service which we ignore by scoping char
  lookup to the Glucose service)
- `RACP.WriteValue { "type": "request" }` with op `0x01` + operator
  `0x03` to fetch only records with `seq >= last_known + 1`
- append to `../records.tsv`

## Build + run

```sh
go build -o sugar .
cd ..                # run from project root so records.tsv lives there
./go/sugar
```

First run: put the meter in pairing mode (hold both arrows ~3s until the
Bluetooth icon appears). After pairing, the meter is trusted and future
syncs just need it to advertise (press a button, take a reading).

## Sample output

```
[agent] registered + default
starting sync loop. last known seq: 134
waiting for meter to advertise (press a button on the meter)…
advertisement seen — Connect()
connected.
gm:   /org/bluez/hci0/dev_04_47_07_32_14_BD/service0006/char0007
racp: /org/bluez/hci0/dev_04_47_07_32_14_BD/service0006/char000c
RACP: report records seq >= 135
  #135  2026-04-15 14:02:11   6.77 mmol/L  (122.0 mg/dL)
  RACP done: req_op=1 code=1
1 new records (last seq = 135)
waiting for meter to advertise (press a button on the meter)…
```

## TSV format

`records.tsv` at the project root:

```
sequence	timestamp	mmol_l	mg_dl	raw_hex
2	2026-03-09 03:18:31	9.10	164.0	0b0200ea07030903121f5802a4b0f80000
3	2026-03-09 05:09:02	8.94	161.0	0b0300ea0703090509025802a1b0f80000
...
```

The `raw_hex` column is the unparsed Glucose Measurement characteristic
value so nothing is lost if we want to re-parse later (sensor status,
sample location, etc.).

## Config

Edit constants at the top of `sugar.go`:

- `pin` — pairing passkey (on the back of the meter)
- `meterAddr` — Bluetooth MAC
- `tsvPath` — output file path
- `syncEvery` — idle delay between sync attempts

## Reference

- [Silicon Labs AN982 — Bluetooth LE Glucose Sensor](https://www.silabs.com/documents/public/application-notes/AN982-Bluetooth-LE-Glucose-Sensor.pdf)
  — describes the Glucose Service characteristic layout, flags byte,
  SFLOAT encoding, and RACP op codes. The meter follows this spec exactly.
