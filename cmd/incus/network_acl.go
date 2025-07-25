package main

import (
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"reflect"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v2"

	cli "github.com/lxc/incus/v6/internal/cmd"
	"github.com/lxc/incus/v6/internal/i18n"
	"github.com/lxc/incus/v6/shared/api"
	"github.com/lxc/incus/v6/shared/termios"
)

type cmdNetworkACL struct {
	global *cmdGlobal
}

// Command returns a cobra.Command for use with (*cobra.Command).AddCommand.
func (c *cmdNetworkACL) Command() *cobra.Command {
	cmd := &cobra.Command{}
	cmd.Use = usage("acl")
	cmd.Short = i18n.G("Manage network ACLs")
	cmd.Long = cli.FormatSection(i18n.G("Description"), i18n.G("Manage network ACLs"))

	// List.
	networkACLListCmd := cmdNetworkACLList{global: c.global, networkACL: c}
	cmd.AddCommand(networkACLListCmd.Command())

	// Show.
	networkACLShowCmd := cmdNetworkACLShow{global: c.global, networkACL: c}
	cmd.AddCommand(networkACLShowCmd.Command())

	// Show log.
	networkACLShowLogCmd := cmdNetworkACLShowLog{global: c.global, networkACL: c}
	cmd.AddCommand(networkACLShowLogCmd.Command())

	// Get.
	networkACLGetCmd := cmdNetworkACLGet{global: c.global, networkACL: c}
	cmd.AddCommand(networkACLGetCmd.Command())

	// Create.
	networkACLCreateCmd := cmdNetworkACLCreate{global: c.global, networkACL: c}
	cmd.AddCommand(networkACLCreateCmd.Command())

	// Set.
	networkACLSetCmd := cmdNetworkACLSet{global: c.global, networkACL: c}
	cmd.AddCommand(networkACLSetCmd.Command())

	// Unset.
	networkACLUnsetCmd := cmdNetworkACLUnset{global: c.global, networkACL: c, networkACLSet: &networkACLSetCmd}
	cmd.AddCommand(networkACLUnsetCmd.Command())

	// Edit.
	networkACLEditCmd := cmdNetworkACLEdit{global: c.global, networkACL: c}
	cmd.AddCommand(networkACLEditCmd.Command())

	// Rename.
	networkACLRenameCmd := cmdNetworkACLRename{global: c.global, networkACL: c}
	cmd.AddCommand(networkACLRenameCmd.Command())

	// Delete.
	networkACLDeleteCmd := cmdNetworkACLDelete{global: c.global, networkACL: c}
	cmd.AddCommand(networkACLDeleteCmd.Command())

	// Rule.
	networkACLRuleCmd := cmdNetworkACLRule{global: c.global, networkACL: c}
	cmd.AddCommand(networkACLRuleCmd.Command())

	// Workaround for subcommand usage errors. See: https://github.com/spf13/cobra/issues/706
	cmd.Args = cobra.NoArgs
	cmd.Run = func(cmd *cobra.Command, _ []string) { _ = cmd.Usage() }
	return cmd
}

// List.
type cmdNetworkACLList struct {
	global     *cmdGlobal
	networkACL *cmdNetworkACL

	flagFormat      string
	flagAllProjects bool
}

