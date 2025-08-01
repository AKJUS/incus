package validate

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/adhocore/gronx"
	"github.com/google/uuid"
	"github.com/kballard/go-shellquote"
	"gopkg.in/yaml.v2"

	"github.com/lxc/incus/v6/shared/osarch"
	"github.com/lxc/incus/v6/shared/units"
	"github.com/lxc/incus/v6/shared/util"
)

// And returns a function that runs one or more validators, all must pass without error.
func And(validators ...func(value string) error) func(value string) error {
	return func(value string) error {
		for _, validator := range validators {
			err := validator(value)
			if err != nil {
				return err
			}
		}

		return nil
	}
}

// Or returns a function that runs one or more validators, at least one must pass without error.
func Or(validators ...func(value string) error) func(value string) error {
	return func(value string) error {
		for _, validator := range validators {
			err := validator(value)
			if err == nil {
				return nil
			}
		}

		return fmt.Errorf("%q isn't a valid value", value)
	}
}

// Required returns a function that runs one or more validators, all must pass without error.
func Required(validators ...func(value string) error) func(value string) error {
	return And(validators...)
}

// Optional wraps Required() function to make it return nil if value is empty string.
func Optional(validators ...func(value string) error) func(value string) error {
	return func(value string) error {
		if value == "" {
			return nil
		}

		return Required(validators...)(value)
	}
}

// IsInt64 validates whether the string can be converted to an int64.
func IsInt64(value string) error {
	_, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return fmt.Errorf("Invalid value for an integer %q", value)
	}

	return nil
}

// IsUint8 validates whether the string can be converted to an uint8.
func IsUint8(value string) error {
	_, err := strconv.ParseUint(value, 10, 8)
	if err != nil {
		return fmt.Errorf("Invalid value for an integer %q. Must be between 0 and 255", value)
	}

	return nil
}

// IsUint32 validates whether the string can be converted to an uint32.
func IsUint32(value string) error {
	_, err := strconv.ParseUint(value, 10, 32)
	if err != nil {
		return fmt.Errorf("Invalid value for uint32 %q: %w", value, err)
	}

	return nil
}

// IsWWN validates whether the string can be converted to a uint64 WWN.
func IsWWN(value string) error {
	if !strings.HasPrefix(value, "0x") {
		return fmt.Errorf("Invalid value for a WWN %q: Missing expected 0x prefix", value)
	}

	_, err := strconv.ParseUint(strings.TrimPrefix(value, "0x"), 16, 64)
	if err != nil {
		return fmt.Errorf("Invalid value for a WWN %q: %w", value, err)
	}

	return nil
}

// IsUint32Range validates whether the string is a uint32 range in the form "number" or "start-end".
func IsUint32Range(value string) error {
	_, _, err := util.ParseUint32Range(value)
	return err
}

// IsInRange checks whether an integer is within a specific range.
func IsInRange(minValue int64, maxValue int64) func(value string) error {
	return func(value string) error {
		valueInt, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return fmt.Errorf("Invalid value for an integer %q", value)
		}

		if valueInt < minValue || valueInt > maxValue {
			return fmt.Errorf("Value isn't within valid range. Must be between %d and %d", minValue, maxValue)
		}

		return nil
	}
}

// IsPriority validates priority number.
func IsPriority(value string) error {
	valueInt, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return fmt.Errorf("Invalid value for an integer %q", value)
	}

	if valueInt < 0 || valueInt > 10 {
		return fmt.Errorf("Invalid value for a limit %q. Must be between 0 and 10", value)
	}

	return nil
}

// IsBool validates if string can be understood as a bool.
func IsBool(value string) error {
	if !slices.Contains([]string{"true", "false", "yes", "no", "1", "0", "on", "off"}, strings.ToLower(value)) {
		return fmt.Errorf("Invalid value for a boolean %q", value)
	}

	return nil
}

// IsOneOf checks whether the string is present in the supplied slice of strings.
func IsOneOf(valid ...string) func(value string) error {
	return func(value string) error {
		if !slices.Contains(valid, value) {
			return fmt.Errorf("Invalid value %q (not one of %s)", value, valid)
		}

		return nil
	}
}

