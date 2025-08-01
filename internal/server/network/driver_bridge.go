package network

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"net"
	"net/http"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/mdlayher/netx/eui64"

	incus "github.com/lxc/incus/v6/client"
	"github.com/lxc/incus/v6/internal/server/apparmor"
	"github.com/lxc/incus/v6/internal/server/cluster"
	"github.com/lxc/incus/v6/internal/server/cluster/request"
	"github.com/lxc/incus/v6/internal/server/daemon"
	"github.com/lxc/incus/v6/internal/server/db"
	dbCluster "github.com/lxc/incus/v6/internal/server/db/cluster"
	"github.com/lxc/incus/v6/internal/server/db/warningtype"
	"github.com/lxc/incus/v6/internal/server/dnsmasq"
	"github.com/lxc/incus/v6/internal/server/dnsmasq/dhcpalloc"
	firewallDrivers "github.com/lxc/incus/v6/internal/server/firewall/drivers"
	"github.com/lxc/incus/v6/internal/server/ip"
	"github.com/lxc/incus/v6/internal/server/network/acl"
	addressset "github.com/lxc/incus/v6/internal/server/network/address-set"
	"github.com/lxc/incus/v6/internal/server/project"
	localUtil "github.com/lxc/incus/v6/internal/server/util"
	"github.com/lxc/incus/v6/internal/server/warnings"
	internalUtil "github.com/lxc/incus/v6/internal/util"
	"github.com/lxc/incus/v6/internal/version"
	"github.com/lxc/incus/v6/shared/api"
	"github.com/lxc/incus/v6/shared/logger"
	"github.com/lxc/incus/v6/shared/revert"
	"github.com/lxc/incus/v6/shared/subprocess"
	"github.com/lxc/incus/v6/shared/util"
	"github.com/lxc/incus/v6/shared/validate"
)

// Default MTU for bridge interface.
const bridgeMTUDefault = 1500

// bridge represents a bridge network.
type bridge struct {
	common
}

// DBType returns the network type DB ID.
func (n *bridge) DBType() db.NetworkType {
	return db.NetworkTypeBridge
}

// Config returns the network driver info.
func (n *bridge) Info() Info {
	info := n.common.Info()
	info.AddressForwards = true

	return info
}

// checkClusterWideMACSafe returns whether it is safe to use the same MAC address for the bridge interface on all
// cluster nodes. It is not suitable to use a static MAC address when "bridge.external_interfaces" is non-empty and
// the bridge interface has no IPv4 or IPv6 address set. This is because in a clustered environment the same bridge
// config is applied to all nodes, and if the bridge is being used to connect multiple nodes to the same network
// segment it would cause MAC conflicts to use the same MAC on all nodes. If an IP address is specified then
// connecting multiple nodes to the same network segment would also cause IP conflicts, so if an IP is defined
// then we assume this is not being done. However if IP addresses are explicitly set to "none" and
// "bridge.external_interfaces" is set then it may not be safe to use a the same MAC address on all nodes.
func (n *bridge) checkClusterWideMACSafe(config map[string]string) error {
	// We can't be sure that multiple clustered nodes aren't connected to the same network segment so don't
	// use a static MAC address for the bridge interface to avoid introducing a MAC conflict.
	if config["bridge.external_interfaces"] != "" && config["ipv4.address"] == "none" && config["ipv6.address"] == "none" {
		return errors.New(`Cannot use static "bridge.hwaddr" MAC address when bridge has no IP addresses and has external interfaces set`)
	}

	// We may have MAC conflicts if tunnels are in use.
	for k := range config {
		if strings.HasPrefix(k, "tunnel.") {
			return errors.New(`Cannot use static "bridge.hwaddr" MAC address when bridge has tunnels connected`)
		}
	}

	// If using a generated IPv6 address, we need a unique MAC.
	if config["ipv6.address"] != "none" && validate.IsNetworkV6(config["ipv6.address"]) == nil {
		return errors.New(`Cannot use static "bridge.hwaddr" MAC address when bridge uses a host-specific IPv6 address`)
	}

	return nil
}

// FillConfig fills requested config with any default values.
func (n *bridge) FillConfig(config map[string]string) error {
	// Set some default values where needed.
	if config["ipv4.address"] == "" {
		config["ipv4.address"] = "auto"
	}

	if config["ipv4.address"] == "auto" && config["ipv4.nat"] == "" {
		config["ipv4.nat"] = "true"
	}

	if config["ipv6.address"] == "" {
		content, err := os.ReadFile("/proc/sys/net/ipv6/conf/default/disable_ipv6")
		if err == nil && string(content) == "0\n" {
			config["ipv6.address"] = "auto"
		}
	}

	if config["ipv6.address"] == "auto" && config["ipv6.nat"] == "" {
		config["ipv6.nat"] = "true"
	}

	// Now replace any "auto" keys with generated values.
	err := n.populateAutoConfig(config)
	if err != nil {
		return fmt.Errorf("Failed generating auto config: %w", err)
	}

	return nil
}

// populateAutoConfig replaces "auto" in config with generated values.
func (n *bridge) populateAutoConfig(config map[string]string) error {
	changedConfig := false

	// Now populate "auto" values where needed.
	if config["ipv4.address"] == "auto" {
		subnet, err := randomSubnetV4()
		if err != nil {
			return err
		}

		config["ipv4.address"] = subnet
		changedConfig = true
	}

	if config["ipv6.address"] == "auto" {
		subnet, err := randomSubnetV6()
		if err != nil {
			return err
		}

		config["ipv6.address"] = subnet
		changedConfig = true
	}

	// Re-validate config if changed.
	if changedConfig && n.state != nil {
		return n.Validate(config)
	}

	return nil
}

// ValidateName validates network name.
func (n *bridge) ValidateName(name string) error {
	err := validate.IsInterfaceName(name)
	if err != nil {
		return err
	}

	// Apply common name validation that applies to all network types.
	return n.common.ValidateName(name)
}

