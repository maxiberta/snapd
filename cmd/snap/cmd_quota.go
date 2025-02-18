// -*- Mode: Go; indent-tabs-mode: t -*-

/*
 * Copyright (C) 2021 Canonical Ltd
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License version 3 as
 * published by the Free Software Foundation.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 *
 */

package main

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/jessevdk/go-flags"

	"github.com/snapcore/snapd/client"
	"github.com/snapcore/snapd/gadget/quantity"
	"github.com/snapcore/snapd/i18n"
	"github.com/snapcore/snapd/strutil"
)

var shortQuotaHelp = i18n.G("Show quota group for a set of snaps")
var longQuotaHelp = i18n.G(`
The quota command shows information about a quota group, including the set of 
snaps and any sub-groups it contains, as well as its resource constraints and 
the current usage of those constrained resources.
`)

var shortQuotasHelp = i18n.G("Show quota groups")
var longQuotasHelp = i18n.G(`
The quotas command shows all quota groups.
`)

var shortRemoveQuotaHelp = i18n.G("Remove quota group")
var longRemoveQuotaHelp = i18n.G(`
The remove-quota command removes the given quota group. 

Currently, only quota groups with no sub-groups can be removed. In order to 
remove a quota group with sub-groups, the sub-groups must first be removed until
there are no sub-groups for the group, then the group itself can be removed.
`)

var shortSetQuotaHelp = i18n.G(`Create or update a quota group.`)
var longSetQuotaHelp = i18n.G(`
The set-quota command updates or creates a quota group with the specified set of
snaps.

A quota group sets resource limits on the set of snaps it contains. Snaps can 
be at most in one quota group but quota groups can be nested. Nested quota 
groups are subject to the restriction that the total sum of each existing quota
in sub-groups cannot exceed that of the parent group the nested groups are part of.

All provided snaps are appended to the group; to remove a snap from a
quota group, the entire group must be removed with remove-quota and recreated 
without the snap. To remove a sub-group from the quota group, the 
sub-group must be removed directly with the remove-quota command.

The memory limit for a quota group can be increased but not decreased. To
decrease the memory limit for a quota group, the entire group must be removed
with the remove-quota command and recreated with a lower limit. Increasing the
memory limit for a quota group does not restart any services associated with 
snaps in the quota group.

The CPU limit for a quota group can be both increased and decreased after being
set on a quota group. The CPU limit can be specified as a single percentage which
means that the quota group is allowed an overall percentage of the CPU resources. Setting
it to 50% means that the quota group is allowed to use up to 50% of all CPU cores
in the allowed CPU set. Setting the percentage to 2x100% means that the quota group 
is allowed up to 100% on two cpu cores.

The CPU set limit for a quota group can be modified to include new cpus, or to remove
existing cpus from the quota already set.

The threads limit for a quota group can be increased but not decreased. To
decrease the threads limit for a quota group, the entire group must be removed
with the remove-quota command and recreated with a lower limit.

New quotas can be set on existing quota groups, but existing quotas cannot be removed
from a quota group, without removing and recreating the entire group.

Adding new snaps to a quota group will result in all non-disabled services in 
that snap being restarted.

An existing sub group cannot be moved from one parent to another.
`)

func init() {
	// TODO: unhide the commands when non-experimental
	cmd := addCommand("set-quota", shortSetQuotaHelp, longSetQuotaHelp,
		func() flags.Commander { return &cmdSetQuota{} },
		waitDescs.also(map[string]string{
			"memory":  i18n.G("Memory quota"),
			"cpu":     i18n.G("CPU quota"),
			"cpu-set": i18n.G("CPU set quota"),
			"threads": i18n.G("Threads quota"),
			"parent":  i18n.G("Parent quota group"),
		}), nil)
	cmd.hidden = true

	cmd = addCommand("quota", shortQuotaHelp, longQuotaHelp, func() flags.Commander { return &cmdQuota{} }, nil, nil)
	cmd.hidden = true

	cmd = addCommand("quotas", shortQuotasHelp, longQuotasHelp, func() flags.Commander { return &cmdQuotas{} }, nil, nil)
	cmd.hidden = true

	cmd = addCommand("remove-quota", shortRemoveQuotaHelp, longRemoveQuotaHelp, func() flags.Commander { return &cmdRemoveQuota{} }, nil, nil)
	cmd.hidden = true
}

