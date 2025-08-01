package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/spf13/cobra"

	incus "github.com/lxc/incus/v6/client"
	cli "github.com/lxc/incus/v6/internal/cmd"
	"github.com/lxc/incus/v6/internal/i18n"
	"github.com/lxc/incus/v6/internal/instance"
	"github.com/lxc/incus/v6/shared/api"
	"github.com/lxc/incus/v6/shared/units"
	"github.com/lxc/incus/v6/shared/util"
)

type column struct {
	Name           string
	Data           columnData
	NeedsState     bool
	NeedsSnapshots bool
}

type columnData func(api.InstanceFull) string

type cmdList struct {
	global *cmdGlobal

	flagColumns     string
	flagFast        bool
	flagFormat      string
	flagAllProjects bool

	shorthandFilters map[string]func(*api.Instance, *api.InstanceState, string) bool
}

// Command returns a cobra.Command for use with (*cobra.Command).AddCommand.
func (c *cmdList) Command() *cobra.Command {
	cmd := &cobra.Command{}
	cmd.Use = usage("list", i18n.G("[<remote>:] [<filter>...]"))
	cmd.Aliases = []string{"ls"}
	cmd.Short = i18n.G("List instances")
	cmd.Long = cli.FormatSection(i18n.G("Description"), i18n.G(
		`List instances

Default column layout: ns46tS
Fast column layout: nsacPt

A single keyword like "web" which will list any instance with a name starting by "web".
A regular expression on the instance name. (e.g. .*web.*01$).
A key/value pair referring to a configuration item. For those, the
namespace can be abbreviated to the smallest unambiguous identifier.
A key/value pair where the key is a shorthand. Multiple values must be delimited by ','. Available shorthands:
  - type={instance type}
  - status={instance current lifecycle status}
  - architecture={instance architecture}
  - location={location name}
  - ipv4={ip or CIDR}
  - ipv6={ip or CIDR}

Examples:
  - "user.blah=abc" will list all instances with the "blah" user property set to "abc".
  - "u.blah=abc" will do the same
  - "security.privileged=true" will list all privileged instances
  - "s.privileged=true" will do the same
  - "type=container" will list all container instances
  - "type=container status=running" will list all running container instances

A regular expression matching a configuration item or its value. (e.g. volatile.eth0.hwaddr=10:66:6a:.*).

When multiple filters are passed, they are added one on top of the other,
selecting instances which satisfy them all.

== Columns ==
The -c option takes a comma separated list of arguments that control
which instance attributes to output when displaying in table or csv
format.

Column arguments are either pre-defined shorthand chars (see below),
or (extended) config keys.

Commas between consecutive shorthand chars are optional.

Pre-defined column shorthand chars:
  4 - IPv4 address
  6 - IPv6 address
  a - Architecture
  b - Storage pool
  c - Creation date
  d - Description
  D - disk usage
  e - Project name
  l - Last used date
  m - Memory usage
  M - Memory usage (%)
  n - Name
  N - Number of Processes
  p - PID of the instance's init process
  P - Profiles
  s - State
  S - Number of snapshots
  t - Type (persistent or ephemeral)
  u - CPU usage (in seconds)
  U - Started date
  L - Location of the instance (e.g. its cluster member)
  f - Base Image Fingerprint (short)
  F - Base Image Fingerprint (long)

Custom columns are defined with "[config:|devices:]key[:name][:maxWidth]":
  KEY: The (extended) config or devices key to display. If [config:|devices:] is omitted then it defaults to config key.
  NAME: Name to display in the column header.
  Defaults to the key if not specified or empty.

  MAXWIDTH: Max width of the column (longer results are truncated).
  Defaults to -1 (unlimited). Use 0 to limit to the column header size.`))

	cmd.Example = cli.FormatSection("", i18n.G(
		`incus list -c nFs46,volatile.eth0.hwaddr:MAC,config:image.os,devices:eth0.parent:ETHP
  Show instances using the "NAME", "BASE IMAGE", "STATE", "IPV4", "IPV6" and "MAC" columns.
  "BASE IMAGE", "MAC" and "IMAGE OS" are custom columns generated from instance configuration keys.
  "ETHP" is a custom column generated from a device key.

incus list -c ns,user.comment:comment
  List instances with their running state and user comment.`))

	cmd.RunE = c.Run
	cmd.Flags().StringVarP(&c.flagColumns, "columns", "c", defaultColumns, i18n.G("Columns")+"``")
	cmd.Flags().StringVarP(&c.flagFormat, "format", "f", c.global.defaultListFormat(), i18n.G(`Format (csv|json|table|yaml|compact|markdown), use suffix ",noheader" to disable headers and ",header" to enable it if missing, e.g. csv,header`)+"``")
	cmd.Flags().BoolVar(&c.flagFast, "fast", false, i18n.G("Fast mode (same as --columns=nsacPt)"))
	cmd.Flags().BoolVar(&c.flagAllProjects, "all-projects", false, i18n.G("Display instances from all projects"))

	cmd.PreRunE = func(cmd *cobra.Command, _ []string) error {
		return cli.ValidateFlagFormatForListOutput(cmd.Flag("format").Value.String())
	}

	cmd.ValidArgsFunction = func(_ *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		if len(args) == 0 {
			return c.global.cmpRemotes(toComplete, false)
		}

		return nil, cobra.ShellCompDirectiveNoFileComp
	}

	return cmd
}