// Command returns a cobra.Command for use with (*cobra.Command).AddCommand.
func (c *cmdNetworkACLList) Command() *cobra.Command {
	cmd := &cobra.Command{}
	cmd.Use = usage("list", i18n.G("[<remote>:]"))
	cmd.Aliases = []string{"ls"}
	cmd.Short = i18n.G("List available network ACLS")
	cmd.Long = cli.FormatSection(i18n.G("Description"), i18n.G("List available network ACL"))

	cmd.RunE = c.Run
	cmd.Flags().StringVarP(&c.flagFormat, "format", "f", c.global.defaultListFormat(), i18n.G(`Format (csv|json|table|yaml|compact|markdown), use suffix ",noheader" to disable headers and ",header" to enable it if missing, e.g. csv,header`)+"``")
	cmd.Flags().BoolVar(&c.flagAllProjects, "all-projects", false, i18n.G("List network ACLs across all projects"))

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

// Run runs the actual command logic.
func (c *cmdNetworkACLList) Run(cmd *cobra.Command, args []string) error {
	// Quick checks.
	exit, err := c.global.checkArgs(cmd, args, 0, 1)
	if exit {
		return err
	}

	// Parse remote.
	remote := ""
	if len(args) > 0 {
		remote = args[0]
	}

	resources, err := c.global.parseServers(remote)
	if err != nil {
		return err
	}

	resource := resources[0]

	// List the networks.
	if resource.name != "" {
		return errors.New(i18n.G("Filtering isn't supported yet"))
	}

	var acls []api.NetworkACL
	if c.flagAllProjects {
		acls, err = resource.server.GetNetworkACLsAllProjects()
		if err != nil {
			return err
		}
	} else {
		acls, err = resource.server.GetNetworkACLs()
		if err != nil {
			return err
		}
	}

	data := [][]string{}
	for _, acl := range acls {
		strUsedBy := fmt.Sprintf("%d", len(acl.UsedBy))
		details := []string{
			acl.Name,
			acl.Description,
			strUsedBy,
		}

		if c.flagAllProjects {
			details = append([]string{acl.Project}, details...)
		}

		data = append(data, details)
	}

	sort.Sort(cli.SortColumnsNaturally(data))

	header := []string{
		i18n.G("NAME"),
		i18n.G("DESCRIPTION"),
		i18n.G("USED BY"),
	}

	if c.flagAllProjects {
		header = append([]string{i18n.G("PROJECT")}, header...)
	}

	return cli.RenderTable(os.Stdout, c.flagFormat, header, data, acls)
}

// Show.
type cmdNetworkACLShow struct {
	global     *cmdGlobal
	networkACL *cmdNetworkACL
}

// Command returns a cobra.Command for use with (*cobra.Command).AddCommand.
func (c *cmdNetworkACLShow) Command() *cobra.Command {
	cmd := &cobra.Command{}
	cmd.Use = usage("show", i18n.G("[<remote>:]<ACL>"))
	cmd.Short = i18n.G("Show network ACL configurations")
	cmd.Long = cli.FormatSection(i18n.G("Description"), i18n.G("Show network ACL configurations"))
	cmd.RunE = c.Run

	cmd.ValidArgsFunction = func(_ *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		if len(args) == 0 {
			return c.global.cmpNetworkACLs(toComplete)
		}

		return nil, cobra.ShellCompDirectiveNoFileComp
	}

	return cmd
}

// Run runs the actual command logic.
func (c *cmdNetworkACLShow) Run(cmd *cobra.Command, args []string) error {
	// Quick checks.
	exit, err := c.global.checkArgs(cmd, args, 1, 1)
	if exit {
		return err
	}

	// Parse remote.
	resources, err := c.global.parseServers(args[0])
	if err != nil {
		return err
	}

	resource := resources[0]

	if resource.name == "" {
		return errors.New(i18n.G("Missing network ACL name"))
	}

	// Show the network ACL config.
	netACL, _, err := resource.server.GetNetworkACL(resource.name)
	if err != nil {
		return err
	}

	sort.Strings(netACL.UsedBy)

	data, err := yaml.Marshal(&netACL)
	if err != nil {
		return err
	}

	fmt.Printf("%s", data)

	return nil
}

// Show log.
type cmdNetworkACLShowLog struct {
	global     *cmdGlobal
	networkACL *cmdNetworkACL
}

// Command returns a cobra.Command for use with (*cobra.Command).AddCommand.
func (c *cmdNetworkACLShowLog) Command() *cobra.Command {
	cmd := &cobra.Command{}
	cmd.Use = usage("show-log", i18n.G("[<remote>:]<ACL>"))
	cmd.Short = i18n.G("Show network ACL log")
	cmd.Long = cli.FormatSection(i18n.G("Description"), i18n.G("Show network ACL log"))
	cmd.RunE = c.Run

	cmd.ValidArgsFunction = func(_ *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		if len(args) == 0 {
			return c.global.cmpNetworkACLs(toComplete)
		}

		return nil, cobra.ShellCompDirectiveNoFileComp
	}

	return cmd
}

// Run runs the actual command logic.
func (c *cmdNetworkACLShowLog) Run(cmd *cobra.Command, args []string) error {
	// Quick checks.
	exit, err := c.global.checkArgs(cmd, args, 1, 1)
	if exit {
		return err
	}

	// Parse remote.
	resources, err := c.global.parseServers(args[0])
	if err != nil {
		return err
	}

	resource := resources[0]
	if resource.name == "" {
		return errors.New(i18n.G("Missing network ACL name"))
	}

	// Get the ACL log.
	log, err := resource.server.GetNetworkACLLogfile(resource.name)
	if err != nil {
		return err
	}

	_, err = io.Copy(os.Stdout, log)
	_ = log.Close()

	return err
}

// Get.
type cmdNetworkACLGet struct {
	global     *cmdGlobal
	networkACL *cmdNetworkACL

	flagIsProperty bool
}

// Command returns a cobra.Command for use with (*cobra.Command).AddCommand.
func (c *cmdNetworkACLGet) Command() *cobra.Command {
	cmd := &cobra.Command{}
	cmd.Use = usage("get", i18n.G("[<remote>:]<ACL> <key>"))
	cmd.Short = i18n.G("Get values for network ACL configuration keys")
	cmd.Long = cli.FormatSection(i18n.G("Description"), i18n.G("Get values for network ACL configuration keys"))

	cmd.Flags().BoolVarP(&c.flagIsProperty, "property", "p", false, i18n.G("Get the key as a network ACL property"))
	cmd.RunE = c.Run

	cmd.ValidArgsFunction = func(_ *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		if len(args) == 0 {
			return c.global.cmpNetworkACLs(toComplete)
		}

		if len(args) == 1 {
			return c.global.cmpNetworkACLConfigs(args[0])
		}

		return nil, cobra.ShellCompDirectiveNoFileComp
	}

	return cmd
}

// Run runs the actual command logic.
func (c *cmdNetworkACLGet) Run(cmd *cobra.Command, args []string) error {
	// Quick checks.
	exit, err := c.global.checkArgs(cmd, args, 2, 2)
	if exit {
		return err
	}

	// Parse remote.
	resources, err := c.global.parseServers(args[0])
	if err != nil {
		return err
	}

	resource := resources[0]

	if resource.name == "" {
		return errors.New(i18n.G("Missing network ACL name"))
	}

	resp, _, err := resource.server.GetNetworkACL(resource.name)
	if err != nil {
		return err
	}

	if c.flagIsProperty {
		w := resp.Writable()
		res, err := getFieldByJSONTag(&w, args[1])
		if err != nil {
			return fmt.Errorf(i18n.G("The property %q does not exist on the network ACL %q: %v"), args[1], resource.name, err)
		}

		fmt.Printf("%v\n", res)
	} else {
		for k, v := range resp.Config {
			if k == args[1] {
				fmt.Printf("%s\n", v)
			}
		}
	}

	return nil
}

// Create.
type cmdNetworkACLCreate struct {
	global     *cmdGlobal
	networkACL *cmdNetworkACL

	flagDescription string
}

// Command returns a cobra.Command for use with (*cobra.Command).AddCommand.
func (c *cmdNetworkACLCreate) Command() *cobra.Command {
	cmd := &cobra.Command{}
	cmd.Use = usage("create", i18n.G("[<remote>:]<ACL> [key=value...]"))
	cmd.Aliases = []string{"add"}
	cmd.Short = i18n.G("Create new network ACLs")
	cmd.Long = cli.FormatSection(i18n.G("Description"), i18n.G("Create new network ACLs"))
	cmd.Example = cli.FormatSection("", i18n.G(`incus network acl create a1

incus network acl create a1 < config.yaml
    Create network acl with configuration from config.yaml`))

	cmd.Flags().StringVar(&c.flagDescription, "description", "", i18n.G("Network ACL description")+"``")

	cmd.RunE = c.Run

	cmd.ValidArgsFunction = func(_ *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		if len(args) == 0 {
			return c.global.cmpNetworkACLs(toComplete)
		}

		return nil, cobra.ShellCompDirectiveNoFileComp
	}

	return cmd
}

// Run runs the actual command logic.
func (c *cmdNetworkACLCreate) Run(cmd *cobra.Command, args []string) error {
	// Quick checks.
	exit, err := c.global.checkArgs(cmd, args, 1, -1)
	if exit {
		return err
	}

	// Parse remote.
	resources, err := c.global.parseServers(args[0])
	if err != nil {
		return err
	}

	resource := resources[0]

	if resource.name == "" {
		return errors.New(i18n.G("Missing network ACL name"))
	}

	// If stdin isn't a terminal, read yaml from it.
	var aclPut api.NetworkACLPut
	if !termios.IsTerminal(getStdinFd()) {
		contents, err := io.ReadAll(os.Stdin)
		if err != nil {
			return err
		}

		err = yaml.UnmarshalStrict(contents, &aclPut)
		if err != nil {
			return err
		}
	}

	// Create the network ACL.
	acl := api.NetworkACLsPost{
		NetworkACLPost: api.NetworkACLPost{
			Name: resource.name,
		},
		NetworkACLPut: aclPut,
	}

	if c.flagDescription != "" {
		acl.Description = c.flagDescription
	}

	if acl.Config == nil {
		acl.Config = map[string]string{}
	}

	for i := 1; i < len(args); i++ {
		entry := strings.SplitN(args[i], "=", 2)
		if len(entry) < 2 {
			return fmt.Errorf(i18n.G("Bad key/value pair: %s"), args[i])
		}

		acl.Config[entry[0]] = entry[1]
	}

	err = resource.server.CreateNetworkACL(acl)
	if err != nil {
		return err
	}

	if !c.global.flagQuiet {
		fmt.Printf(i18n.G("Network ACL %s created")+"\n", resource.name)
	}

	return nil
}

// Set.
type cmdNetworkACLSet struct {
	global     *cmdGlobal
	networkACL *cmdNetworkACL

	flagIsProperty bool
}

// Command returns a cobra.Command for use with (*cobra.Command).AddCommand.
func (c *cmdNetworkACLSet) Command() *cobra.Command {
	cmd := &cobra.Command{}
	cmd.Use = usage("set", i18n.G("[<remote>:]<ACL> <key>=<value>..."))
	cmd.Short = i18n.G("Set network ACL configuration keys")
	cmd.Long = cli.FormatSection(i18n.G("Description"), i18n.G(
		`Set network ACL configuration keys

For backward compatibility, a single configuration key may still be set with:
    incus network set [<remote>:]<ACL> <key> <value>`))

	cmd.Flags().BoolVarP(&c.flagIsProperty, "property", "p", false, i18n.G("Set the key as a network ACL property"))
	cmd.RunE = c.Run

	cmd.ValidArgsFunction = func(_ *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		if len(args) == 0 {
			return c.global.cmpNetworkACLs(toComplete)
		}

		return nil, cobra.ShellCompDirectiveNoFileComp
	}

	return cmd
}

// Run runs the actual command logic.
func (c *cmdNetworkACLSet) Run(cmd *cobra.Command, args []string) error {
	// Quick checks.
	exit, err := c.global.checkArgs(cmd, args, 2, -1)
	if exit {
		return err
	}

	// Parse remote.
	resources, err := c.global.parseServers(args[0])
	if err != nil {
		return err
	}

	resource := resources[0]

	if resource.name == "" {
		return errors.New(i18n.G("Missing network ACL name"))
	}

	// Get the network ACL.
	netACL, etag, err := resource.server.GetNetworkACL(resource.name)
	if err != nil {
		return err
	}

	// Set the keys.
	keys, err := getConfig(args[1:]...)
	if err != nil {
		return err
	}

	writable := netACL.Writable()
	if c.flagIsProperty {
		if cmd.Name() == "unset" {
			for k := range keys {
				err := unsetFieldByJSONTag(&writable, k)
				if err != nil {
					return fmt.Errorf(i18n.G("Error unsetting property: %v"), err)
				}
			}
		} else {
			err := unpackKVToWritable(&writable, keys)
			if err != nil {
				return fmt.Errorf(i18n.G("Error setting properties: %v"), err)
			}
		}
	} else {
		maps.Copy(writable.Config, keys)
	}

	return resource.server.UpdateNetworkACL(resource.name, writable, etag)
}

// Unset.
type cmdNetworkACLUnset struct {
	global        *cmdGlobal
	networkACL    *cmdNetworkACL
	networkACLSet *cmdNetworkACLSet

	flagIsProperty bool
}

// Command returns a cobra.Command for use with (*cobra.Command).AddCommand.
func (c *cmdNetworkACLUnset) Command() *cobra.Command {
	cmd := &cobra.Command{}
	cmd.Use = usage("unset", i18n.G("[<remote>:]<ACL> <key>"))
	cmd.Short = i18n.G("Unset network ACL configuration keys")
	cmd.Long = cli.FormatSection(i18n.G("Description"), i18n.G("Unset network ACL configuration keys"))
	cmd.RunE = c.Run

	cmd.Flags().BoolVarP(&c.flagIsProperty, "property", "p", false, i18n.G("Unset the key as a network ACL property"))

	cmd.ValidArgsFunction = func(_ *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		if len(args) == 0 {
			return c.global.cmpNetworkACLs(toComplete)
		}

		if len(args) == 1 {
			return c.global.cmpNetworkACLConfigs(args[0])
		}

		return nil, cobra.ShellCompDirectiveNoFileComp
	}

	return cmd
}

// Run runs the actual command logic.
func (c *cmdNetworkACLUnset) Run(cmd *cobra.Command, args []string) error {
	// Quick checks.
	exit, err := c.global.checkArgs(cmd, args, 2, 2)
	if exit {
		return err
	}

	c.networkACLSet.flagIsProperty = c.flagIsProperty

	args = append(args, "")
	return c.networkACLSet.Run(cmd, args)
}

// Edit.
type cmdNetworkACLEdit struct {
	global     *cmdGlobal
	networkACL *cmdNetworkACL
}

// Command returns a cobra.Command for use with (*cobra.Command).AddCommand.
func (c *cmdNetworkACLEdit) Command() *cobra.Command {
	cmd := &cobra.Command{}
	cmd.Use = usage("edit", i18n.G("[<remote>:]<ACL>"))
	cmd.Short = i18n.G("Edit network ACL configurations as YAML")
	cmd.Long = cli.FormatSection(i18n.G("Description"), i18n.G("Edit network ACL configurations as YAML"))

	cmd.RunE = c.Run

	cmd.ValidArgsFunction = func(_ *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		if len(args) == 0 {
			return c.global.cmpNetworkACLs(toComplete)
		}

		return nil, cobra.ShellCompDirectiveNoFileComp
	}

	return cmd
}

func (c *cmdNetworkACLEdit) helpTemplate() string {
	return i18n.G(
		`### This is a YAML representation of the network ACL.
### Any line starting with a '# will be ignored.
###
### A network ACL consists of a set of rules and configuration items.
###
### An example would look like:
### name: allow-all-inbound
### description: test desc
### egress: []
### ingress:
### - action: allow
###   state: enabled
###   protocol: ""
###   source: ""
###   source_port: ""
###   destination: ""
###   destination_port: ""
###   icmp_type: ""
###   icmp_code: ""
### config:
###  user.foo: bah
###
### Note that only the ingress and egress rules, description and configuration keys can be changed.`)
}

// Run runs the actual command logic.
func (c *cmdNetworkACLEdit) Run(cmd *cobra.Command, args []string) error {
	// Quick checks.
	exit, err := c.global.checkArgs(cmd, args, 1, 1)
	if exit {
		return err
	}

	// Parse remote.
	resources, err := c.global.parseServers(args[0])
	if err != nil {
		return err
	}

	resource := resources[0]

	if resource.name == "" {
		return errors.New(i18n.G("Missing network ACL name"))
	}

	// If stdin isn't a terminal, read text from it
	if !termios.IsTerminal(getStdinFd()) {
		contents, err := io.ReadAll(os.Stdin)
		if err != nil {
			return err
		}

		// Allow output of `incus network acl show` command to be passed in here, but only take the contents
		// of the NetworkACLPut fields when updating the ACL. The other fields are silently discarded.
		newdata := api.NetworkACL{}
		err = yaml.UnmarshalStrict(contents, &newdata)
		if err != nil {
			return err
		}

		return resource.server.UpdateNetworkACL(resource.name, newdata.NetworkACLPut, "")
	}

	// Get the current config.
	netACL, etag, err := resource.server.GetNetworkACL(resource.name)
	if err != nil {
		return err
	}

	data, err := yaml.Marshal(&netACL)
	if err != nil {
		return err
	}

	// Spawn the editor.
	content, err := textEditor("", []byte(c.helpTemplate()+"\n\n"+string(data)))
	if err != nil {
		return err
	}

	for {
		// Parse the text received from the editor.
		newdata := api.NetworkACL{} // We show the full ACL info, but only send the writable fields.
		err = yaml.UnmarshalStrict(content, &newdata)
		if err == nil {
			err = resource.server.UpdateNetworkACL(resource.name, newdata.Writable(), etag)
		}

		// Respawn the editor.
		if err != nil {
			fmt.Fprintf(os.Stderr, i18n.G("Config parsing error: %s")+"\n", err)
			fmt.Println(i18n.G("Press enter to open the editor again or ctrl+c to abort change"))

			_, err := os.Stdin.Read(make([]byte, 1))
			if err != nil {
				return err
			}

			content, err = textEditor("", content)
			if err != nil {
				return err
			}

			continue
		}

		break
	}

	return nil
}

// Rename.
type cmdNetworkACLRename struct {
	global     *cmdGlobal
	networkACL *cmdNetworkACL
}

// Command returns a cobra.Command for use with (*cobra.Command).AddCommand.
func (c *cmdNetworkACLRename) Command() *cobra.Command {
	cmd := &cobra.Command{}
	cmd.Use = usage("rename", i18n.G("[<remote>:]<ACL> <new-name>"))
	cmd.Aliases = []string{"mv"}
	cmd.Short = i18n.G("Rename network ACLs")
	cmd.Long = cli.FormatSection(i18n.G("Description"), i18n.G("Rename network ACLs"))
	cmd.RunE = c.Run

	cmd.ValidArgsFunction = func(_ *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		if len(args) == 0 {
			return c.global.cmpNetworkACLs(toComplete)
		}

		return nil, cobra.ShellCompDirectiveNoFileComp
	}

	return cmd
}

// Run runs the actual command logic.
func (c *cmdNetworkACLRename) Run(cmd *cobra.Command, args []string) error {
	// Quick checks.
	exit, err := c.global.checkArgs(cmd, args, 2, 2)
	if exit {
		return err
	}

	// Parse remote.
	resources, err := c.global.parseServers(args[0])
	if err != nil {
		return err
	}

	resource := resources[0]

	if resource.name == "" {
		return errors.New(i18n.G("Missing network ACL name"))
	}

	// Rename the network.
	err = resource.server.RenameNetworkACL(resource.name, api.NetworkACLPost{Name: args[1]})
	if err != nil {
		return err
	}

	if !c.global.flagQuiet {
		fmt.Printf(i18n.G("Network ACL %s renamed to %s")+"\n", resource.name, args[1])
	}

	return nil
}

// Delete.
type cmdNetworkACLDelete struct {
	global     *cmdGlobal
	networkACL *cmdNetworkACL
}

// Command returns a cobra.Command for use with (*cobra.Command).AddCommand.
func (c *cmdNetworkACLDelete) Command() *cobra.Command {
	cmd := &cobra.Command{}
	cmd.Use = usage("delete", i18n.G("[<remote>:]<ACL>"))
	cmd.Aliases = []string{"rm", "remove"}
	cmd.Short = i18n.G("Delete network ACLs")
	cmd.Long = cli.FormatSection(i18n.G("Description"), i18n.G("Delete network ACLs"))
	cmd.RunE = c.Run

	cmd.ValidArgsFunction = func(_ *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		if len(args) == 0 {
			return c.global.cmpNetworkACLs(toComplete)
		}

		return nil, cobra.ShellCompDirectiveNoFileComp
	}

	return cmd
}

// Run runs the actual command logic.
func (c *cmdNetworkACLDelete) Run(cmd *cobra.Command, args []string) error {
	// Quick checks.
	exit, err := c.global.checkArgs(cmd, args, 1, 1)
	if exit {
		return err
	}

	// Parse remote.
	resources, err := c.global.parseServers(args[0])
	if err != nil {
		return err
	}

	resource := resources[0]

	if resource.name == "" {
		return errors.New(i18n.G("Missing network ACL name"))
	}

	// Delete the network ACL.
	err = resource.server.DeleteNetworkACL(resource.name)
	if err != nil {
		return err
	}

	if !c.global.flagQuiet {
		fmt.Printf(i18n.G("Network ACL %s deleted")+"\n", resource.name)
	}

	return nil
}

// Add/Remove Rule.
type cmdNetworkACLRule struct {
	global          *cmdGlobal
	networkACL      *cmdNetworkACL
	flagRemoveForce bool
	flagDescription string
}

// Command returns a cobra.Command for use with (*cobra.Command).AddCommand.
func (c *cmdNetworkACLRule) Command() *cobra.Command {
	cmd := &cobra.Command{}
	cmd.Use = usage("rule")
	cmd.Short = i18n.G("Manage network ACL rules")
	cmd.Long = cli.FormatSection(i18n.G("Description"), i18n.G("Manage network ACL rules"))

	// Rule Add.
	cmd.AddCommand(c.CommandAdd())

	// Rule Remove.
	cmd.AddCommand(c.CommandRemove())

	return cmd
}

// CommandAdd returns a cobra.Command for use with (*cobra.Command).AddCommand.
func (c *cmdNetworkACLRule) CommandAdd() *cobra.Command {
	cmd := &cobra.Command{}
	cmd.Use = usage("add", i18n.G("[<remote>:]<ACL> <direction> <key>=<value>..."))
	cmd.Aliases = []string{"create"}
	cmd.Short = i18n.G("Add rules to an ACL")
	cmd.Long = cli.FormatSection(i18n.G("Description"), i18n.G("Add rules to an ACL"))

	cmd.Flags().StringVar(&c.flagDescription, "description", "", i18n.G("Rule description")+"``")

	cmd.RunE = c.RunAdd

	cmd.ValidArgsFunction = func(_ *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		if len(args) == 0 {
			return c.global.cmpNetworkACLs(toComplete)
		}

		if len(args) == 1 {
			return []string{"ingress", "egress"}, cobra.ShellCompDirectiveNoFileComp
		}

		if len(args) == 2 {
			return c.global.cmpNetworkACLRuleProperties()
		}

		return nil, cobra.ShellCompDirectiveNoFileComp
	}

	return cmd
}

// networkACLRuleJSONStructFieldMap returns a map of JSON tag names to struct field indices for api.NetworkACLRule.
func networkACLRuleJSONStructFieldMap() map[string]int {
	// Use reflect to get field names in rule from json tags.
	ruleType := reflect.TypeOf(api.NetworkACLRule{})
	allowedKeys := make(map[string]int, ruleType.NumField())

	for i := range ruleType.NumField() {
		field := ruleType.Field(i)
		if field.PkgPath != "" {
			continue // Skip unexported fields. It is empty for upper case (exported) field names.
		}

		if field.Type.Name() != "string" {
			continue // Skip non-string fields.
		}

		// Split the json tag into its name and options (e.g. json:"action,omitempty").
		tagParts := strings.SplitN(string(field.Tag.Get(("json"))), ",", 2)
		fieldName := tagParts[0]

		if fieldName == "" {
			continue // Skip fields with no tagged field name.
		}

		allowedKeys[fieldName] = i // Add the name to allowed keys and record field index.
	}

	return allowedKeys
}

// parseConfigKeysToRule converts a map of key/value pairs into an api.NetworkACLRule using reflection.
func (c *cmdNetworkACLRule) parseConfigToRule(config map[string]string) (*api.NetworkACLRule, error) {
	// Use reflect to get struct field indices in NetworkACLRule for json tags.
	allowedKeys := networkACLRuleJSONStructFieldMap()

	// Initialize new rule.
	rule := api.NetworkACLRule{}
	ruleValue := reflect.ValueOf(&rule).Elem()

	for k, v := range config {
		fieldIndex, found := allowedKeys[k]
		if !found {
			return nil, fmt.Errorf(i18n.G("Unknown key: %s"), k)
		}

		fieldValue := ruleValue.Field(fieldIndex)
		if !fieldValue.CanSet() {
			return nil, fmt.Errorf(i18n.G("Cannot set key: %s"), k)
		}

		fieldValue.SetString(v) // Set the value into the struct field.
	}

	return &rule, nil
}

// RunAdd runs the actual command logic.
func (c *cmdNetworkACLRule) RunAdd(cmd *cobra.Command, args []string) error {
	// Quick checks.
	exit, err := c.global.checkArgs(cmd, args, 2, -1)
	if exit {
		return err
	}

	// Parse remote.
	resources, err := c.global.parseServers(args[0])
	if err != nil {
		return err
	}

	resource := resources[0]

	if resource.name == "" {
		return errors.New(i18n.G("Missing network ACL name"))
	}

	// Get config keys from arguments.
	keys, err := getConfig(args[2:]...)
	if err != nil {
		return err
	}

	// Get the network ACL.
	netACL, etag, err := resource.server.GetNetworkACL(resource.name)
	if err != nil {
		return err
	}

	rule, err := c.parseConfigToRule(keys)
	if err != nil {
		return err
	}

	if c.flagDescription != "" {
		rule.Description = c.flagDescription
	}

	rule.Normalise() // Strip space.

	// Default to enabled if not specified.
	if rule.State == "" {
		rule.State = "enabled"
	}

	// Add rule to the requested direction (if direction valid).
	switch args[1] {
	case "ingress":
		netACL.Ingress = append(netACL.Ingress, *rule)
	case "egress":
		netACL.Egress = append(netACL.Egress, *rule)
	default:
		return errors.New(i18n.G("The direction argument must be one of: ingress, egress"))
	}

	return resource.server.UpdateNetworkACL(resource.name, netACL.Writable(), etag)
}

// CommandRemove returns a cobra.Command for use with (*cobra.Command).AddCommand.
func (c *cmdNetworkACLRule) CommandRemove() *cobra.Command {
	cmd := &cobra.Command{}
	cmd.Use = usage("remove", i18n.G("[<remote>:]<ACL> <direction> <key>=<value>..."))
	cmd.Aliases = []string{"delete", "rm"}
	cmd.Short = i18n.G("Remove rules from an ACL")
	cmd.Long = cli.FormatSection(i18n.G("Description"), i18n.G("Remove rules from an ACL"))
	cmd.Flags().BoolVar(&c.flagRemoveForce, "force", false, i18n.G("Remove all rules that match"))

	cmd.RunE = c.RunRemove

	cmd.ValidArgsFunction = func(_ *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		if len(args) == 0 {
			return c.global.cmpNetworkACLs(toComplete)
		}

		if len(args) == 1 {
			return []string{"ingress", "egress"}, cobra.ShellCompDirectiveNoFileComp
		}

		if len(args) == 2 {
			return c.global.cmpNetworkACLRuleProperties()
		}

		return nil, cobra.ShellCompDirectiveNoFileComp
	}

	return cmd
}

// RunRemove runs the actual command logic.
func (c *cmdNetworkACLRule) RunRemove(cmd *cobra.Command, args []string) error {
	// Quick checks.
	exit, err := c.global.checkArgs(cmd, args, 2, -1)
	if exit {
		return err
	}

	// Parse remote.
	resources, err := c.global.parseServers(args[0])
	if err != nil {
		return err
	}

	resource := resources[0]

	if resource.name == "" {
		return errors.New(i18n.G("Missing network ACL name"))
	}

	// Get config filters from arguments.
	filters, err := getConfig(args[2:]...)
	if err != nil {
		return err
	}

	// Get the network ACL.
	netACL, etag, err := resource.server.GetNetworkACL(resource.name)
	if err != nil {
		return err
	}

	// Use reflect to get struct field indices in NetworkACLRule for json tags.
	allowedKeys := networkACLRuleJSONStructFieldMap()

	// Check the supplied filters match possible fields.
	for k := range filters {
		_, found := allowedKeys[k]
		if !found {
			return fmt.Errorf(i18n.G("Unknown key: %s"), k)
		}
	}

	// isFilterMatch returns whether the supplied rule has matching field values in the filters supplied.
	// If no filters are supplied, then the rule is considered to have matched.
	isFilterMatch := func(rule *api.NetworkACLRule, filters map[string]string) bool {
		ruleValue := reflect.ValueOf(rule).Elem()

		for k, v := range filters {
			fieldIndex, found := allowedKeys[k]
			if !found {
				return false
			}

			fieldValue := ruleValue.Field(fieldIndex)
			if fieldValue.String() != v {
				return false
			}
		}

		return true // Match found as all struct fields match the supplied filter values.
	}

	// removeFromRules removes a single rule that matches the filters supplied. If multiple rules match then
	// an error is returned unless c.flagRemoveForce is true, in which case all matching rules are removed.
	removeFromRules := func(rules []api.NetworkACLRule, filters map[string]string) ([]api.NetworkACLRule, error) {
		removed := false
		newRules := make([]api.NetworkACLRule, 0, len(rules))

		for _, r := range rules {
			if isFilterMatch(&r, filters) {
				if removed && !c.flagRemoveForce {
					return nil, errors.New(i18n.G("Multiple rules match. Use --force to remove them all"))
				}

				removed = true
				continue // Don't add removed rule to newRules.
			}

			newRules = append(newRules, r)
		}

		if !removed {
			return nil, errors.New(i18n.G("No matching rule(s) found"))
		}

		return newRules, nil
	}

	// Remove matching rule(s) from the requested direction (if direction valid).
	switch args[1] {
	case "ingress":
		rules, err := removeFromRules(netACL.Ingress, filters)
		if err != nil {
			return err
		}

		netACL.Ingress = rules
	case "egress":
		rules, err := removeFromRules(netACL.Egress, filters)
		if err != nil {
			return err
		}

		netACL.Egress = rules
	default:
		return errors.New(i18n.G("The direction argument must be one of: ingress, egress"))
	}

	return resource.server.UpdateNetworkACL(resource.name, netACL.Writable(), etag)
}