type cmdSetQuota struct {
	waitMixin

	MemoryMax  string `long:"memory" optional:"true"`
	CPUMax     string `long:"cpu" optional:"true"`
	CPUSet     string `long:"cpu-set" optional:"true"`
	ThreadsMax string `long:"threads" optional:"true"`
	Parent     string `long:"parent" optional:"true"`
	Positional struct {
		GroupName string              `positional-arg-name:"<group-name>" required:"true"`
		Snaps     []installedSnapName `positional-arg-name:"<snap>" optional:"true"`
	} `positional-args:"yes"`
}

// example cpu quota string: "2x50%", "90%"
var cpuValueMatcher = regexp.MustCompile(`([0-9]+x)?([0-9]+)%`)

func parseCpuQuota(cpuMax string) (count int, percentage int, err error) {
	parseError := func(input string) error {
		return fmt.Errorf("cannot parse cpu quota string %q", input)
	}

	match := cpuValueMatcher.FindStringSubmatch(cpuMax)
	if match == nil {
		return 0, 0, parseError(cpuMax)
	}

	// Detect whether format was NxM% or M%
	if len(match[1]) > 0 {
		// Assume format was NxM%
		count, err = strconv.Atoi(match[1][:len(match[1])-1])
		if err != nil || count == 0 {
			return 0, 0, parseError(cpuMax)
		}
	}

	percentage, err = strconv.Atoi(match[2])
	if err != nil || percentage == 0 {
		return 0, 0, parseError(cpuMax)
	}
	return count, percentage, nil
}

func parseQuotas(maxMemory string, cpuMax string, cpuSet string, threadsMax string) (*client.QuotaValues, error) {
	var mem int64
	var cpuCount int
	var cpuPercentage int
	var cpus []int
	var threads int

	if maxMemory != "" {
		value, err := strutil.ParseByteSize(maxMemory)
		if err != nil {
			return nil, err
		}
		mem = value
	}

	if cpuMax != "" {
		countValue, percentageValue, err := parseCpuQuota(cpuMax)
		if err != nil {
			return nil, err
		}
		if percentageValue > 100 || percentageValue <= 0 {
			return nil, fmt.Errorf("cannot use value %v: cpu quota percentage must be between 1 and 100", percentageValue)
		}

		cpuCount = countValue
		cpuPercentage = percentageValue
	}

	if cpuSet != "" {
		cpuTokens := strutil.CommaSeparatedList(cpuSet)
		for _, cpuToken := range cpuTokens {
			cpu, err := strconv.ParseUint(cpuToken, 10, 32)
			if err != nil {
				return nil, fmt.Errorf("cannot parse CPU set value %q", cpuToken)
			}
			cpus = append(cpus, int(cpu))
		}
	}

	if threadsMax != "" {
		value, err := strconv.ParseUint(threadsMax, 10, 32)
		if err != nil {
			return nil, fmt.Errorf("cannot use threads value %q", threadsMax)
		}
		threads = int(value)
	}

	return &client.QuotaValues{
		Memory: quantity.Size(mem),
		CPU: &client.QuotaCPUValues{
			Count:      cpuCount,
			Percentage: cpuPercentage,
		},
		CPUSet: &client.QuotaCPUSetValues{
			CPUs: cpus,
		},
		Threads: threads,
	}, nil
}

func (x *cmdSetQuota) Execute(args []string) (err error) {
	quotaProvided := x.MemoryMax != "" || x.CPUMax != "" || x.CPUSet != "" || x.ThreadsMax != ""

	names := installedSnapNames(x.Positional.Snaps)

	// figure out if the group exists or not to make error messages more useful
	groupExists := false
	if _, err = x.client.GetQuotaGroup(x.Positional.GroupName); err == nil {
		groupExists = true
	}

	var chgID string

	switch {
	case !quotaProvided && x.Parent == "" && len(x.Positional.Snaps) == 0:
		// no snaps were specified, no memory limit was specified, and no parent
		// was specified, so just the group name was provided - this is not
		// supported since there is nothing to change/create

		if groupExists {
			return fmt.Errorf("no options set to change quota group")
		}
		return fmt.Errorf("cannot create quota group without any limit")

	case !quotaProvided && x.Parent != "" && len(x.Positional.Snaps) == 0:
		// this is either trying to create a new group with a parent and forgot
		// to specify the limits for the new group, or the user is trying
		// to re-parent a group, i.e. move it from the current parent to a
		// different one, which is currently unsupported

		if groupExists {
			// TODO: or this could be setting the parent to the existing parent,
			// which is effectively no change or update but maybe we allow since
			// it's a noop?
			return fmt.Errorf("cannot move a quota group to a new parent")
		}
		return fmt.Errorf("cannot create quota group without any limits")

	case quotaProvided:
		// we have a limits to set for this group, so specify that along
		// with whatever snaps may have been provided and whatever parent may
		// have been specified
		quotaValues, err := parseQuotas(x.MemoryMax, x.CPUMax, x.CPUSet, x.ThreadsMax)
		if err != nil {
			return err
		}

		// note that the group could currently exist with a parent, and we could
		// be specifying x.Parent as "" here - in the future that may mean to
		// orphan a sub-group to no longer have a parent, but currently it just
		// means leave the group with whatever parent it has, or if it doesn't
		// currently exist, create the group without a parent group
		chgID, err = x.client.EnsureQuota(x.Positional.GroupName, x.Parent, names, quotaValues)
		if err != nil {
			return err
		}
	case len(x.Positional.Snaps) != 0:
		// there are snaps specified for this group but no limits, so the
		// group must already exist and we must be adding the specified snaps to
		// the group

		// TODO: this case may someday also imply overwriting the current set of
		// snaps with whatever was specified with some option, but we don't
		// currently support that, so currently all snaps specified here are
		// just added to the group

		chgID, err = x.client.EnsureQuota(x.Positional.GroupName, x.Parent, names, nil)
		if err != nil {
			return err
		}
	default:
		// should be logically impossible to reach here
		panic("impossible set of options")
	}

	if _, err := x.wait(chgID); err != nil {
		if err == noWait {
			return nil
		}
		return err
	}

	return nil
}