const (
	defaultColumns            = "ns46tSL"
	defaultColumnsAllProjects = "ens46tSL"
	configColumnType          = "config"
	deviceColumnType          = "devices"
)

func (c *cmdList) shouldShow(filters []string, inst *api.Instance, state *api.InstanceState) bool {
	c.mapShorthandFilters()

	for _, filter := range filters {
		membs := strings.SplitN(filter, "=", 2)

		key := membs[0]
		var value string
		if len(membs) < 2 {
			value = ""
		} else {
			value = membs[1]
		}

		if c.evaluateShorthandFilter(key, value, inst, state) {
			continue
		}

		return false
	}

	return true
}

func (c *cmdList) evaluateShorthandFilter(key string, value string, inst *api.Instance, state *api.InstanceState) bool {
	const shorthandValueDelimiter = ","
	shorthandFilterFunction, isShorthandFilter := c.shorthandFilters[strings.ToLower(key)]

	if !isShorthandFilter {
		return false
	}

	if !strings.Contains(value, shorthandValueDelimiter) {
		return shorthandFilterFunction(inst, state, value)
	}

	matched := false
	for _, curValue := range strings.Split(value, shorthandValueDelimiter) {
		if shorthandFilterFunction(inst, state, curValue) {
			matched = true
		}
	}

	return matched
}

func (c *cmdList) listInstances(d incus.InstanceServer, instances []api.Instance, filters []string, columns []column) error {
	threads := min(len(instances), 10)

	// Shortcut when needing state and snapshot info.
	hasSnapshots := false
	hasState := false
	for _, column := range columns {
		if column.NeedsSnapshots {
			hasSnapshots = true
		}

		if column.NeedsState {
			hasState = true
		}
	}

	if hasSnapshots && hasState {
		cInfo := []api.InstanceFull{}
		cInfoLock := sync.Mutex{}
		cInfoQueue := make(chan string, threads)
		cInfoWg := sync.WaitGroup{}

		for range threads {
			cInfoWg.Add(1)
			go func() {
				for {
					cName, more := <-cInfoQueue
					if !more {
						break
					}

					state, _, err := d.GetInstanceFull(cName)
					if err != nil {
						continue
					}

					cInfoLock.Lock()
					cInfo = append(cInfo, *state)
					cInfoLock.Unlock()
				}

				cInfoWg.Done()
			}()
		}

		for _, info := range instances {
			cInfoQueue <- info.Name
		}

		close(cInfoQueue)
		cInfoWg.Wait()

		return c.showInstances(cInfo, filters, columns)
	}

	cStates := map[string]*api.InstanceState{}
	cStatesLock := sync.Mutex{}
	cStatesQueue := make(chan string, threads)
	cStatesWg := sync.WaitGroup{}

	cSnapshots := map[string][]api.InstanceSnapshot{}
	cSnapshotsLock := sync.Mutex{}
	cSnapshotsQueue := make(chan string, threads)
	cSnapshotsWg := sync.WaitGroup{}

	for range threads {
		cStatesWg.Add(1)
		go func() {
			for {
				cName, more := <-cStatesQueue
				if !more {
					break
				}

				state, _, err := d.GetInstanceState(cName)
				if err != nil {
					continue
				}

				cStatesLock.Lock()
				cStates[cName] = state
				cStatesLock.Unlock()
			}

			cStatesWg.Done()
		}()

		cSnapshotsWg.Add(1)
		go func() {
			for {
				cName, more := <-cSnapshotsQueue
				if !more {
					break
				}

				snaps, err := d.GetInstanceSnapshots(cName)
				if err != nil {
					continue
				}

				cSnapshotsLock.Lock()
				cSnapshots[cName] = snaps
				cSnapshotsLock.Unlock()
			}

			cSnapshotsWg.Done()
		}()
	}

	for _, inst := range instances {
		for _, column := range columns {
			if column.NeedsState && inst.IsActive() {
				cStatesLock.Lock()
				_, ok := cStates[inst.Name]
				cStatesLock.Unlock()
				if ok {
					continue
				}

				cStatesLock.Lock()
				cStates[inst.Name] = nil
				cStatesLock.Unlock()

				cStatesQueue <- inst.Name
			}

			if column.NeedsSnapshots {
				cSnapshotsLock.Lock()
				_, ok := cSnapshots[inst.Name]
				cSnapshotsLock.Unlock()
				if ok {
					continue
				}

				cSnapshotsLock.Lock()
				cSnapshots[inst.Name] = nil
				cSnapshotsLock.Unlock()

				cSnapshotsQueue <- inst.Name
			}
		}
	}

	close(cStatesQueue)
	close(cSnapshotsQueue)
	cStatesWg.Wait()
	cSnapshotsWg.Wait()

	// Convert to Instance
	data := make([]api.InstanceFull, len(instances))
	for i := range instances {
		data[i].Instance = instances[i]
		data[i].State = cStates[instances[i].Name]
		data[i].Snapshots = cSnapshots[instances[i].Name]
	}

	return c.showInstances(data, filters, columns)
}