// Validate network config.
func (n *bridge) Validate(config map[string]string) error {
	// Build driver specific rules dynamically.
	rules := map[string]func(value string) error{
		// gendoc:generate(entity=network_bridge, group=common, key=bgp.ipv4.nexthop)
		//
		// ---
		//  type: string
		//  condition: BGP server
		//  default: local address
		//  shortdesc: Override the next-hop for advertised prefixes
		"bgp.ipv4.nexthop": validate.Optional(validate.IsNetworkAddressV4),

		// gendoc:generate(entity=network_bridge, group=common, key=bgp.ipv6.nexthop)
		//
		// ---
		//  type: string
		//  condition: BGP server
		//  default: local address
		//  shortdesc: Override the next-hop for advertised prefixes
		"bgp.ipv6.nexthop": validate.Optional(validate.IsNetworkAddressV6),

		// gendoc:generate(entity=network_bridge, group=common, key=bridge.driver)
		//
		// ---
		//  type: string
		//  condition: -
		//  default: `native`
		//  shortdesc: Bridge driver: `native` or `openvswitch`
		"bridge.driver": validate.Optional(validate.IsOneOf("native", "openvswitch")),

		// gendoc:generate(entity=network_bridge, group=common, key=bridge.external_interfaces)
		//
		// ---
		//  type: string
		//  condition: -
		//  default: -
		//  shortdesc: Comma-separated list of unconfigured network interfaces to include in the bridge
		"bridge.external_interfaces": validate.Optional(validateExternalInterfaces),

		// gendoc:generate(entity=network_bridge, group=common, key=bridge.hwaddr)
		//
		// ---
		//  type: string
		//  condition: -
		//  default: -
		//  shortdesc: MAC address for the bridge
		"bridge.hwaddr": validate.Optional(validate.IsNetworkMAC),

		// gendoc:generate(entity=network_bridge, group=common, key=bridge.mtu)
		//
		// ---
		//  type: integer
		//  condition: -
		//  default: `1500`
		//  shortdesc: Bridge MTU (default varies if tunnel in use)
		"bridge.mtu": validate.Optional(validate.IsNetworkMTU),

		// gendoc:generate(entity=network_bridge, group=common, key=ipv4.address)
		//
		// ---
		//  type: string
		//  condition: standard mode
		//  default: - (initial value on creation: `auto`)
		//  shortdesc: IPv4 address for the bridge (use `none` to turn off IPv4 or `auto` to generate a new random unused subnet) (CIDR)
		"ipv4.address": validate.Optional(func(value string) error {
			if validate.IsOneOf("none", "auto")(value) == nil {
				return nil
			}

			return validate.IsNetworkAddressCIDRV4(value)
		}),

		// gendoc:generate(entity=network_bridge, group=common, key=ipv4.firewall)
		//
		// ---
		//  type: bool
		//  condition: IPv4 address
		//  default: `true`
		//  shortdesc: Whether to generate filtering firewall rules for this network
		"ipv4.firewall": validate.Optional(validate.IsBool),

		// gendoc:generate(entity=network_bridge, group=common, key=ipv4.nat)
		//
		// ---
		//  type: bool
		//  condition: IPv4 address
		//  default: `false`(initial value on creation if `ipv4.address` is set to `auto`: `true`)
		//  shortdesc: Whether to NAT
		"ipv4.nat": validate.Optional(validate.IsBool),

		// gendoc:generate(entity=network_bridge, group=common, key=ipv4.nat.order)
		//
		// ---
		//  type: string
		//  condition: IPv4 address
		//  default: `before`
		//  shortdesc: Whether to add the required NAT rules before or after any pre-existing rules
		"ipv4.nat.order": validate.Optional(validate.IsOneOf("before", "after")),

		// gendoc:generate(entity=network_bridge, group=common, key=ipv4.nat.address)
		//
		// ---
		//  type: string
		//  condition: IPv4 address
		//  default: -
		//  shortdesc: The source address used for outbound traffic from the bridge
		"ipv4.nat.address": validate.Optional(validate.IsNetworkAddressV4),

		// gendoc:generate(entity=network_bridge, group=common, key=ipv4.dhcp)
		//
		// ---
		//  type: bool
		//  condition: IPv4 address
		//  default: `true`
		//  shortdesc: Whether to allocate addresses using DHCP
		"ipv4.dhcp": validate.Optional(validate.IsBool),

		// gendoc:generate(entity=network_bridge, group=common, key=ipv4.dhcp.gateway)
		//
		// ---
		//  type: string
		//  condition: IPv4 DHCP
		//  default: IPv4 address
		//  shortdesc: Address of the gateway for the subnet
		"ipv4.dhcp.gateway": validate.Optional(validate.IsNetworkAddressV4),

		// gendoc:generate(entity=network_bridge, group=common, key=ipv4.dhcp.expiry)
		//
		// ---
		//  type: string
		//  condition: IPv4 DHCP
		//  default: `1h`
		//  shortdesc: When to expire DHCP leases
		"ipv4.dhcp.expiry": validate.IsAny,

		// gendoc:generate(entity=network_bridge, group=common, key=ipv4.dhcp.ranges)
		//
		// ---
		//  type: string
		//  condition: IPv4 DHCP
		//  default: all addresses
		//  shortdesc: Comma-separated list of IP ranges to use for DHCP (FIRST-LAST format)
		"ipv4.dhcp.ranges": validate.Optional(validate.IsListOf(validate.IsNetworkRangeV4)),

		// gendoc:generate(entity=network_bridge, group=common, key=ipv4.dhcp.routes)
		//
		// ---
		//  type: string
		//  condition: IPv4 DHCP
		//  default: -
		//  shortdesc: Static routes to provide via DHCP option 121, as a comma-separated list of alternating subnets (CIDR) and gateway addresses (same syntax as dnsmasq)
		"ipv4.dhcp.routes": validate.Optional(validate.IsDHCPRouteList),

		// gendoc:generate(entity=network_bridge, group=common, key=ipv4.routes)
		//
		// ---
		//  type: string
		//  condition: IPv4 address
		//  default: -
		//  shortdesc: Comma-separated list of additional IPv4 CIDR subnets to route to the bridge
		"ipv4.routes": validate.Optional(validate.IsListOf(validate.IsNetworkV4)),

		// gendoc:generate(entity=network_bridge, group=common, key=ipv4.routing)
		//
		// ---
		//  type: bool
		//  condition: IPv4 DHCP
		//  default: `true`
		//  shortdesc: Whether to route traffic in and out of the bridge
		"ipv4.routing": validate.Optional(validate.IsBool),

		// gendoc:generate(entity=network_bridge, group=common, key=ipv4.ovn.ranges)
		//
		// ---
		//  type: string
		//  condition: -
		//  default: -
		//  shortdesc: Comma-separated list of IPv4 ranges to use for child OVN network routers (FIRST-LAST format)
		"ipv4.ovn.ranges": validate.Optional(validate.IsListOf(validate.IsNetworkRangeV4)),

		// gendoc:generate(entity=network_bridge, group=common, key=ipv6.address)
		//
		// ---
		//  type: string
		//  condition: standard mode
		//  default: - (initial value on creation: `auto`)
		//  shortdesc: IPv6 address for the bridge (use `none` to turn off IPv6 or `auto` to generate a new random unused subnet) (CIDR)
		"ipv6.address": validate.Optional(func(value string) error {
			if validate.IsOneOf("none", "auto")(value) == nil {
				return nil
			}

			return validate.Or(validate.IsNetworkAddressCIDRV6, validate.IsNetworkV6)(value)
		}),

		// gendoc:generate(entity=network_bridge, group=common, key=ipv6.firewall)
		//
		// ---
		//  type: bool
		//  condition: IPv6 address
		//  default: `true`
		//  shortdesc: Whether to generate filtering firewall rules for this network
		"ipv6.firewall": validate.Optional(validate.IsBool),

		// gendoc:generate(entity=network_bridge, group=common, key=ipv6.nat)
		//
		// ---
		//  type: bool
		//  condition: IPv6 address
		//  default: `false` (initial value on creation if `ipv6.address` is set to `auto`: `true`)
		//  shortdesc: Whether to NAT
		"ipv6.nat": validate.Optional(validate.IsBool),

		// gendoc:generate(entity=network_bridge, group=common, key=ipv6.nat.order)
		//
		// ---
		//  type: string
		//  condition: IPv6 address
		//  default: `before`
		//  shortdesc: Whether to add the required NAT rules before or after any pre-existing rules
		"ipv6.nat.order": validate.Optional(validate.IsOneOf("before", "after")),

		// gendoc:generate(entity=network_bridge, group=common, key=ipv6.nat.address)
		//
		// ---
		//  type: string
		//  condition: IPv6 address
		//  default: -
		//  shortdesc: The source address used for outbound traffic from the bridge
		"ipv6.nat.address": validate.Optional(validate.IsNetworkAddressV6),

		// gendoc:generate(entity=network_bridge, group=common, key=ipv6.dhcp)
		//
		// ---
		//  type: bool
		//  condition: IPv6 DHCP
		//  default: `true`
		//  shortdesc: Whether to provide additional network configuration over DHCP
		"ipv6.dhcp": validate.Optional(validate.IsBool),

		// gendoc:generate(entity=network_bridge, group=common, key=ipv6.dhcp.expiry)
		//
		// ---
		//  type: string
		//  condition: IPv6 DHCP
		//  default: `1h`
		//  shortdesc: When to expire DHCP leases
		"ipv6.dhcp.expiry": validate.IsAny,

		// gendoc:generate(entity=network_bridge, group=common, key=ipv6.dhcp.stateful)
		//
		// ---
		//  type: bool
		//  condition: IPv6 DHCP
		//  default: `false`
		//  shortdesc: Whether to allocate addresses using DHCP
		"ipv6.dhcp.stateful": validate.Optional(validate.IsBool),

		// gendoc:generate(entity=network_bridge, group=common, key=ipv6.dhcp.ranges)
		//
		// ---
		//  type: string
		//  condition: IPv6 stateful DHCP
		//  default: all addresses
		//  shortdesc: Comma-separated list of IPv6 ranges to use for DHCP (FIRST-LAST format)
		"ipv6.dhcp.ranges": validate.Optional(validate.IsListOf(validate.IsNetworkRangeV6)),

		// gendoc:generate(entity=network_bridge, group=common, key=ipv6.routes)
		//
		// ---
		//  type: string
		//  condition: IPv6 address
		//  default: -
		//  shortdesc: Comma-separated list of additional IPv6 CIDR subnets to route to the bridge
		"ipv6.routes": validate.Optional(validate.IsListOf(validate.IsNetworkV6)),

		// gendoc:generate(entity=network_bridge, group=common, key=ipv6.routing)
		//
		// ---
		//  type: bool
		//  condition: IPv6 address
		//  default: `true`
		//  shortdesc: Whether to route traffic in and out of the bridge
		"ipv6.routing": validate.Optional(validate.IsBool),

		// gendoc:generate(entity=network_bridge, group=common, key=ipv6.ovn.ranges)
		//
		// ---
		//  type: string
		//  condition: -
		//  default: -
		//  shortdesc: Comma-separated list of IPv6 ranges to use for child OVN network routers (FIRST-LAST format)
		"ipv6.ovn.ranges": validate.Optional(validate.IsListOf(validate.IsNetworkRangeV6)),

		// gendoc:generate(entity=network_bridge, group=common, key=dns.nameservers)
		//
		// ---
		//  type: string
		//  condition: -
		//  default: IPv4 and IPv6 address
		//  shortdesc: DNS server IPs to advertise to DHCP clients and via Router Advertisements. Both IPv4 and IPv6 addresses get pushed via DHCP, and IPv6 addresses are also advertised as RDNSS via RA.
		"dns.nameservers": validate.Optional(validate.IsListOf(validate.IsNetworkAddress)),

		// gendoc:generate(entity=network_bridge, group=common, key=dns.domain)
		//
		// ---
		//  type: string
		//  condition: -
		//  default: `incus`
		//  shortdesc: Domain to advertise to DHCP clients and use for DNS resolution
		"dns.domain": validate.IsAny,

		// gendoc:generate(entity=network_bridge, group=common, key=dns.mode)
		//
		// ---
		//  type: string
		//  condition: -
		//  default: `managed`
		//  shortdesc: DNS registration mode: none for no DNS record, managed for Incus-generated static records or dynamic for client-generated records
		"dns.mode": validate.Optional(validate.IsOneOf("dynamic", "managed", "none")),

		// gendoc:generate(entity=network_bridge, group=common, key=dns.search)
		//
		// ---
		//  type: string
		//  condition: -
		//  default: -
		//  shortdesc: Full comma-separated domain search list, defaulting to `dns.domain` value
		"dns.search": validate.IsAny,

		// gendoc:generate(entity=network_bridge, group=common, key=dns.zone.forward)
		//
		// ---
		//  type: string
		//  condition: -
		//  default: `managed`
		//  shortdesc: Comma-separated list of DNS zone names for forward DNS records
		"dns.zone.forward": validate.IsAny,

		// gendoc:generate(entity=network_bridge, group=common, key=dns.zone.reverse.ipv4)
		//
		// ---
		//  type: string
		//  condition: -
		//  default: `managed`
		//  shortdesc: DNS zone name for IPv4 reverse DNS records
		"dns.zone.reverse.ipv4": validate.IsAny,

		// gendoc:generate(entity=network_bridge, group=common, key=dns.zone.reverse.ipv6)
		//
		// ---
		//  type: string
		//  condition: -
		//  default: `managed`
		//  shortdesc: DNS zone name for IPv6 reverse DNS records
		"dns.zone.reverse.ipv6": validate.IsAny,

		// gendoc:generate(entity=network_bridge, group=common, key=raw.dnsmasq)
		//
		// ---
		//  type: string
		//  condition: -
		//  default: -
		//  shortdesc: Additional dnsmasq configuration to append to the configuration file
		"raw.dnsmasq": validate.IsAny,

		// gendoc:generate(entity=network_bridge, group=common, key=security.acls)
		//
		// ---
		//  type: string
		//  condition: -
		//  default: -
		//  shortdesc: Comma-separated list of Network ACLs to apply to NICs connected to this network (see {ref}`network-acls-bridge-limitations`)
		"security.acls": validate.IsAny,
		// gendoc:generate(entity=network_bridge, group=common, key=security.acls.default.ingress.action)
		//
		// ---
		//  type: string
		//  condition: `security.acls`
		//  default: `reject`
		//  shortdesc: Action to use for ingress traffic that doesn't match any ACL rule
		"security.acls.default.ingress.action": validate.Optional(validate.IsOneOf(acl.ValidActions...)),

		// gendoc:generate(entity=network_bridge, group=common, key=security.acls.default.egress.action)
		//
		// ---
		//  type: string
		//  condition: `security.acls`
		//  default: `reject`
		//  shortdesc: Action to use for egress traffic that doesn't match any ACL rule
		"security.acls.default.egress.action": validate.Optional(validate.IsOneOf(acl.ValidActions...)),

		// gendoc:generate(entity=network_bridge, group=common, key=security.acls.default.ingress.logged)
		//
		// ---
		//  type: bool
		//  condition: `security.acls`
		//  default: `false`
		//  shortdesc: Whether to log ingress traffic that doesn't match any ACL rule
		"security.acls.default.ingress.logged": validate.Optional(validate.IsBool),

		// gendoc:generate(entity=network_bridge, group=common, key=security.acls.default.egress.logged)
		//
		// ---
		//  type: bool
		//  condition: `security.acls`
		//  default: `false`
		//  shortdesc: Whether to log egress traffic that doesn't match any ACL rule
		"security.acls.default.egress.logged": validate.Optional(validate.IsBool),
	}

	// Add dynamic validation rules.
	for k := range config {
		// Tunnel keys have the remote name in their name, extract the suffix.
		if strings.HasPrefix(k, "tunnel.") {
			// Validate remote name in key.
			fields := strings.Split(k, ".")
			if len(fields) != 3 {
				return fmt.Errorf("Invalid network configuration key: %s", k)
			}

			if len(n.name)+len(fields[1]) > 14 {
				return fmt.Errorf("Network name too long for tunnel interface: %s-%s", n.name, fields[1])
			}

			tunnelKey := fields[2]

			// Add the correct validation rule for the dynamic field based on last part of key.
			switch tunnelKey {
			case "protocol":
				// gendoc:generate(entity=network_bridge, group=common, key=tunnel.NAME.protocol)
				//
				// ---
				//  type: string
				//  condition: standard mode
				//  default: -
				//  shortdesc: Tunneling protocol: `vxlan` or `gre`
				rules[k] = validate.Optional(validate.IsOneOf("gre", "vxlan"))
			case "local":
				// gendoc:generate(entity=network_bridge, group=common, key=tunnel.NAME.local)
				//
				// ---
				//  type: string
				//  condition: `gre` or `vxlan`
				//  default: -
				//  shortdesc: Local address for the tunnel (not necessary for multicast `vxlan`)
				rules[k] = validate.Optional(validate.IsNetworkAddress)
			case "remote":
				// gendoc:generate(entity=network_bridge, group=common, key=tunnel.NAME.remote)
				//
				// ---
				//  type: string
				//  condition: `gre` or `vxlan`
				//  default: -
				//  shortdesc: Remote address for the tunnel (not necessary for multicast `vxlan`)
				rules[k] = validate.Optional(validate.IsNetworkAddress)
			case "port":
				// gendoc:generate(entity=network_bridge, group=common, key=tunnel.NAME.port)
				//
				// ---
				//  type: integer
				//  condition: `vxlan`
				//  default: `0`
				//  shortdesc: Specific port to use for the `vxlan` tunnel
				rules[k] = networkValidPort
			case "group":
				// gendoc:generate(entity=network_bridge, group=common, key=tunnel.NAME.group)
				//
				// ---
				//  type: string
				//  condition: `vxlan`
				//  default: `239.0.0.1`
				//  shortdesc: Multicast address for `vxlan` (used if local and remote aren't set)
				rules[k] = validate.Optional(validate.IsNetworkAddress)
			case "id":
				// gendoc:generate(entity=network_bridge, group=common, key=tunnel.NAME.id)
				//
				// ---
				//  type: integer
				//  condition: `vxlan`
				//  default: `0`
				//  shortdesc: Specific tunnel ID to use for the `vxlan` tunnel
				rules[k] = validate.Optional(validate.IsInt64)
			case "interface":
				// gendoc:generate(entity=network_bridge, group=common, key=tunnel.NAME.interface)
				//
				// ---
				//  type: string
				//  condition: `vxlan`
				//  default: -
				//  shortdesc: Specific host interface to use for the tunnel
				rules[k] = validate.IsInterfaceName
			case "ttl":
				// gendoc:generate(entity=network_bridge, group=common, key=tunnel.NAME.ttl)
				//
				// ---
				//  type: integer
				//  condition: `vxlan`
				//  default: `1`
				//  shortdesc: Specific TTL to use for multicast routing topologies
				rules[k] = validate.Optional(validate.IsUint8)
			}
		}
	}

	// gendoc:generate(entity=network_bridge, group=bgp, key=bgp.peers.NAME.address)
	//
	// ---
	// type: string
	// condition: BGP server
	// defaultdesc: -
	// shortdesc: Peer address (IPv4 or IPv6) for use by `ovn` downstream networks

	// gendoc:generate(entity=network_bridge, group=bgp, key=bgp.peers.NAME.asn)
	//
	// ---
	// type: integer
	// condition: BGP server
	// defaultdesc: -
	// shortdesc: Peer AS number for use by `ovn` downstream networks

	// gendoc:generate(entity=network_bridge, group=bgp, key=bgp.peers.NAME.password)
	//
	// ---
	// type: string
	// condition: BGP server
	// defaultdesc: - (no password)
	// shortdesc: Peer session password (optional) for use by `ovn` downstream networks

	// gendoc:generate(entity=network_bridge, group=bgp, key=bgp.peers.NAME.holdtime)
	//
	// ---
	// type: integer
	// condition: BGP server
	// defaultdesc: `180`
	// shortdesc: Peer session hold time (in seconds; optional)

	// Add the BGP validation rules.
	bgpRules, err := n.bgpValidationRules(config)
	if err != nil {
		return err
	}

	maps.Copy(rules, bgpRules)

	// gendoc:generate(entity=network_bridge, group=common, key=user.*)
	//
	// ---
	//  type: string
	//  condition: -
	//  default: -
	//  shortdesc: User-provided free-form key/value pairs

	// Validate the configuration.
	err = n.validate(config, rules)
	if err != nil {
		return err
	}

	// Perform composite key checks after per-key validation.

	// Validate DNS zone names.
	err = n.validateZoneNames(config)
	if err != nil {
		return err
	}

	for k, v := range config {
		key := k
		// MTU checks
		if key == "bridge.mtu" && v != "" {
			mtu, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				return fmt.Errorf("Invalid value for an integer: %s", v)
			}

			ipv6 := config["ipv6.address"]
			if ipv6 != "" && ipv6 != "none" && mtu < 1280 {
				return errors.New("The minimum MTU for an IPv6 network is 1280")
			}

			ipv4 := config["ipv4.address"]
			if ipv4 != "" && ipv4 != "none" && mtu < 68 {
				return errors.New("The minimum MTU for an IPv4 network is 68")
			}
		}
	}

	// Check using same MAC address on every cluster node is safe.
	if config["bridge.hwaddr"] != "" {
		err = n.checkClusterWideMACSafe(config)
		if err != nil {
			return err
		}
	}

	// Check IPv4 OVN ranges.
	if config["ipv4.ovn.ranges"] != "" && util.IsTrueOrEmpty(config["ipv4.dhcp"]) {
		dhcpSubnet := n.DHCPv4Subnet()
		allowedNets := []*net.IPNet{}

		if dhcpSubnet != nil {
			if config["ipv4.dhcp.ranges"] == "" {
				return errors.New(`"ipv4.ovn.ranges" must be used in conjunction with non-overlapping "ipv4.dhcp.ranges" when DHCPv4 is enabled`)
			}

			allowedNets = append(allowedNets, dhcpSubnet)
		}

		ovnRanges, err := parseIPRanges(config["ipv4.ovn.ranges"], allowedNets...)
		if err != nil {
			return fmt.Errorf("Failed parsing ipv4.ovn.ranges: %w", err)
		}

		dhcpRanges, err := parseIPRanges(config["ipv4.dhcp.ranges"], allowedNets...)
		if err != nil {
			return fmt.Errorf("Failed parsing ipv4.dhcp.ranges: %w", err)
		}

		for _, ovnRange := range ovnRanges {
			for _, dhcpRange := range dhcpRanges {
				if IPRangesOverlap(ovnRange, dhcpRange) {
					return fmt.Errorf(`The range specified in "ipv4.ovn.ranges" (%q) cannot overlap with "ipv4.dhcp.ranges"`, ovnRange)
				}
			}
		}
	}

	// Check IPv6 OVN ranges.
	if config["ipv6.ovn.ranges"] != "" && util.IsTrueOrEmpty(config["ipv6.dhcp"]) {
		dhcpSubnet := n.DHCPv6Subnet()
		allowedNets := []*net.IPNet{}

		if dhcpSubnet != nil {
			if config["ipv6.dhcp.ranges"] == "" && util.IsTrue(config["ipv6.dhcp.stateful"]) {
				return errors.New(`"ipv6.ovn.ranges" must be used in conjunction with non-overlapping "ipv6.dhcp.ranges" when stateful DHCPv6 is enabled`)
			}

			allowedNets = append(allowedNets, dhcpSubnet)
		}

		ovnRanges, err := parseIPRanges(config["ipv6.ovn.ranges"], allowedNets...)
		if err != nil {
			return fmt.Errorf("Failed parsing ipv6.ovn.ranges: %w", err)
		}

		// If stateful DHCPv6 is enabled, check OVN ranges don't overlap with DHCPv6 stateful ranges.
		// Otherwise SLAAC will be being used to generate client IPs and predefined ranges aren't used.
		if dhcpSubnet != nil && util.IsTrue(config["ipv6.dhcp.stateful"]) {
			dhcpRanges, err := parseIPRanges(config["ipv6.dhcp.ranges"], allowedNets...)
			if err != nil {
				return fmt.Errorf("Failed parsing ipv6.dhcp.ranges: %w", err)
			}

			for _, ovnRange := range ovnRanges {
				for _, dhcpRange := range dhcpRanges {
					if IPRangesOverlap(ovnRange, dhcpRange) {
						return fmt.Errorf(`The range specified in "ipv6.ovn.ranges" (%q) cannot overlap with "ipv6.dhcp.ranges"`, ovnRange)
					}
				}
			}
		}
	}

	// Check Security ACLs are supported and exist.
	if config["security.acls"] != "" {
		err = acl.Exists(n.state, n.Project(), util.SplitNTrimSpace(config["security.acls"], ",", -1, true)...)
		if err != nil {
			return err
		}
	}

	return nil
}

