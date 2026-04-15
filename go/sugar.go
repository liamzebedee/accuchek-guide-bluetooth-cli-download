// sugar.go
// Accu-Chek Guide Me — continuously download glucose records over BLE on Linux.
//
// Single file. Uses BlueZ via D-Bus for everything: registers an in-process
// Agent1 for passkey pairing, waits for the meter to advertise, connects,
// discovers services, reads via the standard Glucose Profile RACP flow,
// and appends new records to records.tsv.
//
// Setup:
//   go mod init sugar
//   go get github.com/godbus/dbus/v5
//   go build .
// Run (first time: put meter in pairing mode):
//   ./sugar

package main

import (
	"bufio"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/godbus/dbus/v5/introspect"
)

const (
	pin        = uint32(538425)
	meterAddr  = "04:47:07:32:14:BD"
	tsvPath    = "records.tsv"
	syncEvery  = 5 * time.Second
	advertWait = 120 * time.Second
)

const (
	agentPath = dbus.ObjectPath("/sugar/agent")

	uuidGlucoseService = "00001808-0000-1000-8000-00805f9b34fb"
	uuidMeasurement    = "00002a18-0000-1000-8000-00805f9b34fb"
	uuidRACP           = "00002a52-0000-1000-8000-00805f9b34fb"
)

var tsvHeader = []string{"sequence", "timestamp", "mmol_l", "mg_dl", "raw_hex"}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// ================================================================
// BlueZ Agent1: fixed-passkey pairing handler.
// ================================================================

type Agent struct{ passkey uint32 }

func (a *Agent) Release() *dbus.Error                           { return nil }
func (a *Agent) AuthorizeService(dbus.ObjectPath, string) *dbus.Error { return nil }
func (a *Agent) RequestAuthorization(dbus.ObjectPath) *dbus.Error     { return nil }
func (a *Agent) RequestConfirmation(dbus.ObjectPath, uint32) *dbus.Error { return nil }
func (a *Agent) DisplayPasskey(dbus.ObjectPath, uint32, uint16) *dbus.Error { return nil }
func (a *Agent) DisplayPinCode(dbus.ObjectPath, string) *dbus.Error         { return nil }
func (a *Agent) Cancel() *dbus.Error                                        { return nil }

func (a *Agent) RequestPinCode(device dbus.ObjectPath) (string, *dbus.Error) {
	fmt.Printf("[agent] RequestPinCode %s -> %d\n", device, a.passkey)
	return fmt.Sprintf("%06d", a.passkey), nil
}

func (a *Agent) RequestPasskey(device dbus.ObjectPath) (uint32, *dbus.Error) {
	fmt.Printf("[agent] RequestPasskey %s -> %d\n", device, a.passkey)
	return a.passkey, nil
}

const agent1IntrospectXML = `
<interface name="org.bluez.Agent1">
  <method name="Release"/>
  <method name="RequestPinCode">
    <arg name="device" type="o" direction="in"/>
    <arg name="pincode" type="s" direction="out"/>
  </method>
  <method name="DisplayPinCode">
    <arg name="device" type="o" direction="in"/>
    <arg name="pincode" type="s" direction="in"/>
  </method>
  <method name="RequestPasskey">
    <arg name="device" type="o" direction="in"/>
    <arg name="passkey" type="u" direction="out"/>
  </method>
  <method name="DisplayPasskey">
    <arg name="device" type="o" direction="in"/>
    <arg name="passkey" type="u" direction="in"/>
    <arg name="entered" type="q" direction="in"/>
  </method>
  <method name="RequestConfirmation">
    <arg name="device" type="o" direction="in"/>
    <arg name="passkey" type="u" direction="in"/>
  </method>
  <method name="RequestAuthorization">
    <arg name="device" type="o" direction="in"/>
  </method>
  <method name="AuthorizeService">
    <arg name="device" type="o" direction="in"/>
    <arg name="uuid" type="s" direction="in"/>
  </method>
  <method name="Cancel"/>
</interface>`