// IsAny accepts all strings as valid.
func IsAny(_ string) error {
	return nil
}

// IsListOf returns a validator for a comma separated list of values.
func IsListOf(validator func(value string) error) func(value string) error {
	return func(value string) error {
		for _, v := range strings.Split(value, ",") {
			v = strings.TrimSpace(v)

			err := validator(v)
			if err != nil {
				return fmt.Errorf("Item %q: %w", v, err)
			}
		}

		return nil
	}
}

// IsNotEmpty requires a non-empty string.
func IsNotEmpty(value string) error {
	if value == "" {
		return errors.New("Required value")
	}

	return nil
}

// IsSize checks if string is valid size according to units.ParseByteSizeString.
func IsSize(value string) error {
	_, err := units.ParseByteSizeString(value)
	if err != nil {
		return err
	}

	return nil
}

// IsDeviceID validates string is four lowercase hex characters suitable as Vendor or Device ID.
func IsDeviceID(value string) error {
	match, _ := regexp.MatchString(`^[0-9a-f]{4}$`, value)
	if !match {
		return errors.New("Invalid value, must be four lower case hex characters")
	}

	return nil
}

// IsInterfaceName validates a real network interface name.
func IsInterfaceName(value string) error {
	// Validate the length.
	if len(value) < 2 {
		return errors.New("Network interface is too short (minimum 2 characters)")
	}

	if len(value) > 15 {
		return errors.New("Network interface is too long (maximum 15 characters)")
	}

	// Validate the character set.
	match, _ := regexp.MatchString(`^[-_a-zA-Z0-9.]+$`, value)
	if !match {
		return errors.New("Network interface contains invalid characters")
	}

	return nil
}

// IsNetworkName validates a name usable for a network.
func IsNetworkName(value string) error {
	err := IsInterfaceName(value)
	if err != nil {
		return err
	}

	err = IsURLSegmentSafe(value)
	if err != nil {
		return err
	}

	if strings.Contains(value, ":") {
		return fmt.Errorf("Cannot contain %q", ":")
	}

	return nil
}

// IsNetworkMAC validates an Ethernet MAC address. e.g. "00:00:5e:00:53:01".
func IsNetworkMAC(value string) error {
	_, err := net.ParseMAC(value)

	// Check is valid Ethernet MAC length and delimiter.
	if err != nil || len(value) != 17 || strings.ContainsAny(value, "-.") {
		return errors.New("Invalid MAC address, must be 6 bytes of hex separated by colons")
	}

	return nil
}

// IsNetworkAddress validates an IP (v4 or v6) address string.
func IsNetworkAddress(value string) error {
	ip := net.ParseIP(value)
	if ip == nil {
		return fmt.Errorf("Not an IP address %q", value)
	}

	return nil
}

// IsNetwork validates an IP network CIDR string.
func IsNetwork(value string) error {
	ip, subnet, err := net.ParseCIDR(value)
	if err != nil {
		return err
	}

	if ip.String() != subnet.IP.String() {
		return fmt.Errorf("Not an IP network address %q", value)
	}

	return nil
}

// IsNetworkAddressCIDR validates an IP address string in CIDR format.
func IsNetworkAddressCIDR(value string) error {
	_, _, err := net.ParseCIDR(value)
	if err != nil {
		return err
	}

	return nil
}

// IsNetworkRange validates an IP range in the format "start-end".
func IsNetworkRange(value string) error {
	ips := strings.SplitN(value, "-", 2)
	if len(ips) != 2 {
		return errors.New("IP range must contain start and end IP addresses")
	}

	startIP := net.ParseIP(ips[0])
	if startIP == nil {
		return fmt.Errorf("Start not an IP address %q", ips[0])
	}

	endIP := net.ParseIP(ips[1])
	if endIP == nil {
		return fmt.Errorf("End not an IP address %q", ips[1])
	}

	if (startIP.To4() != nil) != (endIP.To4() != nil) {
		return errors.New("Start and end IP addresses are not in same family")
	}

	if bytes.Compare(startIP, endIP) > 0 {
		return errors.New("Start IP address must be before or equal to end IP address")
	}

	return nil
}