// Create checks whether the bridge interface name is used already.
func (n *bridge) Create(clientType request.ClientType) error {
	n.logger.Debug("Create", logger.Ctx{"clientType": clientType, "config": n.config})

	if InterfaceExists(n.name) {
		return fmt.Errorf("Network interface %q already exists", n.name)
	}

	return nil
}

// isRunning returns whether the network is up.
func (n *bridge) isRunning() bool {
	return InterfaceExists(n.name)
}

// Delete deletes a network.
func (n *bridge) Delete(clientType request.ClientType) error {
	n.logger.Debug("Delete", logger.Ctx{"clientType": clientType})

	if n.isRunning() {
		err := n.Stop()
		if err != nil {
			return err
		}
	}

	// Clean up extended external interfaces.
	if n.config["bridge.external_interfaces"] != "" {
		for _, entry := range strings.Split(n.config["bridge.external_interfaces"], ",") {
			entry = strings.TrimSpace(entry)
			entryParts := strings.Split(entry, "/")

			if len(entryParts) == 3 {
				ifName := strings.TrimSpace(entryParts[0])
				_, err := net.InterfaceByName(ifName)
				if err == nil {
					err = InterfaceRemove(ifName)
					if err != nil {
						return err
					}
				}
			}
		}
	}

	// Delete apparmor profiles.
	err := apparmor.NetworkDelete(n.state.OS, n)
	if err != nil {
		return err
	}

	return n.common.delete(clientType)
}

// Rename renames a network.
func (n *bridge) Rename(newName string) error {
	n.logger.Debug("Rename", logger.Ctx{"newName": newName})

	if InterfaceExists(newName) {
		return fmt.Errorf("Network interface %q already exists", newName)
	}

	// Bring the network down.
	if n.isRunning() {
		err := n.Stop()
		if err != nil {
			return err
		}
	}

	// Rename common steps.
	err := n.common.rename(newName)
	if err != nil {
		return err
	}

	// Bring the network up.
	err = n.Start()
	if err != nil {
		return err
	}

	return nil
}

// Start starts the network.
func (n *bridge) Start() error {
	n.logger.Debug("Start")

	reverter := revert.New()
	defer reverter.Fail()

	reverter.Add(func() { n.setUnavailable() })

	err := n.setup(nil)
	if err != nil {
		return err
	}

	reverter.Success()

	// Ensure network is marked as available now its started.
	n.setAvailable()

	return nil
}

