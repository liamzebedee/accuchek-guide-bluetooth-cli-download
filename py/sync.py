#!/usr/bin/env python3
"""Connect paired meter via BlueZ D-Bus and download glucose records."""

import os
import struct
import sys
import time
from datetime import datetime

import dbus
import dbus.mainloop.glib
from gi.repository import GLib

ADDR = "04:47:07:32:14:BD"
TSV = "/home/liam/Documents/projects/diabetes/sugar/records.tsv"

BUS = "org.bluez"
OM = "org.freedesktop.DBus.ObjectManager"
PROPS = "org.freedesktop.DBus.Properties"
ADAPTER_IFACE = "org.bluez.Adapter1"
DEVICE_IFACE = "org.bluez.Device1"
SVC_IFACE = "org.bluez.GattService1"
CHR_IFACE = "org.bluez.GattCharacteristic1"

GS_UUID = "00001808-0000-1000-8000-00805f9b34fb"
GM_UUID = "00002a18-0000-1000-8000-00805f9b34fb"
RACP_UUID = "00002a52-0000-1000-8000-00805f9b34fb"


def sfloat(raw: int) -> float:
    m = raw & 0x0FFF
    e = (raw >> 12) & 0x0F
    if m >= 0x0800: m -= 0x1000
    if e >= 0x08:   e -= 0x10
    return m * (10 ** e)


def parse_gm(data: bytes) -> dict:
    flags = data[0]
    seq = struct.unpack_from("<H", data, 1)[0]
    y, mo, d, h, mi, s = struct.unpack_from("<HBBBBB", data, 3)
    off = 10
    ts = None
    if y:
        try: ts = datetime(y, mo, d, h, mi, s)
        except ValueError: ts = None
    if flags & 0x01: off += 2
    conc = None; unit = None
    if flags & 0x02:
        raw = struct.unpack_from("<H", data, off)[0]
        conc = sfloat(raw)
        off += 1 + 2
        unit = "mol/L" if (flags & 0x04) else "kg/L"
    mmol = mgdl = None
    if conc is not None:
        if unit == "mol/L":
            mmol = conc * 1000.0
            mgdl = mmol * 18.0182
        else:
            mgdl = conc * 1e5
            mmol = mgdl / 18.0182
    return {"seq": seq, "ts": ts, "mmol": mmol, "mgdl": mgdl, "raw": data.hex()}


def load_last_seq(path):
    if not os.path.exists(path): return -1
    mx = -1
    with open(path) as f:
        for i, line in enumerate(f):
            if i == 0 and line.startswith("sequence\t"): continue
            parts = line.split("\t", 1)
            try:
                n = int(parts[0])
                if n > mx: mx = n
            except ValueError: pass
    return mx


def open_tsv(path):
    new = not os.path.exists(path)
    f = open(path, "a")
    if new:
        f.write("sequence\ttimestamp\tmmol_l\tmg_dl\traw_hex\n")
        f.flush()
    return f


def find_device(bus, addr):
    mgr = dbus.Interface(bus.get_object(BUS, "/"), OM)
    for path, ifs in mgr.GetManagedObjects().items():
        dev = ifs.get(DEVICE_IFACE)
        if dev and dev.get("Address", "").upper() == addr.upper():
            return path
    return None


def find_glucose_chars(bus, dev_path):
    """Return (gm_path, racp_path) under the Glucose Service on this device."""
    mgr = dbus.Interface(bus.get_object(BUS, "/"), OM)
    objs = mgr.GetManagedObjects()
    # 1) find the Glucose service's path under this device
    gs_path = None
    for p, ifs in objs.items():
        if not p.startswith(dev_path + "/"): continue
        svc = ifs.get(SVC_IFACE)
        if svc and str(svc.get("UUID", "")).lower() == GS_UUID:
            gs_path = p
            break
    if not gs_path:
        return None, None
    # 2) chars under that service
    gm = racp = None
    for p, ifs in objs.items():
        if not p.startswith(gs_path + "/"): continue
        ch = ifs.get(CHR_IFACE)
        if not ch: continue
        u = str(ch.get("UUID", "")).lower()
        if u == GM_UUID: gm = p
        elif u == RACP_UUID: racp = p
    return gm, racp