func registerAgent(conn *dbus.Conn, passkey uint32) error {
	agent := &Agent{passkey: passkey}
	if err := conn.Export(agent, agentPath, "org.bluez.Agent1"); err != nil {
		return err
	}
	if err := conn.Export(
		introspect.Introspectable(agent1Introspectable()),
		agentPath,
		"org.freedesktop.DBus.Introspectable",
	); err != nil {
		return err
	}

	mgr := conn.Object("org.bluez", dbus.ObjectPath("/org/bluez"))
	// Unregister any previous registration of our path, ignore errors.
	mgr.Call("org.bluez.AgentManager1.UnregisterAgent", 0, agentPath)
	if err := mgr.Call("org.bluez.AgentManager1.RegisterAgent", 0, agentPath, "KeyboardOnly").Err; err != nil {
		return fmt.Errorf("RegisterAgent: %w", err)
	}
	if err := mgr.Call("org.bluez.AgentManager1.RequestDefaultAgent", 0, agentPath).Err; err != nil {
		return fmt.Errorf("RequestDefaultAgent: %w", err)
	}
	fmt.Println("[agent] registered + default")
	return nil
}

func agent1Introspectable() string {
	return `<node>` + agent1IntrospectXML + introspect.IntrospectDataString + `</node>`
}

func unregisterAgent(conn *dbus.Conn) {
	_ = conn.Object("org.bluez", "/org/bluez").
		Call("org.bluez.AgentManager1.UnregisterAgent", 0, agentPath).Err
}

// ================================================================
// BlueZ object helpers
// ================================================================

func managedObjects(conn *dbus.Conn) (map[dbus.ObjectPath]map[string]map[string]dbus.Variant, error) {
	var objs map[dbus.ObjectPath]map[string]map[string]dbus.Variant
	err := conn.Object("org.bluez", "/").Call(
		"org.freedesktop.DBus.ObjectManager.GetManagedObjects", 0,
	).Store(&objs)
	return objs, err
}

func variantStr(v dbus.Variant) string {
	if v.Value() == nil {
		return ""
	}
	if s, ok := v.Value().(string); ok {
		return s
	}
	return ""
}

func variantBool(v dbus.Variant) bool {
	if b, ok := v.Value().(bool); ok {
		return b
	}
	return false
}

func findAdapter(conn *dbus.Conn) (dbus.ObjectPath, error) {
	objs, err := managedObjects(conn)
	if err != nil {
		return "", err
	}
	for path, ifs := range objs {
		if _, ok := ifs["org.bluez.Adapter1"]; ok {
			return path, nil
		}
	}
	return "", fmt.Errorf("no BlueZ adapter")
}

func findDeviceByAddr(conn *dbus.Conn, addr string) (dbus.ObjectPath, map[string]dbus.Variant, error) {
	objs, err := managedObjects(conn)
	if err != nil {
		return "", nil, err
	}
	for path, ifs := range objs {
		dev, ok := ifs["org.bluez.Device1"]
		if !ok {
			continue
		}
		if strings.EqualFold(variantStr(dev["Address"]), addr) {
			return path, dev, nil
		}
	}
	return "", nil, nil
}

// ================================================================
// Pairing flow (first run only, when Paired=false)
// ================================================================