// setup restarts the network.
func (n *bridge) setup(oldConfig map[string]string) error {
	// If we are in mock mode, just no-op.
	if n.state.OS.MockMode {
		return nil
	}

	n.logger.Debug("Setting up network")

	reverter := revert.New()
	defer reverter.Fail()

	// Create directory.
	if !util.PathExists(internalUtil.VarPath("networks", n.name)) {
		err := os.MkdirAll(internalUtil.VarPath("networks", n.name), 0o711)
		if err != nil {
			return err
		}
	}

	var err error

	// Build up the bridge interface's settings.
	bridge := ip.Bridge{
		Link: ip.Link{
			Name: n.name,
			MTU:  bridgeMTUDefault,
		},
	}

	// Get a list of tunnels.
	tunnels := n.getTunnels()

	// Decide the MTU for the bridge interface.
	if n.config["bridge.mtu"] != "" {
		mtuInt, err := strconv.ParseUint(n.config["bridge.mtu"], 10, 32)
		if err != nil {
			return fmt.Errorf("Invalid MTU %q: %w", n.config["bridge.mtu"], err)
		}

		bridge.MTU = uint32(mtuInt)
	} else if len(tunnels) > 0 {
		bridge.MTU = 1400
	}

	// Decide the MAC address of bridge interface.
	if n.config["bridge.hwaddr"] != "" {
		bridge.Address, err = net.ParseMAC(n.config["bridge.hwaddr"])
		if err != nil {
			return fmt.Errorf("Failed parsing MAC address %q: %w", n.config["bridge.hwaddr"], err)
		}
	} else {
		// If no cluster wide static MAC address set, then generate one.
		var seedNodeID int64

		if n.checkClusterWideMACSafe(n.config) != nil {
			// If not safe to use a cluster wide MAC, then use cluster node's ID to
			// generate a stable per-node & network derived random MAC.
			seedNodeID = n.state.DB.Cluster.GetNodeID()
		} else {
			// If safe to use a cluster wide MAC, then use a static cluster node of 0 to generate a
			// stable per-network derived random MAC.
			seedNodeID = 0
		}

		// Load server certificate. This is needs to be the same certificate for all nodes in a cluster.
		cert, err := internalUtil.LoadCert(n.state.OS.VarDir)
		if err != nil {
			return err
		}

		// Generate the random seed, this uses the server certificate fingerprint (to ensure that multiple
		// standalone nodes with the same network ID connected to the same external network don't generate
		// the same MAC for their networks). It relies on the certificate being the same for all nodes in a
		// cluster to allow the same MAC to be generated on each bridge interface in the network when
		// seedNodeID is 0 (when safe to do so).
		seed := fmt.Sprintf("%s.%d.%d", cert.Fingerprint(), seedNodeID, n.ID())
		r, err := localUtil.GetStableRandomGenerator(seed)
		if err != nil {
			return fmt.Errorf("Failed generating stable random bridge MAC: %w", err)
		}

		randomHwaddr := randomHwaddr(r)
		bridge.Address, err = net.ParseMAC(randomHwaddr)
		if err != nil {
			return fmt.Errorf("Failed parsing MAC address %q: %w", randomHwaddr, err)
		}

		n.logger.Debug("Stable MAC generated", logger.Ctx{"seed": seed, "hwAddr": bridge.Address.String()})
	}

	// Create the bridge interface if doesn't exist.
	if !n.isRunning() {
		if n.config["bridge.driver"] == "openvswitch" {
			vswitch, err := n.state.OVS()
			if err != nil {
				return fmt.Errorf("Couldn't connect to OpenVSwitch: %v", err)
			}

			// Add and configure the interface in one operation to reduce the number of executions and
			// to avoid systemd-udevd from applying the default MACAddressPolicy=persistent policy.
			err = vswitch.CreateBridge(context.TODO(), n.name, false, bridge.Address, bridge.MTU)
			if err != nil {
				return err
			}

			reverter.Add(func() { _ = vswitch.DeleteBridge(context.Background(), n.name) })
		} else {
			// Add and configure the interface in one operation to reduce the number of executions and
			// to avoid systemd-udevd from applying the default MACAddressPolicy=persistent policy.
			err := bridge.Add()
			if err != nil {
				return err
			}

			reverter.Add(func() { _ = bridge.Delete() })
		}
	} else {
		// If bridge already exists then re-apply settings. If we just created a bridge then we don't
		// need to do this as the settings will have been applied as part of the add operation.

		// Set the MTU on the bridge interface.
		err := bridge.SetMTU(bridge.MTU)
		if err != nil {
			return err
		}

		// Set the MAC address on the bridge interface if specified.
		if bridge.Address != nil {
			err = bridge.SetAddress(bridge.Address)
			if err != nil {
				return err
			}
		}
	}

	// IPv6 bridge configuration.
	if !util.IsNoneOrEmpty(n.config["ipv6.address"]) {
		if !util.PathExists("/proc/sys/net/ipv6") {
			return errors.New("Network has ipv6.address but kernel IPv6 support is missing")
		}

		err := localUtil.SysctlSet(fmt.Sprintf("net/ipv6/conf/%s/disable_ipv6", n.name), "0")
		if err != nil {
			return err
		}

		err = localUtil.SysctlSet(fmt.Sprintf("net/ipv6/conf/%s/autoconf", n.name), "0")
		if err != nil {
			return err
		}

		err = localUtil.SysctlSet(fmt.Sprintf("net/ipv6/conf/%s/accept_dad", n.name), "0")
		if err != nil {
			return err
		}
	} else {
		// Disable IPv6 if no address is specified. This prevents the
		// host being reachable over a guessable link-local address as well as it
		// auto-configuring an address should an instance operate an IPv6 router.
		if util.PathExists("/proc/sys/net/ipv6") {
			err := localUtil.SysctlSet(fmt.Sprintf("net/ipv6/conf/%s/disable_ipv6", n.name), "1")
			if err != nil {
				return err
			}
		}
	}

	err = n.deleteChildren()
	if err != nil {
		return fmt.Errorf("Failed to delete bridge children interfaces: %w", err)
	}

	// Attempt to add a dummy device to the bridge to force the MTU.
	if bridge.MTU != bridgeMTUDefault && n.config["bridge.driver"] != "openvswitch" {
		dummy := &ip.Dummy{
			Link: ip.Link{
				Name: fmt.Sprintf("%s-mtu", n.name),
				MTU:  bridge.MTU,
			},
		}

		err = dummy.Add()
		if err == nil {
			reverter.Add(func() { _ = dummy.Delete() })
			err = dummy.SetUp()
			if err == nil {
				_ = AttachInterface(n.state, n.name, fmt.Sprintf("%s-mtu", n.name))
			}
		}
	}

	// Enable VLAN filtering for Linux bridges.
	if n.config["bridge.driver"] != "openvswitch" {
		// Enable filtering.
		err = BridgeVLANFilterSetStatus(n.name, "1")
		if err != nil {
			n.logger.Warn(fmt.Sprintf("Failed enabling VLAN filtering: %v", err))
		}
	}

	// Bring it up.
	err = bridge.SetUp()
	if err != nil {
		return err
	}

	// Add any listed existing external interface.
	if n.config["bridge.external_interfaces"] != "" {
		for _, entry := range strings.Split(n.config["bridge.external_interfaces"], ",") {
			entry = strings.TrimSpace(entry)

			// Test for extended configuration of external interface.
			entryParts := strings.Split(entry, "/")
			ifParent := ""
			vlanID := 0

			if len(entryParts) == 3 {
				vlanID, err = strconv.Atoi(entryParts[2])
				if err != nil || vlanID < 1 || vlanID > 4094 {
					vlanID = 0
					n.logger.Warn("Ignoring invalid VLAN ID", logger.Ctx{"interface": entry, "vlanID": entryParts[2]})
				} else {
					entry = strings.TrimSpace(entryParts[0])
					ifParent = strings.TrimSpace(entryParts[1])
				}
			}

			iface, err := net.InterfaceByName(entry)
			if err != nil {
				if vlanID == 0 {
					n.logger.Warn("Skipping attaching missing external interface", logger.Ctx{"interface": entry})
					continue
				}

				// If the interface doesn't exist and VLAN ID was provided, create the missing interface.
				ok, err := VLANInterfaceCreate(ifParent, entry, strconv.Itoa(vlanID), false)
				if ok {
					iface, err = net.InterfaceByName(entry)
				}

				if !ok || err != nil {
					return fmt.Errorf("Failed to create external interface %q", entry)
				}
			} else if vlanID > 0 {
				// If the interface exists and VLAN ID was provided, ensure it has the same parent and VLAN ID and is not attached to a different network.
				linkInfo, err := ip.LinkByName(entry)
				if err != nil {
					return fmt.Errorf("Failed to get link info for external interface %q", entry)
				}

				if linkInfo.Kind != "vlan" || linkInfo.Parent != ifParent || linkInfo.VlanID != vlanID || (linkInfo.Master != "" && linkInfo.Master != n.name) {
					return fmt.Errorf("External interface %q already in use", entry)
				}
			}

			unused := true
			addrs, err := iface.Addrs()
			if err == nil {
				for _, addr := range addrs {
					ip, _, err := net.ParseCIDR(addr.String())
					if ip != nil && err == nil && ip.IsGlobalUnicast() {
						unused = false
						break
					}
				}
			}

			if !unused {
				return errors.New("Only unconfigured network interfaces can be bridged")
			}

			err = AttachInterface(n.state, n.name, entry)
			if err != nil {
				return err
			}

			// Make sure the port is up.
			link := &ip.Link{Name: entry}
			err = link.SetUp()
			if err != nil {
				return fmt.Errorf("Failed to bring up the host interface %s: %w", entry, err)
			}
		}
	}

	// Remove any existing firewall rules.
	fwClearIPVersions := []uint{}

	if usesIPv4Firewall(n.config) || usesIPv4Firewall(oldConfig) {
		fwClearIPVersions = append(fwClearIPVersions, 4)
	}

	if usesIPv6Firewall(n.config) || usesIPv6Firewall(oldConfig) {
		fwClearIPVersions = append(fwClearIPVersions, 6)
	}

	if len(fwClearIPVersions) > 0 {
		n.logger.Debug("Clearing firewall")
		err = n.state.Firewall.NetworkClear(n.name, false, fwClearIPVersions)
		if err != nil {
			return fmt.Errorf("Failed clearing firewall: %w", err)
		}
	}

	// Initialize a new firewall option set.
	fwOpts := firewallDrivers.Opts{}

	if n.hasIPv4Firewall() {
		fwOpts.FeaturesV4 = &firewallDrivers.FeatureOpts{}
	}

	if n.hasIPv6Firewall() {
		fwOpts.FeaturesV6 = &firewallDrivers.FeatureOpts{}
	}

	if n.config["security.acls"] != "" {
		fwOpts.ACL = true
	}

	// Snapshot container specific IPv4 routes (added with boot proto) before removing IPv4 addresses.
	// This is because the kernel removes any static routes on an interface when all addresses removed.
	ctRoutes, err := n.bootRoutesV4()
	if err != nil {
		return err
	}

	// Flush all IPv4 addresses and routes.
	addr := &ip.Addr{
		DevName: n.name,
		Scope:   "global",
		Family:  ip.FamilyV4,
	}

	err = addr.Flush()
	if err != nil {
		return err
	}

	r := &ip.Route{
		DevName: n.name,
		Proto:   "static",
		Family:  ip.FamilyV4,
	}

	err = r.Flush()
	if err != nil {
		return err
	}

	// Configure IPv4 firewall.
	if !util.IsNoneOrEmpty(n.config["ipv4.address"]) {
		if n.hasDHCPv4() && n.hasIPv4Firewall() {
			fwOpts.FeaturesV4.ICMPDHCPDNSAccess = true
		}

		// Allow forwarding.
		if util.IsTrueOrEmpty(n.config["ipv4.routing"]) {
			err = localUtil.SysctlSet("net/ipv4/ip_forward", "1")
			if err != nil {
				return err
			}

			if n.hasIPv4Firewall() {
				fwOpts.FeaturesV4.ForwardingAllow = true
			}
		}
	}

	// Start building process using subprocess package.
	command := "dnsmasq"
	dnsmasqCmd := []string{
		"--keep-in-foreground", "--strict-order", "--bind-interfaces",
		"--except-interface=lo",
		"--pid-file=", // Disable attempt at writing a PID file.
		"--no-ping",   // --no-ping is very important to prevent delays to lease file updates.
		fmt.Sprintf("--interface=%s", n.name),
	}

	dnsmasqVersion, err := dnsmasq.GetVersion()
	if err != nil {
		return err
	}

	// --dhcp-rapid-commit option is only supported on >2.79.
	minVer, _ := version.NewDottedVersion("2.79")
	if dnsmasqVersion.Compare(minVer) > 0 {
		dnsmasqCmd = append(dnsmasqCmd, "--dhcp-rapid-commit")
	}

	// --no-negcache option is only supported on >2.47.
	minVer, _ = version.NewDottedVersion("2.47")
	if dnsmasqVersion.Compare(minVer) > 0 {
		dnsmasqCmd = append(dnsmasqCmd, "--no-negcache")
	}

	if !daemon.Debug {
		// --quiet options are only supported on >2.67.
		minVer, _ := version.NewDottedVersion("2.67")

		if dnsmasqVersion.Compare(minVer) > 0 {
			dnsmasqCmd = append(dnsmasqCmd, []string{"--quiet-dhcp", "--quiet-dhcp6", "--quiet-ra"}...)
		}
	}

	var dnsIPv4 []string
	var dnsIPv6 []string
	for _, s := range util.SplitNTrimSpace(n.config["dns.nameservers"], ",", -1, false) {
		if net.ParseIP(s).To4() != nil {
			dnsIPv4 = append(dnsIPv4, s)
		} else {
			dnsIPv6 = append(dnsIPv6, s)
		}
	}

	// Configure IPv4.
	if !util.IsNoneOrEmpty(n.config["ipv4.address"]) {
		// Parse the subnet.
		ipAddress, subnet, err := net.ParseCIDR(n.config["ipv4.address"])
		if err != nil {
			return fmt.Errorf("Failed parsing ipv4.address: %w", err)
		}

		// Update the dnsmasq config.
		dnsmasqCmd = append(dnsmasqCmd, fmt.Sprintf("--listen-address=%s", ipAddress.String()))
		if n.DHCPv4Subnet() != nil {
			if !slices.Contains(dnsmasqCmd, "--dhcp-no-override") {
				dnsmasqCmd = append(dnsmasqCmd, []string{"--dhcp-no-override", "--dhcp-authoritative", fmt.Sprintf("--dhcp-leasefile=%s", internalUtil.VarPath("networks", n.name, "dnsmasq.leases")), fmt.Sprintf("--dhcp-hostsfile=%s", internalUtil.VarPath("networks", n.name, "dnsmasq.hosts"))}...)
			}

			if n.config["ipv4.dhcp.gateway"] != "" {
				dnsmasqCmd = append(dnsmasqCmd, fmt.Sprintf("--dhcp-option-force=3,%s", n.config["ipv4.dhcp.gateway"]))
			}

			if n.config["dns.nameservers"] != "" {
				if len(dnsIPv4) == 0 {
					dnsmasqCmd = append(dnsmasqCmd, "--dhcp-option-force=6")
				} else {
					dnsmasqCmd = append(dnsmasqCmd, fmt.Sprintf("--dhcp-option-force=6,%s", strings.Join(dnsIPv4, ",")))
				}
			}

			if bridge.MTU != bridgeMTUDefault {
				dnsmasqCmd = append(dnsmasqCmd, fmt.Sprintf("--dhcp-option-force=26,%d", bridge.MTU))
			}

			dnsSearch := n.config["dns.search"]
			if dnsSearch != "" {
				dnsmasqCmd = append(dnsmasqCmd, fmt.Sprintf("--dhcp-option-force=119,%s", strings.Trim(dnsSearch, " ")))
			}

			if n.config["ipv4.dhcp.routes"] != "" {
				dnsmasqCmd = append(dnsmasqCmd, fmt.Sprintf("--dhcp-option-force=121,%s", strings.ReplaceAll(n.config["ipv4.dhcp.routes"], " ", "")))
			}

			expiry := "1h"
			if n.config["ipv4.dhcp.expiry"] != "" {
				expiry = n.config["ipv4.dhcp.expiry"]
			}

			if n.config["ipv4.dhcp.ranges"] != "" {
				for _, dhcpRange := range strings.Split(n.config["ipv4.dhcp.ranges"], ",") {
					dhcpRange = strings.TrimSpace(dhcpRange)
					dnsmasqCmd = append(dnsmasqCmd, []string{"--dhcp-range", fmt.Sprintf("%s,%s", strings.ReplaceAll(dhcpRange, "-", ","), expiry)}...)
				}
			} else {
				dnsmasqCmd = append(dnsmasqCmd, []string{"--dhcp-range", fmt.Sprintf("%s,%s,%s", dhcpalloc.GetIP(subnet, 2).String(), dhcpalloc.GetIP(subnet, -2).String(), expiry)}...)
			}
		}

		// Add the address.
		addr := &ip.Addr{
			DevName: n.name,
			Address: &net.IPNet{
				IP:   ipAddress,
				Mask: subnet.Mask,
			},
			Family: ip.FamilyV4,
		}

		err = addr.Add()
		if err != nil {
			return err
		}

		// Configure NAT.
		if util.IsTrue(n.config["ipv4.nat"]) {
			// If a SNAT source address is specified, use that, otherwise default to MASQUERADE mode.
			var srcIP net.IP
			if n.config["ipv4.nat.address"] != "" {
				srcIP = net.ParseIP(n.config["ipv4.nat.address"])
			}

			fwOpts.SNATV4 = &firewallDrivers.SNATOpts{
				SNATAddress: srcIP,
				Subnet:      subnet,
			}

			if n.config["ipv4.nat.order"] == "after" {
				fwOpts.SNATV4.Append = true
			}
		}

		// Add additional routes.
		if n.config["ipv4.routes"] != "" {
			for _, route := range strings.Split(n.config["ipv4.routes"], ",") {
				route, err := ip.ParseIPNet(strings.TrimSpace(route))
				if err != nil {
					return err
				}

				r := &ip.Route{
					DevName: n.name,
					Route:   route,
					Proto:   "static",
					Family:  ip.FamilyV4,
				}

				err = r.Add()
				if err != nil {
					return err
				}
			}
		}

		// Restore container specific IPv4 routes to interface.
		n.applyBootRoutesV4(ctRoutes)
	}

	// Snapshot container specific IPv6 routes (added with boot proto) before removing IPv6 addresses.
	// This is because the kernel removes any static routes on an interface when all addresses removed.
	ctRoutes, err = n.bootRoutesV6()
	if err != nil {
		return err
	}

	// Flush all IPv6 addresses and routes.
	addr = &ip.Addr{
		DevName: n.name,
		Scope:   "global",
		Family:  ip.FamilyV6,
	}

	err = addr.Flush()
	if err != nil {
		return err
	}

	r = &ip.Route{
		DevName: n.name,
		Proto:   "static",
		Family:  ip.FamilyV6,
	}

	err = r.Flush()
	if err != nil {
		return err
	}

	// Configure IPv6.
	if !util.IsNoneOrEmpty(n.config["ipv6.address"]) {
		// Enable IPv6 for the subnet.
		err := localUtil.SysctlSet(fmt.Sprintf("net/ipv6/conf/%s/disable_ipv6", n.name), "0")
		if err != nil {
			return err
		}

		// Parse the subnet.
		ipAddress, subnet, err := net.ParseCIDR(n.config["ipv6.address"])
		if err != nil {
			return fmt.Errorf("Failed parsing ipv6.address: %w", err)
		}

		subnetSize, _ := subnet.Mask.Size()

		// Check if we need to generate a host-specific address.
		if ipAddress.String() == subnet.IP.String() {
			if subnetSize != 64 {
				return errors.New("Can't generate an EUI64 derived IPv6 address with a mask other than /64")
			}

			ipAddress, err = eui64.ParseMAC(subnet.IP, bridge.Address)
			if err != nil {
				return fmt.Errorf("Failed generating EUI64 value for ipv6.address: %w", err)
			}
		}

		if subnetSize > 64 {
			n.logger.Warn("IPv6 networks with a prefix larger than 64 aren't properly supported by dnsmasq")

			err = n.state.DB.Cluster.Transaction(context.TODO(), func(ctx context.Context, tx *db.ClusterTx) error {
				return tx.UpsertWarningLocalNode(ctx, n.project, dbCluster.TypeNetwork, int(n.id), warningtype.LargerIPv6PrefixThanSupported, "")
			})
			if err != nil {
				n.logger.Warn("Failed to create warning", logger.Ctx{"err": err})
			}
		} else {
			err = warnings.ResolveWarningsByLocalNodeAndProjectAndTypeAndEntity(n.state.DB.Cluster, n.project, warningtype.LargerIPv6PrefixThanSupported, dbCluster.TypeNetwork, int(n.id))
			if err != nil {
				n.logger.Warn("Failed to resolve warning", logger.Ctx{"err": err})
			}
		}

		// Update the dnsmasq config.
		dnsmasqCmd = append(dnsmasqCmd, []string{fmt.Sprintf("--listen-address=%s", ipAddress.String()), "--enable-ra"}...)
		if n.DHCPv6Subnet() != nil {
			if n.hasIPv6Firewall() {
				fwOpts.FeaturesV6.ICMPDHCPDNSAccess = true
			}

			// Build DHCP configuration.
			if !slices.Contains(dnsmasqCmd, "--dhcp-no-override") {
				dnsmasqCmd = append(dnsmasqCmd, []string{"--dhcp-no-override", "--dhcp-authoritative", fmt.Sprintf("--dhcp-leasefile=%s", internalUtil.VarPath("networks", n.name, "dnsmasq.leases")), fmt.Sprintf("--dhcp-hostsfile=%s", internalUtil.VarPath("networks", n.name, "dnsmasq.hosts"))}...)
			}

			expiry := "1h"
			if n.config["ipv6.dhcp.expiry"] != "" {
				expiry = n.config["ipv6.dhcp.expiry"]
			}

			if util.IsTrue(n.config["ipv6.dhcp.stateful"]) {
				if n.config["ipv6.dhcp.ranges"] != "" {
					for _, dhcpRange := range strings.Split(n.config["ipv6.dhcp.ranges"], ",") {
						dhcpRange = strings.TrimSpace(dhcpRange)
						dnsmasqCmd = append(dnsmasqCmd, []string{"--dhcp-range", fmt.Sprintf("%s,%d,%s", strings.ReplaceAll(dhcpRange, "-", ","), subnetSize, expiry)}...)
					}
				} else {
					dnsmasqCmd = append(dnsmasqCmd, []string{"--dhcp-range", fmt.Sprintf("%s,%s,%d,%s", dhcpalloc.GetIP(subnet, 2), dhcpalloc.GetIP(subnet, -1), subnetSize, expiry)}...)
				}
			} else {
				dnsmasqCmd = append(dnsmasqCmd, []string{"--dhcp-range", fmt.Sprintf("::,constructor:%s,ra-stateless,ra-names", n.name)}...)
			}
		} else {
			dnsmasqCmd = append(dnsmasqCmd, []string{"--dhcp-range", fmt.Sprintf("::,constructor:%s,ra-only", n.name)}...)
		}

		if n.config["dns.nameservers"] != "" {
			if len(dnsIPv6) == 0 {
				dnsmasqCmd = append(dnsmasqCmd, "--dhcp-option-force=option6:dns-server")
			} else {
				dnsmasqCmd = append(dnsmasqCmd, fmt.Sprintf("--dhcp-option-force=option6:dns-server,[%s]", strings.Join(dnsIPv6, ",")))
			}
		}

		// Allow forwarding.
		if util.IsTrueOrEmpty(n.config["ipv6.routing"]) {
			// Get a list of proc entries.
			entries, err := os.ReadDir("/proc/sys/net/ipv6/conf/")
			if err != nil {
				return err
			}

			// First set accept_ra to 2 for all interfaces (if not disabled).
			// This ensures that the host can still receive IPv6 router advertisements even with
			// forwarding enabled (which enable below), as the default is to ignore router adverts
			// when forward is enabled, and this could render the host unreachable if it uses
			// SLAAC generated IPs.
			for _, entry := range entries {
				// Check that IPv6 router advertisement acceptance is enabled currently.
				// If its set to 0 then we don't want to enable, and if its already set to 2 then
				// we don't need to do anything.
				content, err := os.ReadFile(fmt.Sprintf("/proc/sys/net/ipv6/conf/%s/accept_ra", entry.Name()))
				if err == nil && string(content) != "1\n" {
					continue
				}

				// If IPv6 router acceptance is enabled (set to 1) then we now set it to 2.
				err = localUtil.SysctlSet(fmt.Sprintf("net/ipv6/conf/%s/accept_ra", entry.Name()), "2")
				if err != nil && !errors.Is(err, fs.ErrNotExist) {
					return err
				}
			}

			// Then set forwarding for all of them.
			for _, entry := range entries {
				err = localUtil.SysctlSet(fmt.Sprintf("net/ipv6/conf/%s/forwarding", entry.Name()), "1")
				if err != nil && !errors.Is(err, fs.ErrNotExist) {
					return err
				}
			}

			if n.hasIPv6Firewall() {
				fwOpts.FeaturesV6.ForwardingAllow = true
			}
		}

		// Add the address.
		addr := &ip.Addr{
			DevName: n.name,
			Address: &net.IPNet{
				IP:   ipAddress,
				Mask: subnet.Mask,
			},
			Family: ip.FamilyV6,
		}

		err = addr.Add()
		if err != nil {
			return err
		}

		// Configure NAT.
		if util.IsTrue(n.config["ipv6.nat"]) {
			// If a SNAT source address is specified, use that, otherwise default to MASQUERADE mode.
			var srcIP net.IP
			if n.config["ipv6.nat.address"] != "" {
				srcIP = net.ParseIP(n.config["ipv6.nat.address"])
			}

			fwOpts.SNATV6 = &firewallDrivers.SNATOpts{
				SNATAddress: srcIP,
				Subnet:      subnet,
			}

			if n.config["ipv6.nat.order"] == "after" {
				fwOpts.SNATV6.Append = true
			}
		}

		// Add additional routes.
		if n.config["ipv6.routes"] != "" {
			for _, route := range strings.Split(n.config["ipv6.routes"], ",") {
				route, err := ip.ParseIPNet(route)
				if err != nil {
					return err
				}

				r := &ip.Route{
					DevName: n.name,
					Route:   route,
					Proto:   "static",
					Family:  ip.FamilyV6,
				}

				err = r.Add()
				if err != nil {
					return err
				}
			}
		}

		// Restore container specific IPv6 routes to interface.
		n.applyBootRoutesV6(ctRoutes)
	}

	// Configure tunnels.
	for _, tunnel := range tunnels {

		getConfig := func(key string) string {
			return n.config[fmt.Sprintf("tunnel.%s.%s", tunnel, key)]
		}

		tunProtocol := getConfig("protocol")
		tunLocal := net.ParseIP(getConfig("local"))
		tunRemote := net.ParseIP(getConfig("remote"))
		tunName := fmt.Sprintf("%s-%s", n.name, tunnel)

		// Configure the tunnel.
		if tunProtocol == "gre" {
			// Skip partial configs.
			if tunLocal == nil || tunRemote == nil {
				continue
			}

			gretap := &ip.Gretap{
				Link:   ip.Link{Name: tunName},
				Local:  tunLocal,
				Remote: tunRemote,
			}

			err := gretap.Add()
			if err != nil {
				return err
			}
		} else if tunProtocol == "vxlan" {
			tunGroup := net.ParseIP(getConfig("group"))
			tunInterface := getConfig("interface")

			vxlan := &ip.Vxlan{
				Link:  ip.Link{Name: tunName},
				Local: tunLocal,
			}

			if tunRemote != nil {
				// Skip partial configs.
				if tunLocal == nil {
					continue
				}

				vxlan.Remote = tunRemote
			} else {
				if tunGroup == nil {
					tunGroup = net.IPv4(239, 0, 0, 1) // 239.0.0.1
				}

				devName := tunInterface
				if devName == "" {
					_, devName, err = DefaultGatewaySubnetV4()
					if err != nil {
						return err
					}
				}

				vxlan.Group = tunGroup
				vxlan.DevName = devName
			}

			tunPort := getConfig("port")
			if tunPort != "" {
				vxlan.DstPort, err = strconv.Atoi(tunPort)
				if err != nil {
					return err
				}
			}

			tunID := getConfig("id")
			if tunID == "" {
				vxlan.VxlanID = 1
			} else {
				vxlan.VxlanID, err = strconv.Atoi(tunID)
				if err != nil {
					return err
				}
			}

			tunTTL := getConfig("ttl")
			if tunTTL == "" {
				vxlan.TTL = 1
			} else {
				vxlan.TTL, err = strconv.Atoi(tunTTL)
				if err != nil {
					return err
				}
			}

			err := vxlan.Add()
			if err != nil {
				return err
			}
		}

		// Bridge it and bring up.
		err = AttachInterface(n.state, n.name, tunName)
		if err != nil {
			return err
		}

		tunLink := &ip.Link{Name: tunName}
		err = tunLink.SetMTU(bridge.MTU)
		if err != nil {
			return err
		}

		// Bring up tunnel interface.
		err = tunLink.SetUp()
		if err != nil {
			return err
		}

		// Bring up network interface.
		err = bridge.SetUp()
		if err != nil {
			return err
		}
	}

	// Generate and load apparmor profiles.
	err = apparmor.NetworkLoad(n.state.OS, n)
	if err != nil {
		return err
	}

	// Kill any existing dnsmasq daemon for this network.
	err = dnsmasq.Kill(n.name, false)
	if err != nil {
		return err
	}

	// Configure dnsmasq.
	if n.UsesDNSMasq() {
		// Setup the dnsmasq domain.
		dnsDomain := n.config["dns.domain"]
		if dnsDomain == "" {
			dnsDomain = "incus"
		}

		if n.config["dns.mode"] != "none" {
			dnsmasqCmd = append(dnsmasqCmd, "-s", dnsDomain)
			dnsmasqCmd = append(dnsmasqCmd, "--interface-name", fmt.Sprintf("_gateway.%s,%s", dnsDomain, n.name))
			dnsmasqCmd = append(dnsmasqCmd, "-S", fmt.Sprintf("/%s/", dnsDomain))
		}

		// Create a config file to contain additional config (and to prevent dnsmasq from reading /etc/dnsmasq.conf)
		err = os.WriteFile(internalUtil.VarPath("networks", n.name, "dnsmasq.raw"), fmt.Appendf(nil, "%s\n", n.config["raw.dnsmasq"]), 0o644)
		if err != nil {
			return err
		}

		dnsmasqCmd = append(dnsmasqCmd, fmt.Sprintf("--conf-file=%s", internalUtil.VarPath("networks", n.name, "dnsmasq.raw")))

		// Attempt to drop privileges.
		if n.state.OS.UnprivUser != "" {
			dnsmasqCmd = append(dnsmasqCmd, []string{"-u", n.state.OS.UnprivUser}...)
		}

		if n.state.OS.UnprivGroup != "" {
			dnsmasqCmd = append(dnsmasqCmd, []string{"-g", n.state.OS.UnprivGroup}...)
		}

		// Create DHCP hosts directory.
		if !util.PathExists(internalUtil.VarPath("networks", n.name, "dnsmasq.hosts")) {
			err = os.MkdirAll(internalUtil.VarPath("networks", n.name, "dnsmasq.hosts"), 0o755)
			if err != nil {
				return err
			}
		}

		// Check for dnsmasq.
		_, err := exec.LookPath("dnsmasq")
		if err != nil {
			return errors.New("dnsmasq is required for managed bridges")
		}

		// Update the static leases.
		err = UpdateDNSMasqStatic(n.state, n.name)
		if err != nil {
			return err
		}

		// Create subprocess object dnsmasq.
		dnsmasqLogPath := internalUtil.LogPath(fmt.Sprintf("dnsmasq.%s.log", n.name))
		p, err := subprocess.NewProcess(command, dnsmasqCmd, "", dnsmasqLogPath)
		if err != nil {
			return fmt.Errorf("Failed to create subprocess: %s", err)
		}

		// Apply AppArmor confinement.
		if n.config["raw.dnsmasq"] == "" {
			p.SetApparmor(apparmor.DnsmasqProfileName(n))

			err = warnings.ResolveWarningsByLocalNodeAndProjectAndTypeAndEntity(n.state.DB.Cluster, n.project, warningtype.AppArmorDisabledDueToRawDnsmasq, dbCluster.TypeNetwork, int(n.id))
			if err != nil {
				n.logger.Warn("Failed to resolve warning", logger.Ctx{"err": err})
			}
		} else {
			n.logger.Warn("Skipping AppArmor for dnsmasq due to raw.dnsmasq being set", logger.Ctx{"name": n.name})

			err = n.state.DB.Cluster.Transaction(context.TODO(), func(ctx context.Context, tx *db.ClusterTx) error {
				return tx.UpsertWarningLocalNode(ctx, n.project, dbCluster.TypeNetwork, int(n.id), warningtype.AppArmorDisabledDueToRawDnsmasq, "")
			})
			if err != nil {
				n.logger.Warn("Failed to create warning", logger.Ctx{"err": err})
			}
		}

		// Start dnsmasq.
		err = p.Start(context.Background())
		if err != nil {
			return fmt.Errorf("Failed to run: %s %s: %w", command, strings.Join(dnsmasqCmd, " "), err)
		}

		// Check dnsmasq started OK.
		ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(time.Millisecond*time.Duration(500)))
		_, err = p.Wait(ctx)
		if !errors.Is(err, context.DeadlineExceeded) {
			stderr, _ := os.ReadFile(dnsmasqLogPath)
			cancel()

			return fmt.Errorf("The DNS and DHCP service exited prematurely: %w (%q)", err, strings.TrimSpace(string(stderr)))
		}

		cancel()

		err = p.Save(internalUtil.VarPath("networks", n.name, "dnsmasq.pid"))
		if err != nil {
			// Kill Process if started, but could not save the file.
			err2 := p.Stop()
			if err2 != nil {
				return fmt.Errorf("Could not kill subprocess while handling saving error: %s: %s", err, err2)
			}

			return fmt.Errorf("Failed to save subprocess details: %s", err)
		}
	} else {
		// Clean up old dnsmasq config if exists and we are not starting dnsmasq.
		leasesPath := internalUtil.VarPath("networks", n.name, "dnsmasq.leases")
		if util.PathExists(leasesPath) {
			err := os.Remove(leasesPath)
			if err != nil {
				return fmt.Errorf("Failed to remove old dnsmasq leases file %q: %w", leasesPath, err)
			}
		}

		// Clean up old dnsmasq PID file.
		pidPath := internalUtil.VarPath("networks", n.name, "dnsmasq.pid")
		if util.PathExists(pidPath) {
			err := os.Remove(pidPath)
			if err != nil {
				return fmt.Errorf("Failed to remove old dnsmasq pid file %q: %w", pidPath, err)
			}
		}
	}

	// Setup firewall.
	n.logger.Debug("Setting up firewall")

	if n.state.Firewall.String() == "nftables" {
		n.logger.Debug("Address set feature enabled for nftables backend")
		fwOpts.AddressSet = true
	}

	err = n.state.Firewall.NetworkSetup(n.name, fwOpts)
	if err != nil {
		return fmt.Errorf("Failed to setup firewall: %w", err)
	}

	// Setup named sets for nft firewall.
	// We apply all available address sets to avoid missing some.
	if fwOpts.AddressSet {
		n.logger.Debug("Applying up firewall address sets")
		aclNames := util.SplitNTrimSpace(n.config["security.acls"], ",", -1, false)
		err = addressset.FirewallApplyAddressSetsForACLRules(n.state, "inet", n.Project(), aclNames)
		if err != nil {
			return err
		}
	}

	if fwOpts.ACL {
		aclNet := acl.NetworkACLUsage{
			Name:   n.Name(),
			Type:   n.Type(),
			ID:     n.ID(),
			Config: n.Config(),
		}

		n.logger.Debug("Applying up firewall ACLs")
		err = acl.FirewallApplyACLRules(n.state, n.logger, n.Project(), aclNet)
		if err != nil {
			return err
		}
	}

	// Setup network address forwards.
	err = n.forwardSetupFirewall()
	if err != nil {
		return err
	}

	// Setup BGP.
	err = n.bgpSetup(oldConfig)
	if err != nil {
		return err
	}

	reverter.Success()

	return nil
}