func (c *cmdList) showInstances(instances []api.InstanceFull, filters []string, columns []column) error {
	// Generate the table data
	data := [][]string{}
	instancesFiltered := []api.InstanceFull{}

	for _, inst := range instances {
		if !c.shouldShow(filters, &inst.Instance, inst.State) {
			continue
		}

		instancesFiltered = append(instancesFiltered, inst)

		col := []string{}
		for _, column := range columns {
			col = append(col, column.Data(inst))
		}

		data = append(data, col)
	}

	sort.Sort(cli.SortColumnsNaturally(data))

	headers := []string{}
	for _, column := range columns {
		headers = append(headers, column.Name)
	}

	return cli.RenderTable(os.Stdout, c.flagFormat, headers, data, instancesFiltered)
}

// Run runs the actual command logic.
func (c *cmdList) Run(cmd *cobra.Command, args []string) error {
	conf := c.global.conf

	// Quick checks.
	exit, err := c.global.checkArgs(cmd, args, 0, -1)
	if exit {
		return err
	}

	if c.global.flagProject != "" && c.flagAllProjects {
		return errors.New(i18n.G("Can't specify --project with --all-projects"))
	}

	// Parse the remote
	var remote string
	var name string
	var filters []string

	if len(args) != 0 {
		filters = args
		if strings.Contains(args[0], ":") && !strings.Contains(args[0], "=") {
			var err error
			remote, name, err = conf.ParseRemote(args[0])
			if err != nil {
				return err
			}

			filters = args[1:]
		} else if !strings.Contains(args[0], "=") {
			remote = conf.DefaultRemote
			name = args[0]
			filters = args[1:]
		}
	}

	if name != "" {
		filters = append(filters, name)
	}

	if remote == "" {
		remote = conf.DefaultRemote
	}

	// Connect to the daemon.
	d, err := conf.GetInstanceServer(remote)
	if err != nil {
		return err
	}

	// Get the list of columns
	columns, needsData, err := c.parseColumns(d.IsClustered())
	if err != nil {
		return err
	}

	if needsData && d.HasExtension("container_full") {
		// Using the GetInstancesFull shortcut
		var instances []api.InstanceFull

		serverFilters, clientFilters := getServerSupportedFilters(filters, []string{"ipv4", "ipv6"}, true)
		serverFilters = prepareInstanceServerFilters(serverFilters, api.InstanceFull{})

		if c.flagAllProjects {
			instances, err = d.GetInstancesFullAllProjectsWithFilter(api.InstanceTypeAny, serverFilters)
		} else {
			instances, err = d.GetInstancesFullWithFilter(api.InstanceTypeAny, serverFilters)
		}

		if err != nil {
			return err
		}

		return c.showInstances(instances, clientFilters, columns)
	}

	// Get the list of instances
	var instances []api.Instance
	serverFilters, clientFilters := getServerSupportedFilters(filters, []string{"ipv4", "ipv6"}, true)
	serverFilters = prepareInstanceServerFilters(serverFilters, api.Instance{})

	if c.flagAllProjects {
		instances, err = d.GetInstancesAllProjectsWithFilter(api.InstanceTypeAny, serverFilters)
	} else {
		instances, err = d.GetInstancesWithFilter(api.InstanceTypeAny, serverFilters)
	}

	if err != nil {
		return err
	}

	// Fetch any remaining data and render the table
	return c.listInstances(d, instances, clientFilters, columns)
}