def main():
    dbus.mainloop.glib.DBusGMainLoop(set_as_default=True)
    bus = dbus.SystemBus()

    last = load_last_seq(TSV)
    print(f"last known seq: {last}", flush=True)
    tsv = open_tsv(TSV)

    dev_path = find_device(bus, ADDR)
    if not dev_path:
        print("device not known to BlueZ — run pair.py first", flush=True)
        sys.exit(2)
    print(f"device path: {dev_path}", flush=True)

    device = dbus.Interface(bus.get_object(BUS, dev_path), DEVICE_IFACE)
    props = dbus.Interface(bus.get_object(BUS, dev_path), PROPS)

    # Find adapter for discovery control.
    adapter_path = "/org/bluez/hci0"
    adapter_if = dbus.Interface(bus.get_object(BUS, adapter_path), ADAPTER_IFACE)

    connected = bool(props.Get(DEVICE_IFACE, "Connected"))
    if not connected:
        print("waiting for meter to advertise (start discovery)…", flush=True)
        print("  >>> wake the meter (press a button) <<<", flush=True)
        try: adapter_if.StartDiscovery()
        except dbus.exceptions.DBusException: pass

        wait_loop = GLib.MainLoop()
        state_adv = {"seen": False}

        def on_dev_props(iface, changed, _invalidated, path=None):
            if iface != DEVICE_IFACE: return
            if path != dev_path: return
            # Any new advertisement bumps RSSI.
            if "RSSI" in changed or "ManufacturerData" in changed or "Connected" in changed:
                state_adv["seen"] = True
                wait_loop.quit()

        bus.add_signal_receiver(
            on_dev_props, dbus_interface=PROPS, signal_name="PropertiesChanged",
            path=dev_path, path_keyword="path",
        )
        GLib.timeout_add(120_000, lambda: (wait_loop.quit(), False)[1])
        wait_loop.run()

        try: adapter_if.StopDiscovery()
        except Exception: pass

        if not state_adv["seen"]:
            print("meter did not advertise within 120s", flush=True)
            sys.exit(3)

        print("advertising detected, calling Connect()…", flush=True)
        t0 = time.time()
        try:
            device.Connect(timeout=45)
        except dbus.exceptions.DBusException as e:
            print(f"Connect failed: {e}", flush=True)
            sys.exit(3)
        print(f"  connected in {time.time()-t0:.1f}s", flush=True)
    else:
        print("already connected.", flush=True)

    # Wait for services resolved.
    deadline = time.time() + 20
    while time.time() < deadline:
        if bool(props.Get(DEVICE_IFACE, "ServicesResolved")):
            break
        time.sleep(0.2)
    print("services resolved.", flush=True)

    gm_path, racp_path = find_glucose_chars(bus, dev_path)
    if not gm_path or not racp_path:
        print(f"missing chars: gm={gm_path} racp={racp_path}", flush=True)
        sys.exit(4)
    print(f"gm:   {gm_path}\nracp: {racp_path}", flush=True)

    gm_chr = dbus.Interface(bus.get_object(BUS, gm_path), CHR_IFACE)
    racp_chr = dbus.Interface(bus.get_object(BUS, racp_path), CHR_IFACE)

    loop = GLib.MainLoop()
    state = {"records": 0, "done": False}

    def on_props(iface, changed, invalidated, path=None):
        if iface != CHR_IFACE: return
        val = changed.get("Value")
        if val is None: return
        b = bytes(val)
        if path == gm_path:
            r = parse_gm(b)
            state["records"] += 1
            ts = r["ts"].isoformat(sep=" ") if r["ts"] else ""
            mm = f"{r['mmol']:.2f}" if r["mmol"] is not None else ""
            md = f"{r['mgdl']:.1f}" if r["mgdl"] is not None else ""
            print(f"  #{r['seq']:>4} {ts}  {mm} mmol/L  ({md} mg/dL)", flush=True)
            tsv.write(f"{r['seq']}\t{ts}\t{mm}\t{md}\t{r['raw']}\n")
            tsv.flush()
        elif path == racp_path:
            print(f"  RACP <- {b.hex()}", flush=True)
            if len(b) >= 3 and b[0] == 0x06:
                print(f"  RACP done: req_op={b[1]} code={b[2]}", flush=True)
                state["done"] = True
                loop.quit()
            elif len(b) >= 3 and b[0] == 0x05:
                n = struct.unpack_from("<H", b, 1)[0]
                print(f"  RACP count: {n}", flush=True)

    bus.add_signal_receiver(
        on_props, dbus_interface=PROPS, signal_name="PropertiesChanged",
        path=gm_path, path_keyword="path",
    )
    bus.add_signal_receiver(
        on_props, dbus_interface=PROPS, signal_name="PropertiesChanged",
        path=racp_path, path_keyword="path",
    )

    gm_chr.StartNotify()
    racp_chr.StartNotify()
    print("notifications armed.", flush=True)

    if last < 0:
        cmd = [dbus.Byte(0x01), dbus.Byte(0x01)]
        print("RACP: report all records", flush=True)
    else:
        nxt = last + 1
        cmd = [dbus.Byte(0x01), dbus.Byte(0x03), dbus.Byte(0x01),
               dbus.Byte(nxt & 0xFF), dbus.Byte((nxt >> 8) & 0xFF)]
        print(f"RACP: report records seq >= {nxt}", flush=True)

    racp_chr.WriteValue(cmd, {"type": "request"})

    # Safety: also stop the loop after 120s no matter what.
    GLib.timeout_add(120_000, lambda: (loop.quit(), False)[1])
    loop.run()

    try: gm_chr.StopNotify()
    except Exception: pass
    try: racp_chr.StopNotify()
    except Exception: pass
    try: device.Disconnect()
    except Exception: pass

    tsv.close()
    print(f"\n{state['records']} new records", flush=True)


if __name__ == "__main__":
    main()