// Stop stops the network.
func (n *bridge) Stop() error {
	n.logger.Debug("Stop")

	if !n.isRunning() {
		return nil
	}

	// Clear BGP.
	err := n.bgpClear(n.config)
	if err != nil {
		return err
	}

	err = n.deleteChildren()
	if err != nil {
		return fmt.Errorf("Failed to delete bridge children interfaces: %w", err)
	}

	// Destroy the bridge interface
	if n.config["bridge.driver"] == "openvswitch" {
		vswitch, err := n.state.OVS()
		if err != nil {
			return err
		}

		err = vswitch.DeleteBridge(context.TODO(), n.name)
		if err != nil {
			return err
		}
	} else {
		bridgeLink := &ip.Link{Name: n.name}
		err := bridgeLink.Delete()
		if err != nil {
			return err
		}
	}

	// Fully clear firewall setup.
	fwClearIPVersions := []uint{}

	if usesIPv4Firewall(n.config) {
		fwClearIPVersions = append(fwClearIPVersions, 4)
	}

	if usesIPv6Firewall(n.config) {
		fwClearIPVersions = append(fwClearIPVersions, 6)
	}

	if len(fwClearIPVersions) > 0 {
		n.logger.Debug("Deleting firewall")
		err := n.state.Firewall.NetworkClear(n.name, true, fwClearIPVersions)
		if err != nil {
			return fmt.Errorf("Failed deleting firewall: %w", err)
		}
	}

	// Kill any existing dnsmasq daemon for this network
	err = dnsmasq.Kill(n.name, false)
	if err != nil {
		return err
	}

	// Unload apparmor profiles.
	err = apparmor.NetworkUnload(n.state.OS, n)
	if err != nil {
		return err
	}

	return nil
}

// Update updates the network. Accepts notification boolean indicating if this update request is coming from a
// cluster notification, in which case do not update the database, just apply local changes needed.
func (n *bridge) Update(newNetwork api.NetworkPut, targetNode string, clientType request.ClientType) error {
	n.logger.Debug("Update", logger.Ctx{"clientType": clientType, "newNetwork": newNetwork})

	err := n.populateAutoConfig(newNetwork.Config)
	if err != nil {
		return fmt.Errorf("Failed generating auto config: %w", err)
	}

	dbUpdateNeeded, changedKeys, oldNetwork, err := n.common.configChanged(newNetwork)
	if err != nil {
		return err
	}

	if !dbUpdateNeeded {
		return nil // Nothing changed.
	}

	// If the network as a whole has not had any previous creation attempts, or the node itself is still
	// pending, then don't apply the new settings to the node, just to the database record (ready for the
	// actual global create request to be initiated).
	if n.Status() == api.NetworkStatusPending || n.LocalStatus() == api.NetworkStatusPending {
		return n.common.update(newNetwork, targetNode, clientType)
	}

	reverter := revert.New()
	defer reverter.Fail()

	// Perform any pre-update cleanup needed if local member network was already created.
	if len(changedKeys) > 0 {
		// Define a function which reverts everything.
		reverter.Add(func() {
			// Reset changes to all nodes and database.
			_ = n.common.update(oldNetwork, targetNode, clientType)

			// Reset any change that was made to local bridge.
			_ = n.setup(newNetwork.Config)
		})

		// Bring the bridge down entirely if the driver has changed.
		if slices.Contains(changedKeys, "bridge.driver") && n.isRunning() {
			err = n.Stop()
			if err != nil {
				return err
			}
		}

		// Detach any external interfaces should no longer be attached.
		if slices.Contains(changedKeys, "bridge.external_interfaces") && n.isRunning() {
			devices := []string{}
			for _, dev := range strings.Split(newNetwork.Config["bridge.external_interfaces"], ",") {
				dev = strings.TrimSpace(dev)
				devices = append(devices, dev)
			}

			for _, dev := range strings.Split(oldNetwork.Config["bridge.external_interfaces"], ",") {
				dev = strings.TrimSpace(dev)
				if dev == "" {
					continue
				}

				// Test for extended configuration of external interface.
				ifName := dev
				devParts := strings.Split(dev, "/")
				if len(devParts) == 3 {
					ifName = strings.TrimSpace(devParts[0])
				}

				if !slices.Contains(devices, dev) && InterfaceExists(ifName) {
					err = DetachInterface(n.state, n.name, ifName)
					if err != nil {
						return err
					}

					// Remove the interface if it exists (and we created it).
					if len(devParts) == 3 {
						_, err := net.InterfaceByName(ifName)
						if err == nil {
							err = InterfaceRemove(ifName)
							if err != nil {
								return err
							}
						}
					}
				}
			}
		}
	}

	// Apply changes to all nodes and database.
	err = n.common.update(newNetwork, targetNode, clientType)
	if err != nil {
		return err
	}

	// Restart the network if needed.
	if len(changedKeys) > 0 {
		err = n.setup(oldNetwork.Config)
		if err != nil {
			return err
		}
	}

	reverter.Success()

	return nil
}