func ensurePaired(conn *dbus.Conn) error {
	adapter, err := findAdapter(conn)
	if err != nil {
		return err
	}

	// Discover the device if BlueZ doesn't know it yet.
	devPath, dev, _ := findDeviceByAddr(conn, meterAddr)
	if devPath == "" {
		fmt.Println("meter not cached — starting discovery")
		_ = conn.Object("org.bluez", adapter).Call("org.bluez.Adapter1.StartDiscovery", 0).Err
		defer conn.Object("org.bluez", adapter).Call("org.bluez.Adapter1.StopDiscovery", 0)

		deadline := time.Now().Add(60 * time.Second)
		for time.Now().Before(deadline) {
			devPath, dev, _ = findDeviceByAddr(conn, meterAddr)
			if devPath != "" {
				break
			}
			time.Sleep(300 * time.Millisecond)
		}
		if devPath == "" {
			return fmt.Errorf("meter %s did not appear in 60s — put it in pairing mode", meterAddr)
		}
	}

	if variantBool(dev["Paired"]) {
		return nil
	}

	fmt.Printf("pairing %s (passkey %d)…\n", devPath, pin)
	call := conn.Object("org.bluez", devPath).Call("org.bluez.Device1.Pair", 0)
	if call.Err != nil {
		return fmt.Errorf("Pair: %w", call.Err)
	}

	// Trust so BlueZ auto-accepts future reconnects.
	props := conn.Object("org.bluez", devPath)
	_ = props.Call("org.freedesktop.DBus.Properties.Set", 0,
		"org.bluez.Device1", "Trusted", dbus.MakeVariant(true)).Err

	fmt.Println("paired + trusted.")
	return nil
}

// ================================================================
// Advertise-wait + Connect + service discovery
// ================================================================

func deviceProp(conn *dbus.Conn, devPath dbus.ObjectPath, name string) (dbus.Variant, error) {
	var v dbus.Variant
	err := conn.Object("org.bluez", devPath).Call(
		"org.freedesktop.DBus.Properties.Get", 0,
		"org.bluez.Device1", name,
	).Store(&v)
	return v, err
}

// waitAndConnect starts discovery, and for each advertisement from devPath
// tries Connect(). Keeps retrying on transient errors (connection-abort,
// no-reply) until success or total timeout.
func waitAndConnect(conn *dbus.Conn, devPath dbus.ObjectPath, timeout time.Duration) error {
	adapter, err := findAdapter(conn)
	if err != nil {
		return err
	}

	_ = conn.Object("org.bluez", adapter).Call("org.bluez.Adapter1.StartDiscovery", 0).Err
	defer conn.Object("org.bluez", adapter).Call("org.bluez.Adapter1.StopDiscovery", 0)

	rule := fmt.Sprintf(
		"type='signal',interface='org.freedesktop.DBus.Properties',member='PropertiesChanged',path='%s'",
		devPath,
	)
	if err := conn.BusObject().Call("org.freedesktop.DBus.AddMatch", 0, rule).Err; err != nil {
		return err
	}
	defer conn.BusObject().Call("org.freedesktop.DBus.RemoveMatch", 0, rule)

	sigCh := make(chan *dbus.Signal, 32)
	conn.Signal(sigCh)
	defer conn.RemoveSignal(sigCh)

	// Drain any stale signals.
	drainDeadline := time.After(50 * time.Millisecond)
drain:
	for {
		select {
		case <-sigCh:
		case <-drainDeadline:
			break drain
		}
	}

	overall := time.After(timeout)
	for {
		// Wait for an advertisement (RSSI or ManufacturerData change on the device).
		gotAdv := false
		for !gotAdv {
			select {
			case sig := <-sigCh:
				if sig == nil || sig.Name != "org.freedesktop.DBus.Properties.PropertiesChanged" {
					continue
				}
				if sig.Path != devPath || len(sig.Body) < 2 {
					continue
				}
				iface, _ := sig.Body[0].(string)
				if iface != "org.bluez.Device1" {
					continue
				}
				changed, _ := sig.Body[1].(map[string]dbus.Variant)
				if _, ok := changed["RSSI"]; ok {
					gotAdv = true
				} else if _, ok := changed["ManufacturerData"]; ok {
					gotAdv = true
				} else if c, ok := changed["Connected"]; ok && variantBool(c) {
					return nil
				}
			case <-overall:
				return fmt.Errorf("no successful connect within %s", timeout)
			}
		}

		fmt.Println("advertisement seen — Connect()")
		err := conn.Object("org.bluez", devPath).
			Call("org.bluez.Device1.Connect", 0).Err
		if err == nil {
			return nil
		}
		msg := err.Error()
		fmt.Printf("  Connect failed: %s — waiting for next advertisement\n", msg)
		// Transient: loop and wait for another advertisement.
	}
}