// IsNetworkV4 validates an IPv4 CIDR string.
func IsNetworkV4(value string) error {
	ip, subnet, err := net.ParseCIDR(value)
	if err != nil {
		return err
	}

	if ip.To4() == nil {
		return fmt.Errorf("Not an IPv4 network %q", value)
	}

	if ip.String() != subnet.IP.String() {
		return fmt.Errorf("Not an IPv4 network address %q", value)
	}

	return nil
}

// IsNetworkAddressV4 validates an IPv4 address string.
func IsNetworkAddressV4(value string) error {
	ip := net.ParseIP(value)
	if ip == nil || ip.To4() == nil {
		return fmt.Errorf("Not an IPv4 address %q", value)
	}

	return nil
}

// IsNetworkAddressCIDRV4 validates an IPv4 address string in CIDR format.
func IsNetworkAddressCIDRV4(value string) error {
	ip, subnet, err := net.ParseCIDR(value)
	if err != nil {
		return err
	}

	if ip.To4() == nil {
		return fmt.Errorf("Not an IPv4 address %q", value)
	}

	if ip.String() == subnet.IP.String() {
		return fmt.Errorf("Not a usable IPv4 address %q", value)
	}

	return nil
}

// IsNetworkRangeV4 validates an IPv4 range in the format "start-end".
func IsNetworkRangeV4(value string) error {
	ips := strings.SplitN(value, "-", 2)
	if len(ips) != 2 {
		return errors.New("IP range must contain start and end IP addresses")
	}

	for _, ip := range ips {
		err := IsNetworkAddressV4(ip)
		if err != nil {
			return err
		}
	}

	return nil
}

// IsNetworkV6 validates an IPv6 CIDR string.
func IsNetworkV6(value string) error {
	ip, subnet, err := net.ParseCIDR(value)
	if err != nil {
		return err
	}

	if ip == nil || ip.To4() != nil {
		return fmt.Errorf("Not an IPv6 network %q", value)
	}

	if ip.String() != subnet.IP.String() {
		return fmt.Errorf("Not an IPv6 network address %q", value)
	}

	return nil
}

// IsNetworkAddressV6 validates an IPv6 address string.
func IsNetworkAddressV6(value string) error {
	ip := net.ParseIP(value)
	if ip == nil || ip.To4() != nil {
		return fmt.Errorf("Not an IPv6 address %q", value)
	}

	return nil
}

// IsNetworkAddressCIDRV6 validates an IPv6 address string in CIDR format.
func IsNetworkAddressCIDRV6(value string) error {
	ip, subnet, err := net.ParseCIDR(value)
	if err != nil {
		return err
	}

	if ip.To4() != nil {
		return fmt.Errorf("Not an IPv6 address %q", value)
	}

	if ip.String() == subnet.IP.String() {
		return fmt.Errorf("Not a usable IPv6 address %q", value)
	}

	return nil
}

// IsNetworkRangeV6 validates an IPv6 range in the format "start-end".
func IsNetworkRangeV6(value string) error {
	ips := strings.SplitN(value, "-", 2)
	if len(ips) != 2 {
		return errors.New("IP range must contain start and end IP addresses")
	}

	for _, ip := range ips {
		err := IsNetworkAddressV6(ip)
		if err != nil {
			return err
		}
	}

	return nil
}

// IsNetworkVLAN validates a VLAN ID.
func IsNetworkVLAN(value string) error {
	vlanID, err := strconv.Atoi(value)
	if err != nil {
		return fmt.Errorf("Invalid VLAN ID %q", value)
	}

	if vlanID < 0 || vlanID > 4094 {
		return fmt.Errorf("Out of VLAN ID range (0-4094) %q", value)
	}

	return nil
}