type cmdQuota struct {
	clientMixin

	Positional struct {
		GroupName string `positional-arg-name:"<group-name>" required:"true"`
	} `positional-args:"yes"`
}

func (x *cmdQuota) Execute(args []string) (err error) {
	if len(args) != 0 {
		return fmt.Errorf("too many arguments provided")
	}

	group, err := x.client.GetQuotaGroup(x.Positional.GroupName)
	if err != nil {
		return err
	}

	w := tabWriter()
	defer w.Flush()

	fmt.Fprintf(w, "name:\t%s\n", group.GroupName)
	if group.Parent != "" {
		fmt.Fprintf(w, "parent:\t%s\n", group.Parent)
	}

	// Constraints should always be non-nil, since a quota group always needs to
	// have at least one limit set
	if group.Constraints == nil {
		return fmt.Errorf("internal error: constraints is missing from daemon response")
	}

	fmt.Fprintf(w, "constraints:\n")

	if group.Constraints.Memory != 0 {
		val := strings.TrimSpace(fmtSize(int64(group.Constraints.Memory)))
		fmt.Fprintf(w, "  memory:\t%s\n", val)
	}
	if group.Constraints.CPU != nil {
		fmt.Fprintf(w, "  cpu-count:\t%d\n", group.Constraints.CPU.Count)
		fmt.Fprintf(w, "  cpu-percentage:\t%d\n", group.Constraints.CPU.Percentage)
	}
	if group.Constraints.CPUSet != nil && len(group.Constraints.CPUSet.CPUs) > 0 {
		cpus := strutil.IntsToCommaSeparated(group.Constraints.CPUSet.CPUs)
		fmt.Fprintf(w, "  cpu-set:\t%s\n", cpus)
	}
	if group.Constraints.Threads != 0 {
		fmt.Fprintf(w, "  threads:\t%d\n", group.Constraints.Threads)
	}

	memoryUsage := "0B"
	currentThreads := 0
	if group.Current != nil {
		memoryUsage = strings.TrimSpace(fmtSize(int64(group.Current.Memory)))
		currentThreads = group.Current.Threads
	}

	fmt.Fprintf(w, "current:\n")
	if group.Constraints.Memory != 0 {
		fmt.Fprintf(w, "  memory:\t%s\n", memoryUsage)
	}
	if group.Constraints.Threads != 0 {
		fmt.Fprintf(w, "  threads:\t%d\n", currentThreads)
	}

	if len(group.Subgroups) > 0 {
		fmt.Fprint(w, "subgroups:\n")
		for _, name := range group.Subgroups {
			fmt.Fprintf(w, "  - %s\n", name)
		}
	}
	if len(group.Snaps) > 0 {
		fmt.Fprint(w, "snaps:\n")
		for _, snapName := range group.Snaps {
			fmt.Fprintf(w, "  - %s\n", snapName)
		}
	}

	return nil
}

type cmdRemoveQuota struct {
	waitMixin

	Positional struct {
		GroupName string `positional-arg-name:"<group-name>" required:"true"`
	} `positional-args:"yes"`
}

func (x *cmdRemoveQuota) Execute(args []string) (err error) {
	chgID, err := x.client.RemoveQuotaGroup(x.Positional.GroupName)
	if err != nil {
		return err
	}

	if _, err := x.wait(chgID); err != nil {
		if err == noWait {
			return nil
		}
		return err
	}

	return nil
}