func (c *cmdList) parseColumns(clustered bool) ([]column, bool, error) {
	columnsShorthandMap := map[rune]column{
		'4': {i18n.G("IPV4"), c.ip4ColumnData, true, false},
		'6': {i18n.G("IPV6"), c.ip6ColumnData, true, false},
		'a': {i18n.G("ARCHITECTURE"), c.architectureColumnData, false, false},
		'b': {i18n.G("STORAGE POOL"), c.storagePoolColumnData, false, false},
		'c': {i18n.G("CREATED AT"), c.createdColumnData, false, false},
		'd': {i18n.G("DESCRIPTION"), c.descriptionColumnData, false, false},
		'D': {i18n.G("DISK USAGE"), c.diskUsageColumnData, true, false},
		'e': {i18n.G("PROJECT"), c.projectColumnData, false, false},
		'f': {i18n.G("BASE IMAGE"), c.baseImageColumnData, false, false},
		'F': {i18n.G("BASE IMAGE"), c.baseImageFullColumnData, false, false},
		'l': {i18n.G("LAST USED AT"), c.lastUsedColumnData, false, false},
		'm': {i18n.G("MEMORY USAGE"), c.memoryUsageColumnData, true, false},
		'M': {i18n.G("MEMORY USAGE%"), c.memoryUsagePercentColumnData, true, false},
		'n': {i18n.G("NAME"), c.nameColumnData, false, false},
		'N': {i18n.G("PROCESSES"), c.numberOfProcessesColumnData, true, false},
		'p': {i18n.G("PID"), c.pidColumnData, true, false},
		'P': {i18n.G("PROFILES"), c.profilesColumnData, false, false},
		'S': {i18n.G("SNAPSHOTS"), c.numberSnapshotsColumnData, false, true},
		's': {i18n.G("STATE"), c.statusColumnData, false, false},
		't': {i18n.G("TYPE"), c.typeColumnData, false, false},
		'u': {i18n.G("CPU USAGE"), c.cpuUsageSecondsColumnData, true, false},
		'U': {i18n.G("STARTED AT"), c.startedColumnData, true, false},
	}

	// Add project column if --all-projects flag specified and
	// no one of --fast or --c was passed
	if c.flagAllProjects {
		if c.flagColumns == defaultColumns {
			c.flagColumns = defaultColumnsAllProjects
		}
	}

	if c.flagFast {
		if c.flagColumns != defaultColumns && c.flagColumns != defaultColumnsAllProjects {
			// --columns was specified too
			return nil, false, errors.New(i18n.G("Can't specify --fast with --columns"))
		}

		if c.flagColumns == defaultColumnsAllProjects {
			c.flagColumns = "ensacPt"
		} else {
			c.flagColumns = "nsacPt"
		}
	}

	if clustered {
		columnsShorthandMap['L'] = column{
			i18n.G("LOCATION"), c.locationColumnData, false, false,
		}
	}

	columnList := strings.Split(c.flagColumns, ",")

	columns := []column{}
	needsData := false
	for _, columnEntry := range columnList {
		if columnEntry == "" {
			return nil, false, fmt.Errorf(i18n.G("Empty column entry (redundant, leading or trailing command) in '%s'"), c.flagColumns)
		}

		// Config keys always contain a period, parse anything without a
		// period as a series of shorthand runes.
		if !strings.Contains(columnEntry, ".") {
			if !clustered {
				if columnEntry != defaultColumns && columnEntry != defaultColumnsAllProjects {
					if strings.ContainsAny(columnEntry, "L") {
						return nil, false, errors.New(i18n.G("Can't specify column L when not clustered"))
					}
				}
				columnEntry = strings.ReplaceAll(columnEntry, "L", "")
			}

			for _, columnRune := range columnEntry {
				column, ok := columnsShorthandMap[columnRune]
				if !ok {
					return nil, false, fmt.Errorf(i18n.G("Unknown column shorthand char '%c' in '%s'"), columnRune, columnEntry)
				}

				columns = append(columns, column)

				if column.NeedsState || column.NeedsSnapshots {
					needsData = true
				}
			}
		} else {
			cc := strings.Split(columnEntry, ":")
			colType := configColumnType
			if (cc[0] == configColumnType || cc[0] == deviceColumnType) && len(cc) > 1 {
				colType = cc[0]
				cc = slices.Delete(cc, 0, 1)
			}

			if len(cc) > 3 {
				return nil, false, fmt.Errorf(i18n.G("Invalid config key column format (too many fields): '%s'"), columnEntry)
			}

			k := cc[0]
			if colType == configColumnType {
				_, err := instance.ConfigKeyChecker(k, api.InstanceTypeAny)
				if err != nil {
					return nil, false, fmt.Errorf(i18n.G("Invalid config key '%s' in '%s'"), k, columnEntry)
				}
			}

			column := column{Name: k}
			if len(cc) > 1 {
				if len(cc[1]) == 0 && len(cc) != 3 {
					return nil, false, fmt.Errorf(i18n.G("Invalid name in '%s', empty string is only allowed when defining maxWidth"), columnEntry)
				}

				column.Name = cc[1]
			}

			maxWidth := -1
			if len(cc) > 2 {
				temp, err := strconv.ParseInt(cc[2], 10, 64)
				if err != nil {
					return nil, false, fmt.Errorf(i18n.G("Invalid max width (must be an integer) '%s' in '%s'"), cc[2], columnEntry)
				}

				if temp < -1 {
					return nil, false, fmt.Errorf(i18n.G("Invalid max width (must -1, 0 or a positive integer) '%s' in '%s'"), cc[2], columnEntry)
				}

				if temp == 0 {
					maxWidth = len(column.Name)
				} else {
					maxWidth = int(temp)
				}
			}
			if colType == configColumnType {
				column.Data = func(cInfo api.InstanceFull) string {
					v, ok := cInfo.Config[k]
					if !ok {
						v = cInfo.ExpandedConfig[k]
					}

					// Truncate the data according to the max width.  A negative max width
					// indicates there is no effective limit.
					if maxWidth > 0 && len(v) > maxWidth {
						return v[:maxWidth]
					}

					return v
				}
			}
			if colType == deviceColumnType {
				column.Data = func(cInfo api.InstanceFull) string {
					d := strings.SplitN(k, ".", 2)
					if len(d) == 1 || len(d) > 2 {
						return ""
					}

					v, ok := cInfo.Devices[d[0]][d[1]]
					if !ok {
						v = cInfo.ExpandedDevices[d[0]][d[1]]
					}

					// Truncate the data according to the max width.  A negative max width
					// indicates there is no effective limit.
					if maxWidth > 0 && len(v) > maxWidth {
						return v[:maxWidth]
					}

					return v
				}
			}
			columns = append(columns, column)

			if column.NeedsState || column.NeedsSnapshots {
				needsData = true
			}
		}
	}

	return columns, needsData, nil
}

