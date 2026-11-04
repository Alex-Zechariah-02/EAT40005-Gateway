// Administrator permissions and names.

package gateway

import (
	"fmt"
	"strings"
)

// permission is a set of administrator rights. Each admin route requires
// exactly one; holding read never implies the right to change anything.
type permission int

const (
	permRead permission = 1 << iota
	permReadHistory
	permManageSubjects
	permManagePolicy

	permAll = permRead | permReadHistory | permManageSubjects | permManagePolicy
)

var permissionNames = []struct {
	bit  permission
	name string
}{
	{permRead, "read"},
	{permReadHistory, "read_history"},
	{permManageSubjects, "manage_subjects"},
	{permManagePolicy, "manage_policy"},
}

func (p permission) names() []string {
	names := []string{}
	for _, entry := range permissionNames {
		if p&entry.bit != 0 {
			names = append(names, entry.name)
		}
	}
	return names
}

// parsePermissions reads a comma-separated list of permission names.
func parsePermissions(list string) (permission, error) {
	var p permission
	for name := range strings.SplitSeq(list, ",") {
		found := false
		for _, entry := range permissionNames {
			if strings.TrimSpace(name) == entry.name {
				p |= entry.bit
				found = true
			}
		}
		if !found {
			return 0, fmt.Errorf("unknown permission %q", name)
		}
	}
	return p, nil
}

// Administrator names are short lowercase identifiers, safe to log.
func isAdminName(name string) bool {
	if name == "" || len(name) > maxIDLength {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		alphanumeric := (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')
		if alphanumeric {
			continue
		}
		if i == 0 || (c != '.' && c != '_' && c != '-') {
			return false
		}
	}
	return true
}