func (n *bridge) getTunnels() []string {
	tunnels := []string{}

	for k := range n.config {
		if !strings.HasPrefix(k, "tunnel.") {
			continue
		}

		fields := strings.Split(k, ".")
		if !slices.Contains(tunnels, fields[1]) {
			tunnels = append(tunnels, fields[1])
		}
	}

	return tunnels
}

// bootRoutesV4 returns a list of IPv4 boot routes on the network's device.
func (n *bridge) bootRoutesV4() ([]ip.Route, error) {
	r := &ip.Route{
		DevName: n.name,
		Proto:   "boot",
		Family:  ip.FamilyV4,
	}

	routes, err := r.List()
	if err != nil {
		return nil, err
	}

	return routes, nil
}

// bootRoutesV6 returns a list of IPv6 boot routes on the network's device.
func (n *bridge) bootRoutesV6() ([]ip.Route, error) {
	r := &ip.Route{
		DevName: n.name,
		Proto:   "boot",
		Family:  ip.FamilyV6,
	}

	routes, err := r.List()
	if err != nil {
		return nil, err
	}

	return routes, nil
}

// applyBootRoutesV4 applies a list of IPv4 boot routes to the network's device.
func (n *bridge) applyBootRoutesV4(routes []ip.Route) {
	for _, route := range routes {
		err := route.Replace()
		if err != nil {
			// If it fails, then we can't stop as the route has already gone, so just log and continue.
			n.logger.Error("Failed to restore route", logger.Ctx{"err": err})
		}
	}
}

// applyBootRoutesV6 applies a list of IPv6 boot routes to the network's device.
func (n *bridge) applyBootRoutesV6(routes []ip.Route) {
	for _, route := range routes {
		err := route.Replace()
		if err != nil {
			// If it fails, then we can't stop as the route has already gone, so just log and continue.
			n.logger.Error("Failed to restore route", logger.Ctx{"err": err})
		}
	}
}

// hasIPv4Firewall indicates whether the network has IPv4 firewall enabled.
func (n *bridge) hasIPv4Firewall() bool {
	// IPv4 firewall is only enabled if there is a bridge ipv4.address and ipv4.firewall enabled.
	if !util.IsNoneOrEmpty(n.config["ipv4.address"]) && util.IsTrueOrEmpty(n.config["ipv4.firewall"]) {
		return true
	}

	return false
}

// hasIPv6Firewall indicates whether the network has IPv6 firewall enabled.
func (n *bridge) hasIPv6Firewall() bool {
	// IPv6 firewall is only enabled if there is a bridge ipv6.address and ipv6.firewall enabled.
	if !util.IsNoneOrEmpty(n.config["ipv6.address"]) && util.IsTrueOrEmpty(n.config["ipv6.firewall"]) {
		return true
	}

	return false
}

// hasDHCPv4 indicates whether the network has DHCPv4 enabled.
// An empty ipv4.dhcp setting indicates enabled by default.
func (n *bridge) hasDHCPv4() bool {
	return util.IsTrueOrEmpty(n.config["ipv4.dhcp"])
}

// hasDHCPv6 indicates whether the network has DHCPv6 enabled.
// An empty ipv6.dhcp setting indicates enabled by default.
func (n *bridge) hasDHCPv6() bool {
	return util.IsTrueOrEmpty(n.config["ipv6.dhcp"])
}

// DHCPv4Subnet returns the DHCPv4 subnet (if DHCP is enabled on network).
func (n *bridge) DHCPv4Subnet() *net.IPNet {
	// DHCP is disabled on this network.
	if !n.hasDHCPv4() {
		return nil
	}

	// Return configured bridge subnet directly.
	_, subnet, err := net.ParseCIDR(n.config["ipv4.address"])
	if err != nil {
		return nil
	}

	return subnet
}

// DHCPv6Subnet returns the DHCPv6 subnet (if DHCP or SLAAC is enabled on network).
func (n *bridge) DHCPv6Subnet() *net.IPNet {
	// DHCP is disabled on this network.
	if !n.hasDHCPv6() {
		return nil
	}

	_, subnet, err := net.ParseCIDR(n.config["ipv6.address"])
	if err != nil {
		return nil
	}

	return subnet
}

// forwardConvertToFirewallForward converts forwards into format compatible with the firewall package.
func (n *bridge) forwardConvertToFirewallForwards(listenAddress net.IP, defaultTargetAddress net.IP, portMaps []*forwardPortMap) []firewallDrivers.AddressForward {
	var vips []firewallDrivers.AddressForward

	if defaultTargetAddress != nil {
		vips = append(vips, firewallDrivers.AddressForward{
			ListenAddress: listenAddress,
			TargetAddress: defaultTargetAddress,
		})
	}

	for _, portMap := range portMaps {
		vips = append(vips, firewallDrivers.AddressForward{
			ListenAddress: listenAddress,
			Protocol:      portMap.protocol,
			TargetAddress: portMap.target.address,
			ListenPorts:   portMap.listenPorts,
			TargetPorts:   portMap.target.ports,
			SNAT:          portMap.snat,
		})
	}

	return vips
}

// bridgeProjectNetworks takes a map of all networks in all projects and returns a filtered map of bridge networks.
func (n *bridge) bridgeProjectNetworks(projectNetworks map[string]map[int64]api.Network) map[string][]*api.Network {
	bridgeProjectNetworks := make(map[string][]*api.Network)
	for netProject, networks := range projectNetworks {
		for _, ni := range networks {
			network := ni // Local var creating pointer to rather than iterator.

			// Skip non-bridge networks.
			if network.Type != "bridge" {
				continue
			}

			if bridgeProjectNetworks[netProject] == nil {
				bridgeProjectNetworks[netProject] = []*api.Network{&network}
			} else {
				bridgeProjectNetworks[netProject] = append(bridgeProjectNetworks[netProject], &network)
			}
		}
	}

	return bridgeProjectNetworks
}

// bridgeNetworkExternalSubnets returns a list of external subnets used by bridge networks. Networks are considered
// to be using external subnets for their ipv4.address and/or ipv6.address if they have NAT disabled, and/or if
// they have external NAT addresses specified.
func (n *bridge) bridgeNetworkExternalSubnets(bridgeProjectNetworks map[string][]*api.Network) ([]externalSubnetUsage, error) {
	externalSubnets := make([]externalSubnetUsage, 0)
	for netProject, networks := range bridgeProjectNetworks {
		for _, netInfo := range networks {
			for _, keyPrefix := range []string{"ipv4", "ipv6"} {
				// If NAT is disabled, then network subnet is an external subnet.
				if util.IsFalseOrEmpty(netInfo.Config[fmt.Sprintf("%s.nat", keyPrefix)]) {
					key := fmt.Sprintf("%s.address", keyPrefix)

					_, ipNet, err := net.ParseCIDR(netInfo.Config[key])
					if err != nil {
						continue // Skip invalid/unspecified network addresses.
					}

					externalSubnets = append(externalSubnets, externalSubnetUsage{
						subnet:         *ipNet,
						networkProject: netProject,
						networkName:    netInfo.Name,
						usageType:      subnetUsageNetwork,
					})
				}

				// Find any external subnets used for network SNAT.
				if netInfo.Config[fmt.Sprintf("%s.nat.address", keyPrefix)] != "" {
					key := fmt.Sprintf("%s.nat.address", keyPrefix)

					subnetSize := 128
					if keyPrefix == "ipv4" {
						subnetSize = 32
					}

					_, ipNet, err := net.ParseCIDR(fmt.Sprintf("%s/%d", netInfo.Config[key], subnetSize))
					if err != nil {
						return nil, fmt.Errorf("Failed parsing %q of %q in project %q: %w", key, netInfo.Name, netProject, err)
					}

					externalSubnets = append(externalSubnets, externalSubnetUsage{
						subnet:         *ipNet,
						networkProject: netProject,
						networkName:    netInfo.Name,
						usageType:      subnetUsageNetworkSNAT,
					})
				}

				// Find any routes being used by the network.
				for _, cidr := range util.SplitNTrimSpace(netInfo.Config[fmt.Sprintf("%s.routes", keyPrefix)], ",", -1, true) {
					_, ipNet, err := net.ParseCIDR(cidr)
					if err != nil {
						continue // Skip invalid/unspecified network addresses.
					}

					externalSubnets = append(externalSubnets, externalSubnetUsage{
						subnet:         *ipNet,
						networkProject: netProject,
						networkName:    netInfo.Name,
						usageType:      subnetUsageNetwork,
					})
				}
			}
		}
	}

	return externalSubnets, nil
}

// bridgedNICExternalRoutes returns a list of external routes currently used by bridged NICs that are connected to
// networks specified.
func (n *bridge) bridgedNICExternalRoutes(bridgeProjectNetworks map[string][]*api.Network) ([]externalSubnetUsage, error) {
	externalRoutes := make([]externalSubnetUsage, 0)

	err := n.state.DB.Cluster.Transaction(context.TODO(), func(ctx context.Context, tx *db.ClusterTx) error {
		return tx.InstanceList(ctx, func(inst db.InstanceArgs, p api.Project) error {
			// Get the instance's effective network project name.
			instNetworkProject := project.NetworkProjectFromRecord(&p)

			if instNetworkProject != api.ProjectDefaultName {
				return nil // Managed bridge networks can only exist in default project.
			}

			devices := db.ExpandInstanceDevices(inst.Devices, inst.Profiles)

			// Iterate through each of the instance's devices, looking for bridged NICs that are linked to
			// networks specified.
			for devName, devConfig := range devices {
				if devConfig["type"] != "nic" {
					continue
				}

				// Check whether the NIC device references one of the networks supplied.
				if !NICUsesNetwork(devConfig, bridgeProjectNetworks[instNetworkProject]...) {
					continue
				}

				// For bridged NICs that are connected to networks specified, check if they have any
				// routes or external routes configured, and if so add them to the list to return.
				for _, key := range []string{"ipv4.routes", "ipv6.routes", "ipv4.routes.external", "ipv6.routes.external"} {
					for _, cidr := range util.SplitNTrimSpace(devConfig[key], ",", -1, true) {
						_, ipNet, _ := net.ParseCIDR(cidr)
						if ipNet == nil {
							// Skip if NIC device doesn't have a valid route.
							continue
						}

						externalRoutes = append(externalRoutes, externalSubnetUsage{
							subnet:          *ipNet,
							networkProject:  instNetworkProject,
							networkName:     devConfig["network"],
							instanceProject: inst.Project,
							instanceName:    inst.Name,
							instanceDevice:  devName,
							usageType:       subnetUsageInstance,
						})
					}
				}
			}

			return nil
		})
	})
	if err != nil {
		return nil, err
	}

	return externalRoutes, nil
}