// IsNetworkMTU validates MTU number >= 1280 and <= 16384.
// Anything below 68 and the kernel doesn't allow IPv4, anything below 1280 and the kernel doesn't allow IPv6.
// So require an IPv6-compatible MTU as the low value and cap at the max ethernet jumbo frame size.
func IsNetworkMTU(value string) error {
	mtu, err := strconv.ParseUint(value, 10, 32)
	if err != nil {
		return fmt.Errorf("Invalid MTU %q", value)
	}

	if mtu < 1280 || mtu > 16384 {
		return fmt.Errorf("Out of MTU range (1280-16384) %q", value)
	}

	return nil
}

// IsNetworkPort validates an IP port number >= 0 and <= 65535.
func IsNetworkPort(value string) error {
	port, err := strconv.ParseUint(value, 10, 32)
	if err != nil {
		return fmt.Errorf("Invalid port number %q", value)
	}

	if port > 65535 {
		return fmt.Errorf("Out of port number range (0-65535) %q", value)
	}

	return nil
}

// IsNetworkPortRange validates an IP port range in the format "port" or "start-end".
func IsNetworkPortRange(value string) error {
	ports := strings.SplitN(value, "-", 2)
	portsLen := len(ports)
	if portsLen != 1 && portsLen != 2 {
		return errors.New("Port range must contain either a single port or start and end port numbers")
	}

	startPort, err := strconv.ParseUint(ports[0], 10, 32)
	if err != nil {
		return fmt.Errorf("Invalid port number %q", value)
	}

	if portsLen == 2 {
		endPort, err := strconv.ParseUint(ports[1], 10, 32)
		if err != nil {
			return fmt.Errorf("Invalid end port number %q", value)
		}

		if startPort >= endPort {
			return fmt.Errorf("Start port %d must be lower than end port %d", startPort, endPort)
		}
	}

	return nil
}

// IsDHCPRouteList validates a comma-separated list of alternating CIDR networks and IP addresses.
func IsDHCPRouteList(value string) error {
	parts := strings.Split(value, ",")
	for i, s := range parts {
		// routes are pairs of subnet and gateway
		var err error
		if i%2 == 0 { // subnet part
			err = IsNetworkV4(s)
		} else { // gateway part
			err = IsNetworkAddressV4(s)
		}

		if err != nil {
			return err
		}
	}

	if len(parts)%2 != 0 { // uneven number of parts means the gateway of the last route is missing
		return fmt.Errorf("missing gateway for route %v", parts[len(parts)-1])
	}

	return nil
}

// IsURLSegmentSafe validates whether value can be used in a URL segment.
func IsURLSegmentSafe(value string) error {
	for _, char := range []string{"/", "?", "&", "+"} {
		if strings.Contains(value, char) {
			return fmt.Errorf("Cannot contain %q", char)
		}
	}

	return nil
}

// IsUUID validates whether a value is a UUID.
func IsUUID(value string) error {
	_, err := uuid.Parse(value)
	if err != nil {
		return errors.New("Invalid UUID")
	}

	return nil
}

// IsPCIAddress validates whether a value is a PCI address.
func IsPCIAddress(value string) error {
	match, _ := regexp.MatchString(`^(?:[0-9a-fA-F]{4}:)?[0-9a-fA-F]{2}:[0-9a-fA-F]{2}\.[0-9a-fA-F]$`, value)
	if !match {
		return errors.New("Invalid PCI address")
	}

	return nil
}

// IsCompressionAlgorithm validates whether a value is a valid compression algorithm and is available on the system.
func IsCompressionAlgorithm(value string) error {
	if value == "none" {
		return nil
	}

	// Going to look up tar2sqfs executable binary
	if value == "squashfs" {
		value = "tar2sqfs"
	}

	// Parse the command.
	fields, err := shellquote.Split(value)
	if err != nil {
		return err
	}

	_, err = exec.LookPath(fields[0])
	return err
}

// IsArchitecture validates whether the value is a valid architecture name.
func IsArchitecture(value string) error {
	return IsOneOf(osarch.SupportedArchitectures()...)(value)
}