func connectDevice(conn *dbus.Conn, devPath dbus.ObjectPath) error {
	return conn.Object("org.bluez", devPath).Call("org.bluez.Device1.Connect", 0).Err
}

func disconnectDevice(conn *dbus.Conn, devPath dbus.ObjectPath) {
	_ = conn.Object("org.bluez", devPath).Call("org.bluez.Device1.Disconnect", 0).Err
}

func waitServicesResolved(conn *dbus.Conn, devPath dbus.ObjectPath, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		v, err := deviceProp(conn, devPath, "ServicesResolved")
		if err == nil && variantBool(v) {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("services not resolved within %s", timeout)
}

// findGlucoseChars locates the GM + RACP characteristics under the
// Glucose Service (0x1808). The meter exposes a second RACP under a
// Roche vendor service, so filtering by UUID alone is ambiguous —
// we must scope to the Glucose Service path.
func findGlucoseChars(conn *dbus.Conn, devPath dbus.ObjectPath) (gm, racp dbus.ObjectPath, err error) {
	objs, err := managedObjects(conn)
	if err != nil {
		return "", "", err
	}
	var gsPath dbus.ObjectPath
	prefix := string(devPath) + "/"
	for p, ifs := range objs {
		if !strings.HasPrefix(string(p), prefix) {
			continue
		}
		svc, ok := ifs["org.bluez.GattService1"]
		if !ok {
			continue
		}
		if strings.ToLower(variantStr(svc["UUID"])) == uuidGlucoseService {
			gsPath = p
			break
		}
	}
	if gsPath == "" {
		return "", "", fmt.Errorf("Glucose service not found")
	}
	svcPrefix := string(gsPath) + "/"
	for p, ifs := range objs {
		if !strings.HasPrefix(string(p), svcPrefix) {
			continue
		}
		c, ok := ifs["org.bluez.GattCharacteristic1"]
		if !ok {
			continue
		}
		switch strings.ToLower(variantStr(c["UUID"])) {
		case uuidMeasurement:
			gm = p
		case uuidRACP:
			racp = p
		}
	}
	if gm == "" || racp == "" {
		return "", "", fmt.Errorf("missing chars: gm=%q racp=%q", gm, racp)
	}
	return gm, racp, nil
}

// ================================================================
// Glucose Measurement parsing (BLE Glucose Profile)
// ================================================================

type gmRec struct {
	seq     uint16
	ts      string
	mmol    float64
	mgdl    float64
	hasConc bool
}

func parseSFLOAT(raw uint16) float64 {
	m := int32(raw & 0x0FFF)
	e := int32((raw >> 12) & 0x0F)
	if m >= 0x0800 {
		m -= 0x1000
	}
	if e >= 0x08 {
		e -= 0x10
	}
	f := 1.0
	if e >= 0 {
		for i := int32(0); i < e; i++ {
			f *= 10
		}
	} else {
		for i := int32(0); i < -e; i++ {
			f /= 10
		}
	}
	return float64(m) * f
}

func parseGM(d []byte) gmRec {
	if len(d) < 10 {
		return gmRec{}
	}
	flags := d[0]
	seq := binary.LittleEndian.Uint16(d[1:3])
	year := binary.LittleEndian.Uint16(d[3:5])
	month, day := d[5], d[6]
	hour, minute, second := d[7], d[8], d[9]
	off := 10
	if flags&0x01 != 0 {
		off += 2
	}
	r := gmRec{seq: seq}
	if year > 0 {
		r.ts = fmt.Sprintf("%04d-%02d-%02d %02d:%02d:%02d",
			year, month, day, hour, minute, second)
	}
	if flags&0x02 != 0 && off+2 <= len(d) {
		raw := binary.LittleEndian.Uint16(d[off : off+2])
		conc := parseSFLOAT(raw)
		if flags&0x04 != 0 {
			r.mmol = conc * 1000
			r.mgdl = r.mmol * 18.0182
		} else {
			r.mgdl = conc * 1e5
			r.mmol = r.mgdl / 18.0182
		}
		r.hasConc = true
	}
	return r
}

// ================================================================
// TSV persistence
// ================================================================

func loadLastSeq(path string) (int, error) {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return -1, nil
	}
	if err != nil {
		return -1, err
	}
	defer f.Close()
	mx := -1
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	first := true
	for sc.Scan() {
		line := sc.Text()
		if first {
			first = false
			if strings.HasPrefix(line, "sequence\t") {
				continue
			}
		}
		parts := strings.SplitN(line, "\t", 2)
		if len(parts) < 1 {
			continue
		}
		n, err := strconv.Atoi(parts[0])
		if err != nil {
			continue
		}
		if n > mx {
			mx = n
		}
	}
	return mx, sc.Err()
}