type cmdQuotas struct {
	clientMixin
}

func (x *cmdQuotas) Execute(args []string) (err error) {
	res, err := x.client.Quotas()
	if err != nil {
		return err
	}
	if len(res) == 0 {
		fmt.Fprintln(Stdout, i18n.G("No quota groups defined."))
		return nil
	}

	w := tabWriter()
	fmt.Fprintf(w, "Quota\tParent\tConstraints\tCurrent\n")
	err = processQuotaGroupsTree(res, func(q *client.QuotaGroupResult) error {
		if q.Constraints == nil {
			return fmt.Errorf("internal error: constraints is missing from daemon response")
		}

		var grpConstraints []string

		// format memory constraint as memory=N
		if q.Constraints.Memory != 0 {
			grpConstraints = append(grpConstraints, "memory="+strings.TrimSpace(fmtSize(int64(q.Constraints.Memory))))
		}

		// format cpu constraint as cpu=NxM%,cpu-set=x,y,z
		if q.Constraints.CPU != nil {
			if q.Constraints.CPU.Count != 0 {
				grpConstraints = append(grpConstraints, fmt.Sprintf("cpu=%dx", q.Constraints.CPU.Count))
			}
			if q.Constraints.CPU.Percentage != 0 {
				grpConstraints = append(grpConstraints, fmt.Sprintf("cpu=%d%%", q.Constraints.CPU.Percentage))
			}
		}

		if q.Constraints.CPUSet != nil && len(q.Constraints.CPUSet.CPUs) > 0 {
			cpus := strutil.IntsToCommaSeparated(q.Constraints.CPUSet.CPUs)
			grpConstraints = append(grpConstraints, "cpu-set="+cpus)
		}

		// format threads constraint as threads=N
		if q.Constraints.Threads != 0 {
			grpConstraints = append(grpConstraints, "threads="+strconv.Itoa(q.Constraints.Threads))
		}

		// format current resource values as memory=N,threads=N
		var grpCurrent []string
		if q.Current != nil {
			if q.Constraints.Memory != 0 && q.Current.Memory != 0 {
				grpCurrent = append(grpCurrent, "memory="+strings.TrimSpace(fmtSize(int64(q.Current.Memory))))
			}
			if q.Constraints.Threads != 0 && q.Current.Threads != 0 {
				grpCurrent = append(grpCurrent, "threads="+fmt.Sprintf("%d", q.Current.Threads))
			}
		}

		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", q.GroupName, q.Parent, strings.Join(grpConstraints, ","), strings.Join(grpCurrent, ","))

		return nil
	})
	if err != nil {
		return err
	}
	w.Flush()
	return nil
}

type quotaGroup struct {
	res       *client.QuotaGroupResult
	subGroups []*quotaGroup
}

type byQuotaName []*quotaGroup

func (q byQuotaName) Len() int           { return len(q) }
func (q byQuotaName) Swap(i, j int)      { q[i], q[j] = q[j], q[i] }
func (q byQuotaName) Less(i, j int) bool { return q[i].res.GroupName < q[j].res.GroupName }

// processQuotaGroupsTree recreates the hierarchy of quotas and then visits it
// recursively following the hierarchy first, then naming order.
func processQuotaGroupsTree(quotas []*client.QuotaGroupResult, handleGroup func(q *client.QuotaGroupResult) error) error {
	var roots []*quotaGroup
	groupLookup := make(map[string]*quotaGroup, len(quotas))

	for _, q := range quotas {
		grp := &quotaGroup{res: q}
		groupLookup[q.GroupName] = grp

		if q.Parent == "" {
			roots = append(roots, grp)
		}
	}

	sort.Sort(byQuotaName(roots))

	// populate sub-groups
	for _, g := range groupLookup {
		sort.Strings(g.res.Subgroups)
		for _, subgrpName := range g.res.Subgroups {
			subGroup, ok := groupLookup[subgrpName]
			if !ok {
				return fmt.Errorf("internal error: inconsistent groups received, unknown subgroup %q", subgrpName)
			}
			g.subGroups = append(g.subGroups, subGroup)
		}
	}

	var processGroups func(groups []*quotaGroup) error
	processGroups = func(groups []*quotaGroup) error {
		for _, g := range groups {
			if err := handleGroup(g.res); err != nil {
				return err
			}
			if len(g.subGroups) > 0 {
				if err := processGroups(g.subGroups); err != nil {
					return err
				}
			}
		}
		return nil
	}
	return processGroups(roots)
}
