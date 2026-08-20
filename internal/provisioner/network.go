package provisioner

import (
	"fmt"
	"sync"
)

// vmCIDR is the parent range carved into per-microVM /30 subnets. Each slot n
// gets 172.16.n.0/30: host (gateway) .1, guest .2. A single NAT masquerade rule
// for the whole range covers every microVM.
const vmCIDR = "172.16.0.0/16"

// natTable is the dedicated nftables table firerunner manages, isolated from any
// other host firewall rules.
const natTable = "firerunner"

// vmNet is the fully-resolved per-microVM network, derived purely from a slot.
type vmNet struct {
	slot     int
	tap      string
	hostIP   string // gateway, on the host side of the tap
	guestIP  string
	netmask  string
	guestMAC string
}

// slotNet returns the network parameters for a slot. It is pure so the IP/MAC
// derivation can be unit-tested.
func slotNet(slot int, tapPrefix string) vmNet {
	return vmNet{
		slot:     slot,
		tap:      fmt.Sprintf("%s%d", tapPrefix, slot),
		hostIP:   fmt.Sprintf("172.16.%d.1", slot),
		guestIP:  fmt.Sprintf("172.16.%d.2", slot),
		netmask:  "255.255.255.252",
		guestMAC: fmt.Sprintf("06:00:AC:10:%02x:02", slot),
	}
}

// composeBootArgs appends the kernel IP-autoconfiguration clause so the guest
// boots with a static address, gateway and netmask without needing DHCP. Format:
// ip=<client>:<server>:<gw>:<netmask>:<hostname>:<device>:<autoconf>.
func composeBootArgs(base string, n vmNet) string {
	return fmt.Sprintf("%s ip=%s::%s:%s::eth0:off", base, n.guestIP, n.hostIP, n.netmask)
}

// natCommands returns the idempotent host commands that enable IPv4 forwarding
// and install a masquerade rule for the microVM range out the external
// interface. Flushing the chain before adding the rule keeps it idempotent
// across restarts. It is pure so the command sequence can be asserted in tests.
func natCommands(extIface string) [][]string {
	return [][]string{
		{"sysctl", "-w", "net.ipv4.ip_forward=1"},
		{"nft", "add", "table", "ip", natTable},
		{"nft", "add", "chain", "ip", natTable, "postrouting", "{", "type", "nat", "hook", "postrouting", "priority", "100", ";", "}"},
		{"nft", "flush", "chain", "ip", natTable, "postrouting"},
		{"nft", "add", "rule", "ip", natTable, "postrouting", "ip", "saddr", vmCIDR, "oifname", extIface, "masquerade"},
	}
}

// tapUpCommands brings up a per-microVM tap device with its host-side gateway IP.
func tapUpCommands(n vmNet) [][]string {
	return [][]string{
		{"ip", "tuntap", "add", "dev", n.tap, "mode", "tap"},
		{"ip", "addr", "add", n.hostIP + "/30", "dev", n.tap},
		{"ip", "link", "set", n.tap, "up"},
	}
}

// tapDownCommands removes a tap device.
func tapDownCommands(tap string) [][]string {
	return [][]string{{"ip", "link", "del", tap}}
}

// ipam hands out and reclaims network slots, bounding concurrent microVMs and
// guaranteeing non-overlapping per-VM subnets.
type ipam struct {
	mu   sync.Mutex
	free []int
}

func newIPAM(size int) *ipam {
	free := make([]int, size)
	for i := range free {
		free[i] = i
	}
	return &ipam{free: free}
}

// acquire reserves a slot, returning false when the pool is exhausted.
func (p *ipam) acquire() (int, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.free) == 0 {
		return 0, false
	}
	slot := p.free[len(p.free)-1]
	p.free = p.free[:len(p.free)-1]
	return slot, true
}

// release returns a slot to the pool.
func (p *ipam) release(slot int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.free = append(p.free, slot)
}