func openAppendTSV(path string) (*os.File, error) {
	_, statErr := os.Stat(path)
	existed := statErr == nil
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return nil, err
	}
	if !existed {
		fmt.Fprintln(f, strings.Join(tsvHeader, "\t"))
	}
	return f, nil
}

func appendRecord(f *os.File, r gmRec, raw []byte) error {
	mmol, mgdl := "", ""
	if r.hasConc {
		mmol = fmt.Sprintf("%.2f", r.mmol)
		mgdl = fmt.Sprintf("%.1f", r.mgdl)
	}
	_, err := fmt.Fprintf(f, "%d\t%s\t%s\t%s\t%s\n",
		r.seq, r.ts, mmol, mgdl, hex.EncodeToString(raw))
	return err
}

// ================================================================
// Download flow (subscribe + RACP)
// ================================================================

func download(conn *dbus.Conn, gm, racp dbus.ObjectPath, lastSeq int, tsv *os.File) (int, int, error) {
	// Subscribe to PropertiesChanged on both chars BEFORE StartNotify.
	for _, p := range []dbus.ObjectPath{gm, racp} {
		rule := fmt.Sprintf(
			"type='signal',interface='org.freedesktop.DBus.Properties',member='PropertiesChanged',path='%s'",
			p,
		)
		if err := conn.BusObject().Call("org.freedesktop.DBus.AddMatch", 0, rule).Err; err != nil {
			return 0, lastSeq, err
		}
		defer conn.BusObject().Call("org.freedesktop.DBus.RemoveMatch", 0, rule)
	}
	sigCh := make(chan *dbus.Signal, 128)
	conn.Signal(sigCh)
	defer conn.RemoveSignal(sigCh)

	if err := conn.Object("org.bluez", gm).Call("org.bluez.GattCharacteristic1.StartNotify", 0).Err; err != nil {
		return 0, lastSeq, fmt.Errorf("StartNotify gm: %w", err)
	}
	defer conn.Object("org.bluez", gm).Call("org.bluez.GattCharacteristic1.StopNotify", 0)

	if err := conn.Object("org.bluez", racp).Call("org.bluez.GattCharacteristic1.StartNotify", 0).Err; err != nil {
		return 0, lastSeq, fmt.Errorf("StartNotify racp: %w", err)
	}
	defer conn.Object("org.bluez", racp).Call("org.bluez.GattCharacteristic1.StopNotify", 0)

	// Build RACP command.
	var cmd []byte
	if lastSeq < 0 {
		fmt.Println("RACP: report all records")
		cmd = []byte{0x01, 0x01}
	} else {
		next := uint16(lastSeq + 1)
		fmt.Printf("RACP: report records seq >= %d\n", next)
		cmd = []byte{0x01, 0x03, 0x01, byte(next & 0xff), byte(next >> 8)}
	}

	opts := map[string]dbus.Variant{"type": dbus.MakeVariant("request")}
	if err := conn.Object("org.bluez", racp).Call(
		"org.bluez.GattCharacteristic1.WriteValue", 0, cmd, opts,
	).Err; err != nil {
		return 0, lastSeq, fmt.Errorf("WriteValue racp: %w", err)
	}

	var mu sync.Mutex
	records := 0
	maxSeq := lastSeq
	done := make(chan struct{})
	var doneOnce sync.Once

	go func() {
		for sig := range sigCh {
			if sig == nil || sig.Name != "org.freedesktop.DBus.Properties.PropertiesChanged" {
				continue
			}
			if len(sig.Body) < 2 {
				continue
			}
			iface, _ := sig.Body[0].(string)
			if iface != "org.bluez.GattCharacteristic1" {
				continue
			}
			changed, _ := sig.Body[1].(map[string]dbus.Variant)
			val, ok := changed["Value"]
			if !ok {
				continue
			}
			b, ok := val.Value().([]byte)
			if !ok {
				continue
			}

			switch sig.Path {
			case gm:
				rec := parseGM(b)
				mu.Lock()
				records++
				if int(rec.seq) > maxSeq {
					maxSeq = int(rec.seq)
				}
				mu.Unlock()
				ts := rec.ts
				if ts == "" {
					ts = "????"
				}
				if rec.hasConc {
					fmt.Printf("  #%-4d %s  %5.2f mmol/L  (%5.1f mg/dL)\n",
						rec.seq, ts, rec.mmol, rec.mgdl)
				} else {
					fmt.Printf("  #%-4d %s  (no conc)\n", rec.seq, ts)
				}
				if err := appendRecord(tsv, rec, b); err != nil {
					fmt.Fprintln(os.Stderr, "tsv:", err)
				}
			case racp:
				if len(b) >= 3 && b[0] == 0x06 {
					fmt.Printf("  RACP done: req_op=%d code=%d\n", b[1], b[2])
					doneOnce.Do(func() { close(done) })
				} else if len(b) >= 3 && b[0] == 0x05 {
					n := binary.LittleEndian.Uint16(b[1:3])
					fmt.Printf("  RACP count: %d\n", n)
				}
			}
		}
	}()

	select {
	case <-done:
	case <-time.After(120 * time.Second):
		fmt.Println("RACP timeout")
	}

	mu.Lock()
	n, m := records, maxSeq
	mu.Unlock()
	return n, m, nil
}

