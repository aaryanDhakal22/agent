// Package netscan is the Pi's view of its LAN for printers: the kernel ARP
// table (IP → MAC), the local IPv4 networks it is on, and a bounded TCP
// sweep of a /24 for ESC/POS printers (port 9100). Linux-only in practice
// (/proc/net/arp); elsewhere the ARP table just reads empty.
package netscan

import (
	"bufio"
	"context"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ARPPath is the kernel's IPv4 neighbour table.
const ARPPath = "/proc/net/arp"

// PrinterPort is the raw ESC/POS (JetDirect) port.
const PrinterPort = 9100

// Scan defaults: 32 dials at a time, 400 ms each — a silent /24 takes ~3 s.
const (
	DefaultConcurrency = 32
	DefaultDialTimeout = 400 * time.Millisecond
)

// atfCom is the "complete" ARP flag (an entry with a resolved MAC).
const atfCom = 0x2

// ParseARP reads /proc/net/arp into ip → mac (lower case). Incomplete
// entries (flag without ATF_COM) and the all-zero MAC are skipped.
//
//	IP address       HW type     Flags       HW address            Mask     Device
//	192.168.1.40     0x1         0x2         00:11:62:aa:bb:cc     *        eth0
func ParseARP(r io.Reader) map[string]string {
	out := map[string]string{}
	sc := bufio.NewScanner(r)
	first := true
	for sc.Scan() {
		if first { // header
			first = false
			continue
		}
		f := strings.Fields(sc.Text())
		if len(f) < 4 {
			continue
		}
		ip := net.ParseIP(f[0])
		if ip == nil || ip.To4() == nil {
			continue
		}
		flags, err := strconv.ParseUint(strings.TrimPrefix(f[2], "0x"), 16, 32)
		if err != nil || flags&atfCom == 0 {
			continue
		}
		mac := NormalizeMAC(f[3])
		if mac == "" {
			continue
		}
		out[ip.To4().String()] = mac
	}
	return out
}

// ReadARP reads the kernel ARP table (empty when there is none).
func ReadARP() (map[string]string, error) {
	f, err := os.Open(ARPPath)
	if err != nil {
		return map[string]string{}, err
	}
	defer f.Close()
	return ParseARP(f), nil
}

// NormalizeMAC returns a lower-case aa:bb:cc:dd:ee:ff, or "" for anything
// else (including the all-zero address of an incomplete entry).
func NormalizeMAC(s string) string {
	hw, err := net.ParseMAC(strings.TrimSpace(s))
	if err != nil || len(hw) != 6 {
		return ""
	}
	zero := true
	for _, b := range hw {
		if b != 0 {
			zero = false
		}
	}
	if zero {
		return ""
	}
	return hw.String()
}

// LocalNet is one IPv4 network this machine is on.
type LocalNet struct {
	Iface string
	IP    net.IP // this machine's address
	Net   *net.IPNet
}

// skipIface: virtual interfaces a printer never sits behind (dev machines
// have docker bridges; the Pi has tailscale).
func skipIface(name string) bool {
	for _, p := range []string{"docker", "br-", "veth", "virbr", "tailscale", "tun", "tap", "wg", "zt", "lo"} {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// LocalNets lists the up, non-loopback interfaces' private IPv4 networks.
func LocalNets() []LocalNet {
	ifs, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []LocalNet
	for _, ifc := range ifs {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 || skipIface(ifc.Name) {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip4 := ipn.IP.To4()
			if ip4 == nil || !ip4.IsPrivate() {
				continue
			}
			out = append(out, LocalNet{Iface: ifc.Name, IP: ip4, Net: &net.IPNet{IP: ip4.Mask(ipn.Mask), Mask: ipn.Mask}})
		}
	}
	return out
}

// OnLink reports whether ip is directly on one of the local networks (so its
// ARP entry is the device itself, not the gateway in front of it).
func OnLink(ip net.IP, nets []LocalNet) bool {
	ip4 := ip.To4()
	if ip4 == nil {
		return false
	}
	for _, n := range nets {
		if n.Net.Contains(ip4) {
			return true
		}
	}
	return false
}

// HostOf strips a :port (fake printers in tests dial host:port).
func HostOf(addr string) string {
	if h, _, err := net.SplitHostPort(addr); err == nil {
		return h
	}
	return addr
}

// maxNets caps how many networks one sweep covers.
const maxNets = 4

// SweepHosts lists the addresses to sweep: for each local network, the /24
// around this machine's address (only the part inside the network when the
// network is smaller), minus .0, .255 and this machine.
func SweepHosts(nets []LocalNet) []string {
	seen := map[string]bool{}
	var out []string
	n := 0
	for _, ln := range nets {
		if n == maxNets {
			break
		}
		base := ln.IP.To4().Mask(net.CIDRMask(24, 32))
		key := base.String()
		if seen[key] {
			continue
		}
		seen[key] = true
		n++
		for i := 1; i < 255; i++ {
			h := net.IPv4(base[0], base[1], base[2], byte(i)).To4()
			if h.Equal(ln.IP) || !ln.Net.Contains(h) {
				continue
			}
			out = append(out, h.String())
		}
	}
	return out
}

// Dialer opens a TCP connection (net.Dialer in production, fakes in tests).
type Dialer func(ctx context.Context, addr string) error

// TCPDial connects and closes (a printer prints nothing for an empty session).
func TCPDial(timeout time.Duration) Dialer {
	return func(ctx context.Context, addr string) error {
		d := net.Dialer{Timeout: timeout}
		c, err := d.DialContext(ctx, "tcp", addr)
		if err != nil {
			return err
		}
		return c.Close()
	}
}

// Sweep dials port on every host, concurrency at a time, and returns the
// hosts that answered (in input order).
func Sweep(ctx context.Context, hosts []string, port int, concurrency int, dial Dialer) []string {
	if concurrency < 1 {
		concurrency = 1
	}
	ok := make([]bool, len(hosts))
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for i, h := range hosts {
		if ctx.Err() != nil {
			break
		}
		sem <- struct{}{}
		wg.Add(1)
		go func(i int, h string) {
			defer wg.Done()
			defer func() { <-sem }()
			if dial(ctx, net.JoinHostPort(h, strconv.Itoa(port))) == nil {
				ok[i] = true
			}
		}(i, h)
	}
	wg.Wait()
	var out []string
	for i, h := range hosts {
		if ok[i] {
			out = append(out, h)
		}
	}
	return out
}