// getExternalSubnetInUse returns information about usage of external subnets by bridge networks (and NICs
// connected to them) on this member.
func (n *bridge) getExternalSubnetInUse() ([]externalSubnetUsage, error) {
	var err error
	var projectNetworks map[string]map[int64]api.Network
	var projectNetworksForwardsOnUplink map[string]map[int64][]string
	var externalSubnets []externalSubnetUsage

	err = n.state.DB.Cluster.Transaction(context.TODO(), func(ctx context.Context, tx *db.ClusterTx) error {
		// Get all managed networks across all projects.
		projectNetworks, err = tx.GetCreatedNetworks(ctx)
		if err != nil {
			return fmt.Errorf("Failed to load all networks: %w", err)
		}

		// Get all network forward listen addresses for forwards assigned to this specific cluster member.
		networksByProjects, err := tx.GetNetworksAllProjects(ctx)
		if err != nil {
			return fmt.Errorf("Failed loading network forward listen addresses: %w", err)
		}

		for projectName, networks := range networksByProjects {
			for _, networkName := range networks {
				networkID, err := tx.GetNetworkID(ctx, projectName, networkName)
				if err != nil {
					return fmt.Errorf("Failed loading network forward listen addresses: %w", err)
				}

				// Get all network forward listen addresses for all networks (of any type) connected to our uplink.
				networkForwards, err := dbCluster.GetNetworkForwards(ctx, tx.Tx(), dbCluster.NetworkForwardFilter{
					NetworkID: &networkID,
				})
				if err != nil {
					return fmt.Errorf("Failed loading network forward listen addresses: %w", err)
				}

				projectNetworksForwardsOnUplink = make(map[string]map[int64][]string)
				for _, forward := range networkForwards {
					// Filter network forwards that belong to this specific cluster member
					if forward.NodeID.Valid && (forward.NodeID.Int64 == tx.GetNodeID()) {
						if projectNetworksForwardsOnUplink[projectName] == nil {
							projectNetworksForwardsOnUplink[projectName] = make(map[int64][]string)
						}

						projectNetworksForwardsOnUplink[projectName][networkID] = append(projectNetworksForwardsOnUplink[projectName][networkID], forward.ListenAddress)
					}
				}
			}
		}

		externalSubnets, err = n.common.getExternalSubnetInUse(ctx, tx, n.name, true)
		if err != nil {
			return fmt.Errorf("Failed getting external subnets in use: %w", err)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	// Get managed bridge networks.
	bridgeProjectNetworks := n.bridgeProjectNetworks(projectNetworks)

	// Get external subnets used by other managed bridge networks.
	bridgeNetworkExternalSubnets, err := n.bridgeNetworkExternalSubnets(bridgeProjectNetworks)
	if err != nil {
		return nil, err
	}

	// Get external routes configured on bridged NICs.
	bridgedNICExternalRoutes, err := n.bridgedNICExternalRoutes(bridgeProjectNetworks)
	if err != nil {
		return nil, err
	}

	externalSubnets = append(externalSubnets, bridgeNetworkExternalSubnets...)
	externalSubnets = append(externalSubnets, bridgedNICExternalRoutes...)

	err = n.state.DB.Cluster.Transaction(context.TODO(), func(ctx context.Context, tx *db.ClusterTx) error {
		// Detect if there are any conflicting proxy devices on all instances with the to be created network forward
		return tx.InstanceList(ctx, func(inst db.InstanceArgs, p api.Project) error {
			devices := db.ExpandInstanceDevices(inst.Devices, inst.Profiles)

			for devName, devConfig := range devices {
				if devConfig["type"] != "proxy" {
					continue
				}

				proxyListenAddr, err := ProxyParseAddr(devConfig["listen"])
				if err != nil {
					return err
				}

				proxySubnet, err := ParseIPToNet(proxyListenAddr.Address)
				if err != nil {
					continue // If proxy listen isn't a valid IP it can't conflict.
				}

				externalSubnets = append(externalSubnets, externalSubnetUsage{
					usageType:       subnetUsageProxy,
					subnet:          *proxySubnet,
					instanceProject: inst.Project,
					instanceName:    inst.Name,
					instanceDevice:  devName,
				})
			}

			return nil
		})
	})
	if err != nil {
		return nil, err
	}

	// Add forward listen addresses to this list.
	for projectName, networks := range projectNetworksForwardsOnUplink {
		for networkID, listenAddresses := range networks {
			for _, listenAddress := range listenAddresses {
				// Convert listen address to subnet.
				listenAddressNet, err := ParseIPToNet(listenAddress)
				if err != nil {
					return nil, fmt.Errorf("Invalid existing forward listen address %q", listenAddress)
				}

				// Create an externalSubnetUsage for the listen address by using the network ID
				// of the listen address to retrieve the already loaded network name from the
				// projectNetworks map.
				externalSubnets = append(externalSubnets, externalSubnetUsage{
					subnet:         *listenAddressNet,
					networkProject: projectName,
					networkName:    projectNetworks[projectName][networkID].Name,
					usageType:      subnetUsageNetworkForward,
				})
			}
		}
	}

	return externalSubnets, nil
}

// ForwardCreate creates a network forward.
func (n *bridge) ForwardCreate(forward api.NetworkForwardsPost, clientType request.ClientType) error {
	memberSpecific := true // bridge supports per-member forwards.

	err := n.state.DB.Cluster.Transaction(context.TODO(), func(ctx context.Context, tx *db.ClusterTx) error {
		// Check if there is an existing forward using the same listen address.
		networkID := n.ID()
		dbRecords, err := dbCluster.GetNetworkForwards(ctx, tx.Tx(), dbCluster.NetworkForwardFilter{
			NetworkID:     &networkID,
			ListenAddress: &forward.ListenAddress,
		})
		if err != nil {
			return err
		}

		filteredRecords := make([]dbCluster.NetworkForward, 0, len(dbRecords))
		for _, dbRecord := range dbRecords {
			// bridge supports per-member forwards so do memberSpecific filtering
			if !dbRecord.NodeID.Valid || (dbRecord.NodeID.Int64 == tx.GetNodeID()) {
				filteredRecords = append(filteredRecords, dbRecord)
			}
		}

		if len(filteredRecords) == 0 {
			return api.StatusErrorf(http.StatusNotFound, "Network forward not found")
		}

		if len(filteredRecords) > 1 {
			return api.StatusErrorf(http.StatusConflict, "Network forward found on more than one cluster member. Please target a specific member")
		}

		_, err = filteredRecords[0].ToAPI(ctx, tx.Tx())
		if err != nil {
			return err
		}

		return nil
	})
	if err == nil {
		return api.StatusErrorf(http.StatusConflict, "A forward for that listen address already exists")
	}

	// Convert listen address to subnet so we can check its valid and can be used.
	listenAddressNet, err := ParseIPToNet(forward.ListenAddress)
	if err != nil {
		return fmt.Errorf("Failed parsing address forward listen address %q: %w", forward.ListenAddress, err)
	}

	_, err = n.forwardValidate(listenAddressNet.IP, &forward.NetworkForwardPut)
	if err != nil {
		return err
	}

	externalSubnetsInUse, err := n.getExternalSubnetInUse()
	if err != nil {
		return err
	}

	// Check the listen address subnet doesn't fall within any existing network external subnets.
	for _, externalSubnetUser := range externalSubnetsInUse {
		// Check if usage is from our own network.
		if externalSubnetUser.networkProject == n.project && externalSubnetUser.networkName == n.name {
			// Skip checking conflict with our own network's subnet or SNAT address.
			// But do not allow other conflict with other usage types within our own network.
			if externalSubnetUser.usageType == subnetUsageNetwork || externalSubnetUser.usageType == subnetUsageNetworkSNAT {
				continue
			}
		}

		if SubnetContains(&externalSubnetUser.subnet, listenAddressNet) || SubnetContains(listenAddressNet, &externalSubnetUser.subnet) {
			// This error is purposefully vague so that it doesn't reveal any names of
			// resources potentially outside of the network.
			return fmt.Errorf("Forward listen address %q overlaps with another network or NIC", listenAddressNet.String())
		}
	}

	reverter := revert.New()
	defer reverter.Fail()

	var forwardID int64

	err = n.state.DB.Cluster.Transaction(context.TODO(), func(ctx context.Context, tx *db.ClusterTx) error {
		// Create forward DB record.
		nodeID := sql.NullInt64{
			Valid: memberSpecific,
			Int64: tx.GetNodeID(),
		}

		dbRecord := dbCluster.NetworkForward{
			NetworkID:     n.ID(),
			NodeID:        nodeID,
			ListenAddress: forward.ListenAddress,
			Description:   forward.Description,
			Ports:         forward.Ports,
		}

		if forward.Ports == nil {
			dbRecord.Ports = []api.NetworkForwardPort{}
		}

		forwardID, err = dbCluster.CreateNetworkForward(ctx, tx.Tx(), dbRecord)
		if err != nil {
			return err
		}

		err = dbCluster.CreateNetworkForwardConfig(ctx, tx.Tx(), forwardID, forward.Config)
		if err != nil {
			return err
		}

		return nil
	})
	if err != nil {
		return err
	}

	reverter.Add(func() {
		_ = n.state.DB.Cluster.Transaction(context.TODO(), func(ctx context.Context, tx *db.ClusterTx) error {
			return dbCluster.DeleteNetworkForward(ctx, tx.Tx(), n.ID(), forwardID)
		})
		_ = n.forwardSetupFirewall()
		_ = n.forwardBGPSetupPrefixes()
	})

	err = n.forwardSetupFirewall()
	if err != nil {
		return err
	}

	// Check if hairpin mode needs to be enabled on active NIC bridge ports.
	if n.config["bridge.driver"] != "openvswitch" {
		brNetfilterEnabled := false
		for _, ipVersion := range []uint{4, 6} {
			if BridgeNetfilterEnabled(ipVersion) == nil {
				brNetfilterEnabled = true
				break
			}
		}

		// If br_netfilter is enabled and bridge has forwards, we enable hairpin mode on each NIC's bridge
		// port in case any of the forwards target the NIC and the instance attempts to connect to the
		// forward's listener. Without hairpin mode on the target of the forward will not be able to
		// connect to the listener.
		if brNetfilterEnabled {
			var listenAddresses map[int64]string

			err := n.state.DB.Cluster.Transaction(context.TODO(), func(ctx context.Context, tx *db.ClusterTx) error {
				networkID := n.ID()
				dbRecords, err := dbCluster.GetNetworkForwards(ctx, tx.Tx(), dbCluster.NetworkForwardFilter{
					NetworkID: &networkID,
				})
				if err != nil {
					return err
				}

				listenAddresses = make(map[int64]string)
				for _, dbRecord := range dbRecords {
					if !dbRecord.NodeID.Valid || (dbRecord.NodeID.Int64 == tx.GetNodeID()) {
						listenAddresses[dbRecord.ID] = dbRecord.ListenAddress
					}
				}

				return err
			})
			if err != nil {
				return fmt.Errorf("Failed loading network forwards: %w", err)
			}

			// If we are the first forward on this bridge, enable hairpin mode on active NIC ports.
			if len(listenAddresses) <= 1 {
				filter := dbCluster.InstanceFilter{Node: &n.state.ServerName}

				err = n.state.DB.Cluster.Transaction(context.TODO(), func(ctx context.Context, tx *db.ClusterTx) error {
					return tx.InstanceList(ctx, func(inst db.InstanceArgs, p api.Project) error {
						// Get the instance's effective network project name.
						instNetworkProject := project.NetworkProjectFromRecord(&p)

						if instNetworkProject != api.ProjectDefaultName {
							return nil // Managed bridge networks can only exist in default project.
						}

						devices := db.ExpandInstanceDevices(inst.Devices.Clone(), inst.Profiles)

						// Iterate through each of the instance's devices, looking for bridged NICs
						// that are linked to this network.
						for devName, devConfig := range devices {
							if devConfig["type"] != "nic" {
								continue
							}

							// Check whether the NIC device references our network..
							if !NICUsesNetwork(devConfig, &api.Network{Name: n.Name()}) {
								continue
							}

							hostName := inst.Config[fmt.Sprintf("volatile.%s.host_name", devName)]
							if InterfaceExists(hostName) {
								link := &ip.Link{Name: hostName}
								err = link.BridgeLinkSetHairpin(true)
								if err != nil {
									return fmt.Errorf("Error enabling hairpin mode on bridge port %q: %w", link.Name, err)
								}

								n.logger.Debug("Enabled hairpin mode on NIC bridge port", logger.Ctx{"inst": inst.Name, "project": inst.Project, "device": devName, "dev": link.Name})
							}
						}

						return nil
					}, filter)
				})
				if err != nil {
					return err
				}
			}
		}
	}

	// Refresh exported BGP prefixes on local member.
	err = n.forwardBGPSetupPrefixes()
	if err != nil {
		return fmt.Errorf("Failed applying BGP prefixes for address forwards: %w", err)
	}

	reverter.Success()

	return nil
}

// ForwardUpdate updates a network forward.
func (n *bridge) ForwardUpdate(listenAddress string, req api.NetworkForwardPut, clientType request.ClientType) error {
	var curForwardID int64
	var curForward *api.NetworkForward

	var curNodeID sql.NullInt64

	err := n.state.DB.Cluster.Transaction(context.TODO(), func(ctx context.Context, tx *db.ClusterTx) error {
		var err error

		networkID := n.ID()
		dbRecords, err := dbCluster.GetNetworkForwards(ctx, tx.Tx(), dbCluster.NetworkForwardFilter{
			NetworkID:     &networkID,
			ListenAddress: &listenAddress,
		})
		if err != nil {
			return err
		}

		filteredRecords := make([]dbCluster.NetworkForward, 0, len(dbRecords))
		for _, dbRecord := range dbRecords {
			// bridge supports per-member forwards so do memberSpecific filtering
			if !dbRecord.NodeID.Valid || (dbRecord.NodeID.Int64 == tx.GetNodeID()) {
				filteredRecords = append(filteredRecords, dbRecord)
			}
		}

		if len(filteredRecords) == 0 {
			return api.StatusErrorf(http.StatusNotFound, "Network forward not found")
		}

		if len(filteredRecords) > 1 {
			return api.StatusErrorf(http.StatusConflict, "Network forward found on more than one cluster member. Please target a specific member")
		}

		curForwardID = filteredRecords[0].ID
		curNodeID = filteredRecords[0].NodeID
		curForward, err = filteredRecords[0].ToAPI(ctx, tx.Tx())
		if err != nil {
			return err
		}

		return nil
	})
	if err != nil {
		return err
	}

	_, err = n.forwardValidate(net.ParseIP(curForward.ListenAddress), &req)
	if err != nil {
		return err
	}

	curForwardEtagHash, err := localUtil.EtagHash(curForward.Etag())
	if err != nil {
		return err
	}

	newForward := api.NetworkForward{
		ListenAddress:     curForward.ListenAddress,
		NetworkForwardPut: req,
	}

	newForwardEtagHash, err := localUtil.EtagHash(newForward.Etag())
	if err != nil {
		return err
	}

	if curForwardEtagHash == newForwardEtagHash {
		return nil // Nothing has changed.
	}

	reverter := revert.New()
	defer reverter.Fail()

	err = n.state.DB.Cluster.Transaction(context.TODO(), func(ctx context.Context, tx *db.ClusterTx) error {
		fwd := dbCluster.NetworkForward{
			NetworkID:     n.ID(),
			NodeID:        curNodeID,
			ListenAddress: listenAddress,
			Description:   newForward.Description,
			Ports:         newForward.Ports,
		}

		err = dbCluster.UpdateNetworkForward(ctx, tx.Tx(), n.ID(), listenAddress, fwd)
		if err != nil {
			return err
		}

		err = dbCluster.UpdateNetworkForwardConfig(ctx, tx.Tx(), curForwardID, newForward.Config)
		if err != nil {
			return err
		}

		return nil
	})
	if err != nil {
		return err
	}

	reverter.Add(func() {
		_ = n.state.DB.Cluster.Transaction(context.TODO(), func(ctx context.Context, tx *db.ClusterTx) error {
			fwd := dbCluster.NetworkForward{
				NetworkID:     n.ID(),
				NodeID:        curNodeID,
				ListenAddress: listenAddress,
				Description:   curForward.Description,
				Ports:         curForward.Ports,
			}

			err = dbCluster.UpdateNetworkForward(ctx, tx.Tx(), n.ID(), listenAddress, fwd)
			if err != nil {
				return err
			}

			err = dbCluster.UpdateNetworkForwardConfig(ctx, tx.Tx(), curForwardID, curForward.Config)
			if err != nil {
				return err
			}

			return nil
		})
		_ = n.forwardSetupFirewall()
		_ = n.forwardBGPSetupPrefixes()
	})

	err = n.forwardSetupFirewall()
	if err != nil {
		return err
	}

	reverter.Success()

	return nil
}

// ForwardDelete deletes a network forward.
func (n *bridge) ForwardDelete(listenAddress string, clientType request.ClientType) error {
	memberSpecific := true // bridge supports per-member forwards.
	var forwardID int64
	var forward *api.NetworkForward

	err := n.state.DB.Cluster.Transaction(context.TODO(), func(ctx context.Context, tx *db.ClusterTx) error {
		var err error

		networkID := n.ID()
		dbRecords, err := dbCluster.GetNetworkForwards(ctx, tx.Tx(), dbCluster.NetworkForwardFilter{
			NetworkID:     &networkID,
			ListenAddress: &listenAddress,
		})
		if err != nil {
			return err
		}

		filteredRecords := make([]dbCluster.NetworkForward, 0, len(dbRecords))
		for _, dbRecord := range dbRecords {
			// bridge supports per-member forwards so do memberSpecific filtering
			if !dbRecord.NodeID.Valid || (dbRecord.NodeID.Int64 == tx.GetNodeID()) {
				filteredRecords = append(filteredRecords, dbRecord)
			}
		}

		if len(filteredRecords) == 0 {
			return api.StatusErrorf(http.StatusNotFound, "Network forward not found")
		}

		if len(filteredRecords) > 1 {
			return api.StatusErrorf(http.StatusConflict, "Network forward found on more than one cluster member. Please target a specific member")
		}

		forwardID = filteredRecords[0].ID
		forward, err = filteredRecords[0].ToAPI(ctx, tx.Tx())
		if err != nil {
			return err
		}

		return nil
	})
	if err != nil {
		return err
	}

	reverter := revert.New()
	defer reverter.Fail()

	err = n.state.DB.Cluster.Transaction(context.TODO(), func(ctx context.Context, tx *db.ClusterTx) error {
		return dbCluster.DeleteNetworkForward(ctx, tx.Tx(), n.ID(), forwardID)
	})
	if err != nil {
		return err
	}

	reverter.Add(func() {
		_ = n.state.DB.Cluster.Transaction(context.TODO(), func(ctx context.Context, tx *db.ClusterTx) error {
			nodeID := sql.NullInt64{
				Valid: memberSpecific,
				Int64: tx.GetNodeID(),
			}

			dbRecord := dbCluster.NetworkForward{
				NetworkID:     n.ID(),
				NodeID:        nodeID,
				ListenAddress: forward.ListenAddress,
				Description:   forward.Description,
				Ports:         forward.Ports,
			}

			if forward.Ports == nil {
				dbRecord.Ports = []api.NetworkForwardPort{}
			}

			forwardID, err = dbCluster.CreateNetworkForward(ctx, tx.Tx(), dbRecord)
			if err != nil {
				return err
			}

			err = dbCluster.CreateNetworkForwardConfig(ctx, tx.Tx(), forwardID, forward.Config)
			if err != nil {
				return err
			}

			return nil
		})

		_ = n.forwardSetupFirewall()
		_ = n.forwardBGPSetupPrefixes()
	})

	err = n.forwardSetupFirewall()
	if err != nil {
		return err
	}

	// Refresh exported BGP prefixes on local member.
	err = n.forwardBGPSetupPrefixes()
	if err != nil {
		return fmt.Errorf("Failed applying BGP prefixes for address forwards: %w", err)
	}

	reverter.Success()

	return nil
}

// forwardSetupFirewall applies all network address forwards defined for this network and this member.
func (n *bridge) forwardSetupFirewall() error {
	var forwards map[int64]*api.NetworkForward

	err := n.state.DB.Cluster.Transaction(context.TODO(), func(ctx context.Context, tx *db.ClusterTx) error {
		var err error

		networkID := n.ID()
		dbRecords, err := dbCluster.GetNetworkForwards(ctx, tx.Tx(), dbCluster.NetworkForwardFilter{
			NetworkID: &networkID,
		})
		if err != nil {
			return err
		}

		forwards = make(map[int64]*api.NetworkForward)
		for _, dbRecord := range dbRecords {
			if !dbRecord.NodeID.Valid || (dbRecord.NodeID.Int64 == tx.GetNodeID()) {
				forward, err := dbRecord.ToAPI(ctx, tx.Tx())
				if err != nil {
					return err
				}

				forwards[dbRecord.ID] = forward
			}
		}

		return err
	})
	if err != nil {
		return fmt.Errorf("Failed loading network forwards: %w", err)
	}

	var fwForwards []firewallDrivers.AddressForward
	ipVersions := make(map[uint]struct{})

	for _, forward := range forwards {
		// Convert listen address to subnet so we can check its valid and can be used.
		listenAddressNet, err := ParseIPToNet(forward.ListenAddress)
		if err != nil {
			return fmt.Errorf("Failed parsing address forward listen address %q: %w", forward.ListenAddress, err)
		}

		// Track which IP versions we are using.
		if listenAddressNet.IP.To4() == nil {
			ipVersions[6] = struct{}{}
		} else {
			ipVersions[4] = struct{}{}
		}

		portMaps, err := n.forwardValidate(listenAddressNet.IP, &forward.NetworkForwardPut)
		if err != nil {
			return fmt.Errorf("Failed validating firewall address forward for listen address %q: %w", forward.ListenAddress, err)
		}

		fwForwards = append(fwForwards, n.forwardConvertToFirewallForwards(listenAddressNet.IP, net.ParseIP(forward.Config["target_address"]), portMaps)...)
	}

	if len(forwards) > 0 {
		// Check if br_netfilter is enabled to, and warn if not.
		brNetfilterWarning := false
		for ipVersion := range ipVersions {
			err = BridgeNetfilterEnabled(ipVersion)
			if err != nil {
				brNetfilterWarning = true
				msg := fmt.Sprintf("IPv%d bridge netfilter not enabled. Instances using the bridge will not be able to connect to the forward listen IPs", ipVersion)
				n.logger.Warn(msg, logger.Ctx{"err": err})
				err = n.state.DB.Cluster.Transaction(context.TODO(), func(ctx context.Context, tx *db.ClusterTx) error {
					return tx.UpsertWarningLocalNode(ctx, n.project, dbCluster.TypeNetwork, int(n.id), warningtype.ProxyBridgeNetfilterNotEnabled, fmt.Sprintf("%s: %v", msg, err))
				})
				if err != nil {
					n.logger.Warn("Failed to create warning", logger.Ctx{"err": err})
				}
			}
		}

		if !brNetfilterWarning {
			err = warnings.ResolveWarningsByLocalNodeAndProjectAndTypeAndEntity(n.state.DB.Cluster, n.project, warningtype.ProxyBridgeNetfilterNotEnabled, dbCluster.TypeNetwork, int(n.id))
			if err != nil {
				n.logger.Warn("Failed to resolve warning", logger.Ctx{"err": err})
			}
		}
	}

	err = n.state.Firewall.NetworkApplyForwards(n.name, fwForwards)
	if err != nil {
		return fmt.Errorf("Failed applying firewall address forwards: %w", err)
	}

	return nil
}

// Leases returns a list of leases for the bridged network. It will reach out to other cluster members as needed.
// The projectName passed here refers to the initial project from the API request which may differ from the network's project.
func (n *bridge) Leases(projectName string, clientType request.ClientType) ([]api.NetworkLease, error) {
	var err error
	var projectMacs []string
	leases := []api.NetworkLease{}

	// Get all static leases.
	if clientType == request.ClientTypeNormal {
		// If requested project matches network's project then include gateway and downstream uplink IPs.
		if projectName == n.project {
			// Add our own gateway IPs.
			for _, addr := range []string{n.config["ipv4.address"], n.config["ipv6.address"]} {
				ip, _, _ := net.ParseCIDR(addr)
				if ip != nil {
					leases = append(leases, api.NetworkLease{
						Hostname: fmt.Sprintf("%s.gw", n.Name()),
						Address:  ip.String(),
						Type:     "gateway",
					})
				}
			}

			// Include downstream OVN routers using the network as an uplink.
			var projectNetworks map[string]map[int64]api.Network
			err = n.state.DB.Cluster.Transaction(context.TODO(), func(ctx context.Context, tx *db.ClusterTx) error {
				projectNetworks, err = tx.GetCreatedNetworks(ctx)
				return err
			})
			if err != nil {
				return nil, err
			}

			// Look for networks using the current network as an uplink.
			for projectName, networks := range projectNetworks {
				for _, network := range networks {
					if network.Config["network"] != n.name {
						continue
					}

					// Found a network, add leases.
					for _, k := range []string{"volatile.network.ipv4.address", "volatile.network.ipv6.address"} {
						v := network.Config[k]
						if v != "" {
							leases = append(leases, api.NetworkLease{
								Hostname: fmt.Sprintf("%s-%s.uplink", projectName, network.Name),
								Address:  v,
								Type:     "uplink",
							})
						}
					}
				}
			}
		}

		// Get all the instances in the requested project that are connected to this network.
		filter := dbCluster.InstanceFilter{Project: &projectName}
		err = UsedByInstanceDevices(n.state, n.Project(), n.Name(), n.Type(), func(inst db.InstanceArgs, nicName string, nicConfig map[string]string) error {
			// Fill in the hwaddr from volatile.
			if nicConfig["hwaddr"] == "" {
				nicConfig["hwaddr"] = inst.Config[fmt.Sprintf("volatile.%s.hwaddr", nicName)]
			}

			// Record the MAC.
			hwAddr, _ := net.ParseMAC(nicConfig["hwaddr"])
			if hwAddr != nil {
				projectMacs = append(projectMacs, hwAddr.String())
			}

			// Add the lease.
			nicIP4 := net.ParseIP(nicConfig["ipv4.address"])
			if nicIP4 != nil {
				leases = append(leases, api.NetworkLease{
					Hostname: inst.Name,
					Address:  nicIP4.String(),
					Hwaddr:   hwAddr.String(),
					Type:     "static",
					Location: inst.Node,
				})
			}

			nicIP6 := net.ParseIP(nicConfig["ipv6.address"])
			if nicIP6 != nil {
				leases = append(leases, api.NetworkLease{
					Hostname: inst.Name,
					Address:  nicIP6.String(),
					Hwaddr:   hwAddr.String(),
					Type:     "static",
					Location: inst.Node,
				})
			}

			// Add EUI64 records.
			_, netIP6, _ := net.ParseCIDR(n.config["ipv6.address"])
			if netIP6 != nil && hwAddr != nil && util.IsFalseOrEmpty(n.config["ipv6.dhcp.stateful"]) {
				eui64IP6, err := eui64.ParseMAC(netIP6.IP, hwAddr)
				if err == nil {
					leases = append(leases, api.NetworkLease{
						Hostname: inst.Name,
						Address:  eui64IP6.String(),
						Hwaddr:   hwAddr.String(),
						Type:     "dynamic",
						Location: inst.Node,
					})
				}
			}

			return nil
		}, filter)
		if err != nil {
			return nil, err
		}
	}

	// Get dynamic leases.
	leaseFile := internalUtil.VarPath("networks", n.name, "dnsmasq.leases")
	if !util.PathExists(leaseFile) {
		return leases, nil
	}

	content, err := os.ReadFile(leaseFile)
	if err != nil {
		return nil, err
	}

	for _, lease := range strings.Split(string(content), "\n") {
		fields := strings.Fields(lease)
		if len(fields) >= 5 {
			// Parse the MAC.
			mac := GetMACSlice(fields[1])
			macStr := strings.Join(mac, ":")

			if len(macStr) < 17 && fields[4] != "" {
				macStr = fields[4][len(fields[4])-17:]
			}

			// Look for an existing static entry.
			found := false
			for _, entry := range leases {
				if entry.Hwaddr == macStr && entry.Address == fields[2] {
					found = true
					break
				}
			}

			if found {
				continue
			}

			// DHCPv6 leases can't be tracked down to a MAC so clear the field.
			// This means that instance project filtering will not work on IPv6 leases.
			if strings.Contains(fields[2], ":") {
				macStr = ""
			}

			// Skip leases that don't match any of the instance MACs from the project (only when we
			// have populated the projectMacs list in ClientTypeNormal mode). Otherwise get all local
			// leases and they will be filtered on the server handling the end user request.
			if clientType == request.ClientTypeNormal && macStr != "" && !slices.Contains(projectMacs, macStr) {
				continue
			}

			// Add the lease to the list.
			leases = append(leases, api.NetworkLease{
				Hostname: fields[3],
				Address:  fields[2],
				Hwaddr:   macStr,
				Type:     "dynamic",
				Location: n.state.ServerName,
			})
		}
	}

	// Collect leases from other servers.
	if clientType == request.ClientTypeNormal {
		notifier, err := cluster.NewNotifier(n.state, n.state.Endpoints.NetworkCert(), n.state.ServerCert(), cluster.NotifyAll)
		if err != nil {
			return nil, err
		}

		err = notifier(func(client incus.InstanceServer) error {
			memberLeases, err := client.GetNetworkLeases(n.name)
			if err != nil {
				return err
			}

			// Add local leases from other members, filtering them for MACs that belong to the project.
			for _, lease := range memberLeases {
				if lease.Hwaddr != "" && slices.Contains(projectMacs, lease.Hwaddr) {
					leases = append(leases, lease)
				}
			}

			return nil
		})
		if err != nil {
			return nil, err
		}
	}

	return leases, nil
}

// UsesDNSMasq indicates if network's config indicates if it needs to use dnsmasq.
func (n *bridge) UsesDNSMasq() bool {
	// Skip dnsmasq when no connectivity is configured.
	if util.IsNoneOrEmpty(n.config["ipv4.address"]) && util.IsNoneOrEmpty(n.config["ipv6.address"]) {
		return false
	}

	// Start dnsmasq if providing instance DNS records.
	if n.config["dns.mode"] != "none" {
		return true
	}

	// Start dnsmassq if IPv6 is used (needed for SLAAC or DHCPv6).
	if !util.IsNoneOrEmpty(n.config["ipv6.address"]) {
		return true
	}

	// Start dnsmasq if IPv4 DHCP is used.
	if !util.IsNoneOrEmpty(n.config["ipv4.address"]) && n.hasDHCPv4() {
		return true
	}

	return false
}

func (n *bridge) deleteChildren() error {
	// Get a list of interfaces
	ifaces, err := net.Interfaces()
	if err != nil {
		return err
	}

	var externalInterfaces []string
	if n.config["bridge.external_interfaces"] != "" {
		for _, entry := range strings.Split(n.config["bridge.external_interfaces"], ",") {
			entry = strings.Split(strings.TrimSpace(entry), "/")[0]
			externalInterfaces = append(externalInterfaces, entry)
		}
	}

	kinds := []string{
		"vxlan",
		"gretap",
		"dummy",
	}

	for _, iface := range ifaces {
		l, err := ip.LinkByName(iface.Name)
		if err != nil {
			// If we can't load the link, chances are the interface isn't one that we should be deleting.
			continue
		}

		if l.Master != n.name || slices.Contains(externalInterfaces, iface.Name) || !slices.Contains(kinds, l.Kind) {
			continue
		}

		err = l.Delete()
		if err != nil {
			return err
		}
	}

	return nil
}