// ================================================================
// Sync orchestration
// ================================================================

func syncOnce(conn *dbus.Conn, lastSeq *int, tsv *os.File) error {
	if err := ensurePaired(conn); err != nil {
		return fmt.Errorf("pair: %w", err)
	}

	devPath, _, err := findDeviceByAddr(conn, meterAddr)
	if err != nil {
		return err
	}
	if devPath == "" {
		return fmt.Errorf("device disappeared after pairing?")
	}

	connV, _ := deviceProp(conn, devPath, "Connected")
	if !variantBool(connV) {
		fmt.Println("waiting for meter to advertise (press a button on the meter)…")
		if err := waitAndConnect(conn, devPath, advertWait); err != nil {
			return err
		}
		fmt.Println("connected.")
	}
	defer disconnectDevice(conn, devPath)

	if err := waitServicesResolved(conn, devPath, 20*time.Second); err != nil {
		return err
	}
	gm, racp, err := findGlucoseChars(conn, devPath)
	if err != nil {
		return err
	}
	fmt.Printf("gm:   %s\nracp: %s\n", gm, racp)

	n, maxSeq, err := download(conn, gm, racp, *lastSeq, tsv)
	if err != nil {
		return err
	}
	if maxSeq > *lastSeq {
		*lastSeq = maxSeq
	}
	fmt.Printf("%d new records (last seq = %d)\n", n, *lastSeq)
	return nil
}

// ================================================================
// main
// ================================================================

func main() {
	conn, err := dbus.SystemBus()
	must(err)
	defer conn.Close()

	must(registerAgent(conn, pin))
	defer unregisterAgent(conn)

	lastSeq, err := loadLastSeq(tsvPath)
	must(err)
	fmt.Printf("starting sync loop. last known seq: %d\n", lastSeq)

	tsv, err := openAppendTSV(tsvPath)
	must(err)
	defer tsv.Close()

	for {
		if err := syncOnce(conn, &lastSeq, tsv); err != nil {
			fmt.Fprintln(os.Stderr, "sync error:", err)
		}
		time.Sleep(syncEvery)
	}
}