// IsCron checks that it's a valid cron pattern or alias.
func IsCron(aliases []string) func(value string) error {
	return func(value string) error {
		isValid := func(value string) error {
			// Accept valid aliases.
			if slices.Contains(aliases, value) {
				return nil
			}

			if gronx.IsValid(value) {
				return nil
			}

			return fmt.Errorf("Error parsing cron expr: %s", value)
		}

		// Can be comma+space separated (just commas are valid cron pattern).
		value = strings.ToLower(value)
		triggers := strings.Split(value, ", ")
		for _, trigger := range triggers {
			err := isValid(trigger)
			if err != nil {
				return err
			}
		}

		return nil
	}
}

// IsListenAddress returns a validator for a listen address.
func IsListenAddress(allowDNS bool, allowWildcard bool, requirePort bool) func(value string) error {
	return func(value string) error {
		// Validate address format and port.
		host, _, err := net.SplitHostPort(value)
		if err != nil {
			if requirePort {
				return errors.New("A port is required as part of the address")
			}

			host = value
		}

		// Validate wildcard.
		if slices.Contains([]string{"", "::", "[::]", "0.0.0.0"}, host) {
			if !allowWildcard {
				return errors.New("Wildcard addresses aren't allowed")
			}

			return nil
		}

		// Validate DNS.
		ip := net.ParseIP(strings.Trim(host, "[]"))
		if ip != nil {
			return nil
		}

		if !allowDNS {
			return errors.New("DNS names not allowed in address")
		}

		_, err = net.LookupHost(host)
		if err != nil {
			return fmt.Errorf("Couldn't resolve %q", host)
		}

		return nil
	}
}

// IsAbsFilePath checks if value is an absolute file path.
func IsAbsFilePath(value string) error {
	if !filepath.IsAbs(value) {
		return errors.New("Must be absolute file path")
	}

	return nil
}

// ParseNetworkVLANRange parses a VLAN range in the form "number" or "start-end".
// Returns the start number and the number of items in the range.
func ParseNetworkVLANRange(vlan string) (int, int, error) {
	err := IsNetworkVLAN(vlan)
	if err == nil {
		vlanRangeStart, err := strconv.Atoi(vlan)
		if err != nil {
			return -1, -1, err
		}

		return vlanRangeStart, 1, nil
	}

	vlanRange := strings.Split(vlan, "-")
	if len(vlanRange) != 2 {
		return -1, -1, fmt.Errorf("Invalid VLAN range input: %s", vlan)
	}

	if IsNetworkVLAN(vlanRange[0]) != nil || IsNetworkVLAN(vlanRange[1]) != nil {
		return -1, -1, fmt.Errorf("Invalid VLAN range boundary. start:%s, end:%s", vlanRange[0], vlanRange[1])
	}

	vlanRangeStart, err := strconv.Atoi(vlanRange[0])
	if err != nil {
		return -1, -1, err
	}

	vlanRangeEnd, err := strconv.Atoi(vlanRange[1])
	if err != nil {
		return -1, -1, err
	}

	if vlanRangeStart > vlanRangeEnd {
		return -1, -1, fmt.Errorf("Invalid VLAN range boundary. start:%d is higher than end:%d", vlanRangeStart, vlanRangeEnd)
	}

	return vlanRangeStart, vlanRangeEnd - vlanRangeStart + 1, nil
}

// IsHostname checks the string is valid DNS hostname.
func IsHostname(name string) error {
	// Validate length
	if len(name) < 1 || len(name) > 63 {
		return errors.New("Name must be 1-63 characters long")
	}

	// Validate first character
	if strings.HasPrefix(name, "-") {
		return errors.New(`Name must not start with "-" character`)
	}

	// Validate last character
	if strings.HasSuffix(name, "-") {
		return errors.New(`Name must not end with "-" character`)
	}

	_, err := strconv.ParseUint(name, 10, 64)
	if err == nil {
		return errors.New("Name cannot be a number")
	}

	match, err := regexp.MatchString(`^[\-a-zA-Z0-9]+$`, name)
	if err != nil {
		return err
	}

	if !match {
		return errors.New("Name can only contain alphanumeric and hyphen characters")
	}

	return nil
}

