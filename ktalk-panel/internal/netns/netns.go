// Package netns manages Linux network namespace isolation for ktalk-core processes.
// Each client is launched inside a dedicated netns with:
//   - A veth pair: host-side "vh<id>" and ns-side "vt<id>"
//   - NAT (MASQUERADE) on the host side
//   - Optional tc-tbf traffic shaping on the host veth
//   - An isolated /etc/resolv.conf
//
// On non-Linux platforms (or when running without root) this package
// is a no-op — processes run in the default network namespace.
package netns

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
)

// Namespace represents a Linux network namespace created for a client.
type Namespace struct {
	// Name is the netns name, e.g. "ktalk-alice".
	Name string
	// HostVeth is the host-side veth interface name, e.g. "vhab12".
	HostVeth string
	// NsVeth is the ns-side veth interface name, e.g. "vtab12".
	NsVeth string
	// HostIP is the host veth IP, e.g. "10.200.1.1/30".
	HostIP string
	// NsIP is the ns veth IP, e.g. "10.200.1.2/30".
	NsIP string
	// SpeedMbps is the tc-tbf rate in Mbit/s. 0 = unlimited.
	SpeedMbps int
}

// Available returns true if network namespace management is supported
// (Linux + root).
func Available() bool {
	if runtime.GOOS != "linux" {
		return false
	}
	return os.Getuid() == 0
}

// Create creates a new network namespace with the given configuration.
func Create(ns Namespace) error {
	if !Available() {
		return nil
	}

	cmds := [][]string{
		{"ip", "netns", "add", ns.Name},
		{"ip", "link", "add", ns.HostVeth, "type", "veth", "peer", "name", ns.NsVeth},
		{"ip", "link", "set", ns.NsVeth, "netns", ns.Name},
		{"ip", "addr", "add", ns.HostIP, "dev", ns.HostVeth},
		{"ip", "link", "set", ns.HostVeth, "up"},
		{"ip", "netns", "exec", ns.Name, "ip", "addr", "add", ns.NsIP, "dev", ns.NsVeth},
		{"ip", "netns", "exec", ns.Name, "ip", "link", "set", ns.NsVeth, "up"},
		{"ip", "netns", "exec", ns.Name, "ip", "link", "set", "lo", "up"},
		// Default route inside netns points to host veth
		{"ip", "netns", "exec", ns.Name, "ip", "route", "add", "default", "via", gatewayIP(ns.HostIP)},
		// NAT on host
		{"iptables", "-t", "nat", "-A", "POSTROUTING", "-s", nsSubnet(ns.NsIP), "-j", "MASQUERADE"},
		{"sysctl", "-w", "net.ipv4.ip_forward=1"},
	}

	for _, args := range cmds {
		if err := run(args...); err != nil {
			// Attempt cleanup on failure
			_ = Delete(ns)
			return fmt.Errorf("netns create: %v: %w", args, err)
		}
	}

	// Write resolv.conf inside netns
	resolvPath := fmt.Sprintf("/etc/netns/%s/resolv.conf", ns.Name)
	if err := os.MkdirAll(fmt.Sprintf("/etc/netns/%s", ns.Name), 0755); err == nil {
		_ = os.WriteFile(resolvPath, []byte("nameserver 1.1.1.1\nnameserver 8.8.8.8\n"), 0644)
	}

	// tc traffic shaping if speed limit set
	if ns.SpeedMbps > 0 {
		rate := fmt.Sprintf("%dmbit", ns.SpeedMbps)
		_ = run("tc", "qdisc", "add", "dev", ns.HostVeth, "root", "tbf",
			"rate", rate, "burst", "32kbit", "latency", "400ms")
	}

	return nil
}

// Delete removes the network namespace and associated resources.
func Delete(ns Namespace) error {
	if !Available() {
		return nil
	}
	// Remove tc qdisc (ignore error — may not exist)
	_ = run("tc", "qdisc", "del", "dev", ns.HostVeth, "root")
	// Remove NAT rule
	_ = run("iptables", "-t", "nat", "-D", "POSTROUTING", "-s", nsSubnet(ns.NsIP), "-j", "MASQUERADE")
	// Remove veth (also removes ns-side)
	_ = run("ip", "link", "del", ns.HostVeth)
	// Remove netns
	_ = run("ip", "netns", "del", ns.Name)
	// Remove resolv.conf
	_ = os.RemoveAll(fmt.Sprintf("/etc/netns/%s", ns.Name))
	return nil
}

// ExecArgs returns the ip-netns-exec prefix to run a command inside the namespace.
// Returns nil if Available() is false.
func ExecArgs(nsName string) []string {
	if !Available() || nsName == "" {
		return nil
	}
	return []string{"ip", "netns", "exec", nsName}
}

// TrafficBytes reads transmitted bytes from the host veth via /sys/class/net.
func TrafficBytes(hostVeth string) (uint64, error) {
	path := fmt.Sprintf("/sys/class/net/%s/statistics/tx_bytes", hostVeth)
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	var n uint64
	_, err = fmt.Sscanf(strings.TrimSpace(string(data)), "%d", &n)
	return n, err
}

func run(args ...string) error {
	cmd := exec.Command(args[0], args[1:]...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w (output: %s)", strings.Join(args, " "), err, string(out))
	}
	return nil
}

// gatewayIP extracts the gateway IP from a CIDR like "10.200.1.1/30" → "10.200.1.1"
func gatewayIP(cidr string) string {
	if idx := strings.Index(cidr, "/"); idx >= 0 {
		return cidr[:idx]
	}
	return cidr
}

// nsSubnet converts a namespace IP like "10.200.1.2/30" to the /30 subnet.
func nsSubnet(nsIP string) string {
	// For simplicity return the /30 prefix based on the IP.
	// A production implementation would use net.ParseCIDR.
	if idx := strings.LastIndex(nsIP, "."); idx >= 0 {
		prefix := nsIP[:idx]
		if slash := strings.Index(nsIP, "/"); slash >= 0 {
			return prefix + ".0" + nsIP[slash:]
		}
	}
	return nsIP
}