func (c *cmdList) getBaseImage(cInfo api.InstanceFull, long bool) string {
	v, ok := cInfo.Config["volatile.base_image"]
	if !ok {
		return ""
	}

	if !long && len(v) >= 12 {
		v = v[:12]
	}

	return v
}

func (c *cmdList) baseImageColumnData(cInfo api.InstanceFull) string {
	return c.getBaseImage(cInfo, false)
}

func (c *cmdList) baseImageFullColumnData(cInfo api.InstanceFull) string {
	return c.getBaseImage(cInfo, true)
}

func (c *cmdList) nameColumnData(cInfo api.InstanceFull) string {
	return cInfo.Name
}

func (c *cmdList) descriptionColumnData(cInfo api.InstanceFull) string {
	return cInfo.Description
}

func (c *cmdList) statusColumnData(cInfo api.InstanceFull) string {
	return strings.ToUpper(cInfo.Status)
}

func (c *cmdList) ip4ColumnData(cInfo api.InstanceFull) string {
	if cInfo.IsActive() && cInfo.State != nil && cInfo.State.Network != nil {
		ipv4s := []string{}
		for netName, network := range cInfo.State.Network {
			if network.Type == "loopback" {
				continue
			}

			for _, addr := range network.Addresses {
				if slices.Contains([]string{"link", "local"}, addr.Scope) {
					continue
				}

				if addr.Family == "inet" {
					ipv4s = append(ipv4s, fmt.Sprintf("%s (%s)", addr.Address, netName))
				}
			}
		}

		sort.Sort(sort.Reverse(sort.StringSlice(ipv4s)))
		return strings.Join(ipv4s, "\n")
	}

	return ""
}

