package cmd

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"

	"github.com/spf13/cobra"
)

// The AWS half of the installer's plugins (installer/internal/plugin): each
// plugin with AWS resources registers a pluginDef from an init function in
// its own file (plugin_<id>.go), which gives it
//
//   - `setup-aws plugin <id>`, which creates or reconciles its resources
//     (idempotent, every write through env.act so --plan writes nothing);
//   - a teardown step, run in the teardown's plugin phase before Substrate's
//     own data and the cluster go (plugin resources can hold the VPC's
//     subnets and security groups);
//   - inventory rows, so a teardown's final check sees what is left;
//   - tagging-API types it reads itself (TaggingTypes), whose stale entries
//     inventory must then ignore, as it does for its own kinds.
//
// Every resource a plugin creates carries the owner tag and TAGS, and its
// teardown deletes only what carries this deployment's owner tag (AC2.2).
type pluginDef struct {
	ID, Short string
	// Create runs `setup-aws plugin <ID>`.
	Create func(ctx context.Context, e *env) error
	// Teardown returns the plugin's teardown steps, in order. Cluster marks
	// steps --keep-cluster keeps (none, for most plugins: their resources are
	// Substrate data).
	Teardown func(e *tdEnv) []pluginStep
	// Inventory lists what is left of the plugin.
	Inventory func(ctx context.Context, e *tdEnv) ([]tdRow, error)
	// TaggingTypes are "service:type" pairs (as arnType splits an ARN) the
	// plugin's Inventory reads per service.
	TaggingTypes []string
}

type pluginStep struct {
	Cluster bool
	Step    tdStep
}

var pluginDefs = map[string]pluginDef{}

// registerPlugin adds d and its subcommand. A duplicate ID panics.
func registerPlugin(d pluginDef) {
	if d.ID == "" {
		panic("setup-aws: plugin with no ID")
	}
	if _, dup := pluginDefs[d.ID]; dup {
		panic("setup-aws: duplicate plugin " + d.ID)
	}
	pluginDefs[d.ID] = d
	if d.Create == nil {
		return
	}
	var del bool
	c := &cobra.Command{
		Use:   d.ID,
		Short: d.Short,
		Long: d.Short + `.

With --delete it deletes them instead, as teardown's plugin phase does: only
what carries this deployment's owner tag, every refusal found before the first
delete. --plan reads only.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if del {
				e, err := newTDEnv(cmd.Context(), cmd.OutOrStdout())
				if err != nil {
					return err
				}
				if err := e.deletePlugin(cmd.Context(), d); err != nil {
					return errors.New(e.mask(err.Error()))
				}
				return nil
			}
			e, err := newEnv(cmd.Context())
			if err != nil {
				return err
			}
			return d.Create(cmd.Context(), e)
		},
	}
	c.Flags().BoolVar(&del, "delete", false, "delete the plugin's AWS resources instead of creating them")
	pluginCmd.AddCommand(c)
}

// deletePlugin runs one plugin's teardown steps on their own (`setup-aws
// plugin <id> --delete`), with teardown's rules: every refusal is found before
// the first delete, and the plugin's inventory must be empty after.
func (e *tdEnv) deletePlugin(ctx context.Context, d pluginDef) error {
	if d.Teardown == nil {
		e.say("plugin %s has no AWS resources to delete", d.ID)
		return nil
	}
	var steps []tdStep
	for _, s := range d.Teardown(e) {
		steps = append(steps, s.Step)
	}
	var refusals []string
	for _, s := range steps {
		_, r, err := s(ctx)
		if err != nil {
			return err
		}
		refusals = append(refusals, r...)
	}
	if len(refusals) > 0 {
		return refusalError(refusals)
	}
	n := 0
	for _, s := range steps {
		ts, r, err := s(ctx)
		if err != nil {
			return err
		}
		if len(r) > 0 {
			return refusalError(r)
		}
		if err := e.run(ctx, ts); err != nil {
			return err
		}
		n += len(ts)
	}
	if n == 0 {
		e.say("nothing to delete")
	}
	if e.plan || d.Inventory == nil {
		return nil
	}
	left, err := d.Inventory(ctx, e)
	if err != nil {
		return err
	}
	if len(left) > 0 {
		e.printRows(left)
		return fmt.Errorf("%d resources of plugin %s are left (listed above)", len(left), d.ID)
	}
	e.say("Plugin %s deleted: nothing of it is left for %s", d.ID, e.n.OwnerValue)
	return nil
}

var pluginCmd = &cobra.Command{
	Use:   "plugin",
	Short: "Create the AWS resources of an optional plugin",
}

func init() { rootCmd.AddCommand(pluginCmd) }

// pluginReadsType reports a tagging-API type ("service:type") a registered
// plugin's Inventory reads itself. It is looked up in pluginDefs at run time,
// rather than added to tdKnown at registration, so a test that empties
// pluginDefs sees the core inventory exactly, whichever plugins are compiled
// in.
func pluginReadsType(t string) bool {
	for _, d := range pluginDefs {
		if slices.Contains(d.TaggingTypes, t) {
			return true
		}
	}
	return false
}

// sortedPluginDefs is every registered plugin by ID.
func sortedPluginDefs() []pluginDef {
	out := make([]pluginDef, 0, len(pluginDefs))
	for _, d := range pluginDefs {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// pluginPhase is the teardown's plugin phase: every plugin's steps, plugins by
// ID. A step marked Cluster is kept by --keep-cluster.
func (e *tdEnv) pluginPhase() tdPhase {
	var steps []tdStep
	for _, d := range sortedPluginDefs() {
		if d.Teardown == nil {
			continue
		}
		for _, s := range d.Teardown(e) {
			if s.Cluster && e.keepCluster {
				continue
			}
			steps = append(steps, s.Step)
		}
	}
	return tdPhase{steps: steps}
}

// pluginInventory is every plugin's inventory rows.
func (e *tdEnv) pluginInventory(ctx context.Context) ([]tdRow, error) {
	var rows []tdRow
	for _, d := range sortedPluginDefs() {
		if d.Inventory == nil {
			continue
		}
		r, err := d.Inventory(ctx, e)
		if err != nil {
			return nil, fmt.Errorf("plugin %s: %w", d.ID, err)
		}
		rows = append(rows, r...)
	}
	return rows, nil
}