// IsDeviceName checks name is 1-63 characters long, doesn't start with a full stop and contains only alphanumeric,
// forward slash, hyphen, colon, underscore and full stop characters.
func IsDeviceName(name string) error {
	if len(name) < 1 || len(name) > 63 {
		return errors.New("Name must be 1-63 characters long")
	}

	if string(name[0]) == "." {
		return errors.New(`Name must not start with "." character`)
	}

	match, err := regexp.MatchString(`^[\/\.\-:_a-zA-Z0-9]+$`, name)
	if err != nil {
		return err
	}

	if !match {
		return errors.New("Name can only contain alphanumeric, forward slash, hyphen, colon, underscore and full stop characters")
	}

	return nil
}

// IsRequestURL checks value is a valid HTTP/HTTPS request URL.
func IsRequestURL(value string) error {
	if value == "" {
		return errors.New("Empty URL")
	}

	_, err := url.ParseRequestURI(value)
	if err != nil {
		return fmt.Errorf("Invalid URL: %w", err)
	}

	return nil
}

// IsCloudInitUserData checks value is valid cloud-init user data.
func IsCloudInitUserData(value string) error {
	if value == "#cloud-config" || strings.HasPrefix(value, "#cloud-config\n") {
		lines := strings.SplitN(value, "\n", 2)

		// If value only contains the cloud-config header, it is valid.
		if len(lines) == 1 {
			return nil
		}

		return IsYAML(lines[1])
	}

	// Since there are various other user-data formats besides cloud-config, consider those valid.
	return nil
}

// IsYAML checks value is valid YAML.
func IsYAML(value string) error {
	out := struct{}{}

	err := yaml.Unmarshal([]byte(value), &out)
	if err != nil {
		return err
	}

	return nil
}

// IsValidCPUSet checks value is a valid CPU set.
func IsValidCPUSet(value string) error {
	// Validate the CPU set syntax.
	match, _ := regexp.MatchString(`^(?:[0-9]+(?:[,-][0-9]+)?)(?:,[0-9]+(?:[,-][0-9]+)*)?$`, value)
	if !match {
		return errors.New("Invalid CPU limit syntax")
	}

	// Validate single values.
	cpu, err := strconv.ParseInt(value, 10, 64)
	if err == nil {
		if cpu < 1 {
			return fmt.Errorf("Invalid cpuset value: %s", value)
		}

		return nil
	}

	// Handle complex values.
	cpus := make(map[int64]int)
	chunks := strings.Split(value, ",")

	for _, chunk := range chunks {
		if strings.Contains(chunk, "-") {
			// Range
			fields := strings.SplitN(chunk, "-", 2)
			if len(fields) != 2 {
				return fmt.Errorf("Invalid cpuset value: %s", value)
			}

			low, err := strconv.ParseInt(fields[0], 10, 64)
			if err != nil {
				return fmt.Errorf("Invalid cpuset value: %s", value)
			}

			high, err := strconv.ParseInt(fields[1], 10, 64)
			if err != nil {
				return fmt.Errorf("Invalid cpuset value: %s", value)
			}

			for i := low; i <= high; i++ {
				cpus[i]++
			}
		} else {
			// Simple entry
			nr, err := strconv.ParseInt(chunk, 10, 64)
			if err != nil {
				return fmt.Errorf("Invalid cpuset value: %s", value)
			}

			cpus[nr]++
		}
	}

	for i := range cpus {
		// The CPU was specified more than once, e.g. 1-3,3.
		if cpus[i] > 1 {
			return errors.New("Cannot define CPU multiple times")
		}
	}

	return nil
}

// IsShorterThan checks whether a string is shorter than a specific length.
func IsShorterThan(length int) func(value string) error {
	return func(value string) error {
		if len(value) > length {
			return fmt.Errorf("Value is too long. Must be within %d characters", length)
		}

		return nil
	}
}

// IsMinimumDuration validates whether a value is a duration longer than a specific minimum.
func IsMinimumDuration(minimum time.Duration) func(value string) error {
	return func(value string) error {
		duration, err := time.ParseDuration(value)
		if err != nil {
			return errors.New("Invalid duration")
		}

		if duration < minimum {
			return fmt.Errorf("Duration must be greater than %s", minimum)
		}

		return nil
	}
}