func (c *cmdList) ip6ColumnData(cInfo api.InstanceFull) string {
	if cInfo.IsActive() && cInfo.State != nil && cInfo.State.Network != nil {
		ipv6s := []string{}
		for netName, network := range cInfo.State.Network {
			if network.Type == "loopback" {
				continue
			}

			for _, addr := range network.Addresses {
				if slices.Contains([]string{"link", "local"}, addr.Scope) {
					continue
				}

				if addr.Family == "inet6" {
					ipv6s = append(ipv6s, fmt.Sprintf("%s (%s)", addr.Address, netName))
				}
			}
		}

		sort.Sort(sort.Reverse(sort.StringSlice(ipv6s)))
		return strings.Join(ipv6s, "\n")
	}

	return ""
}

func (c *cmdList) projectColumnData(cInfo api.InstanceFull) string {
	return cInfo.Project
}

func (c *cmdList) memoryUsageColumnData(cInfo api.InstanceFull) string {
	if cInfo.IsActive() && cInfo.State != nil && cInfo.State.Memory.Usage > 0 {
		return units.GetByteSizeStringIEC(cInfo.State.Memory.Usage, 2)
	}

	return ""
}

func (c *cmdList) memoryUsagePercentColumnData(cInfo api.InstanceFull) string {
	if cInfo.IsActive() && cInfo.State != nil && cInfo.State.Memory.Usage > 0 {
		if cInfo.ExpandedConfig["limits.memory"] != "" {
			memorylimit := cInfo.ExpandedConfig["limits.memory"]

			if strings.Contains(memorylimit, "%") {
				return ""
			}

			val, err := units.ParseByteSizeString(cInfo.ExpandedConfig["limits.memory"])
			if err == nil && val > 0 {
				return fmt.Sprintf("%.1f%%", (float64(cInfo.State.Memory.Usage)/float64(val))*float64(100))
			}
		}
	}

	return ""
}

func (c *cmdList) cpuUsageSecondsColumnData(cInfo api.InstanceFull) string {
	if cInfo.IsActive() && cInfo.State != nil && cInfo.State.CPU.Usage > 0 {
		return fmt.Sprintf("%ds", cInfo.State.CPU.Usage/1000000000)
	}

	return ""
}

func (c *cmdList) diskUsageColumnData(cInfo api.InstanceFull) string {
	rootDisk, _, _ := instance.GetRootDiskDevice(cInfo.ExpandedDevices)

	if cInfo.State != nil && cInfo.State.Disk != nil && cInfo.State.Disk[rootDisk].Usage > 0 {
		return units.GetByteSizeStringIEC(cInfo.State.Disk[rootDisk].Usage, 2)
	}

	return ""
}

func (c *cmdList) typeColumnData(cInfo api.InstanceFull) string {
	ret := strings.ToUpper(cInfo.Type)

	if ret == "" {
		ret = "CONTAINER"
	}

	if util.IsTrue(cInfo.ExpandedConfig["volatile.container.oci"]) {
		ret = fmt.Sprintf("%s (%s)", ret, i18n.G("APP"))
	}

	if cInfo.Ephemeral {
		ret = fmt.Sprintf("%s (%s)", ret, i18n.G("EPHEMERAL"))
	}

	return ret
}

func (c *cmdList) numberSnapshotsColumnData(cInfo api.InstanceFull) string {
	if cInfo.Snapshots != nil {
		return fmt.Sprintf("%d", len(cInfo.Snapshots))
	}

	return "0"
}

func (c *cmdList) pidColumnData(cInfo api.InstanceFull) string {
	if cInfo.IsActive() && cInfo.State != nil {
		return fmt.Sprintf("%d", cInfo.State.Pid)
	}

	return ""
}

func (c *cmdList) architectureColumnData(cInfo api.InstanceFull) string {
	return cInfo.Architecture
}

func (c *cmdList) storagePoolColumnData(cInfo api.InstanceFull) string {
	for _, v := range cInfo.ExpandedDevices {
		if v["type"] == "disk" && v["path"] == "/" {
			return v["pool"]
		}
	}

	return ""
}

func (c *cmdList) profilesColumnData(cInfo api.InstanceFull) string {
	return strings.Join(cInfo.Profiles, "\n")
}

func (c *cmdList) createdColumnData(cInfo api.InstanceFull) string {
	if !cInfo.CreatedAt.IsZero() {
		return cInfo.CreatedAt.Local().Format(dateLayout)
	}

	return ""
}

func (c *cmdList) startedColumnData(cInfo api.InstanceFull) string {
	if cInfo.State != nil && !cInfo.State.StartedAt.IsZero() {
		return cInfo.State.StartedAt.Local().Format(dateLayout)
	}

	return ""
}

func (c *cmdList) lastUsedColumnData(cInfo api.InstanceFull) string {
	if !cInfo.LastUsedAt.IsZero() {
		return cInfo.LastUsedAt.Local().Format(dateLayout)
	}

	return ""
}

func (c *cmdList) numberOfProcessesColumnData(cInfo api.InstanceFull) string {
	if cInfo.IsActive() && cInfo.State != nil {
		return fmt.Sprintf("%d", cInfo.State.Processes)
	}

	return ""
}

func (c *cmdList) locationColumnData(cInfo api.InstanceFull) string {
	return cInfo.Location
}

func (c *cmdList) matchByNet(cState *api.InstanceState, query string, family string) bool {
	// Skip if no state.
	if cState == nil {
		return false
	}

	// Skip if no network data.
	if cState.Network == nil {
		return false
	}

	// Consider the filter as a CIDR.
	_, subnet, _ := net.ParseCIDR(query)

	// Go through interfaces.
	for _, network := range cState.Network {
		for _, addr := range network.Addresses {
			if family == "ipv6" && addr.Family != "inet6" {
				continue
			}

			if family == "ipv4" && addr.Family != "inet" {
				continue
			}

			if addr.Address == query {
				return true
			}

			if subnet != nil {
				ipAddr := net.ParseIP(addr.Address)
				if ipAddr != nil && subnet.Contains(ipAddr) {
					return true
				}
			}
		}
	}

	return false
}

func (c *cmdList) matchByIPV6(_ *api.Instance, cState *api.InstanceState, query string) bool {
	return c.matchByNet(cState, query, "ipv6")
}

func (c *cmdList) matchByIPV4(_ *api.Instance, cState *api.InstanceState, query string) bool {
	return c.matchByNet(cState, query, "ipv4")
}

func (c *cmdList) mapShorthandFilters() {
	c.shorthandFilters = map[string]func(*api.Instance, *api.InstanceState, string) bool{
		"ipv4": c.matchByIPV4,
		"ipv6": c.matchByIPV6,
	}
}

// prepareInstanceServerFilters processes and formats filter criteria
// for instances, ensuring they are in a format that the server can interpret.
func prepareInstanceServerFilters(filters []string, i any) []string {
	formatedFilters := []string{}

	for _, filter := range filters {
		membs := strings.SplitN(filter, "=", 2)
		key := membs[0]

		if len(membs) == 1 {
			regexpValue := key
			if !strings.Contains(key, "^") && !strings.Contains(key, "$") {
				regexpValue = "^" + regexpValue + "$"
			}

			filter = fmt.Sprintf("name=(%s|^%s.*)", regexpValue, key)
		} else {
			firstPart := key
			if strings.Contains(key, ".") {
				firstPart = strings.Split(key, ".")[0]
			}

			if !structHasField(reflect.TypeOf(i), firstPart) {
				filter = fmt.Sprintf("expanded_config.%s", filter)
			}

			if key == "state" {
				filter = fmt.Sprintf("status=%s", membs[1])
			}
		}

		formatedFilters = append(formatedFilters, filter)
	}

	return formatedFilters
}
