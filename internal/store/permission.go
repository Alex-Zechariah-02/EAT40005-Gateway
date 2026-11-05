// Administrator permissions and names.

package store

import (
	"fmt"
	"strings"

	"github.com/Alex-Zechariah-02/apiward/internal/policy"
)

// Permission is a set of administrator rights. Each admin route requires
// exactly one; holding read never implies the right to change anything.
type Permission int

const (
	PermRead Permission = 1 << iota
	PermReadHistory
	PermManageSubjects
	PermManagePolicy

	PermAll = PermRead | PermReadHistory | PermManageSubjects | PermManagePolicy
)

var permissionNames = []struct {
	bit  Permission
	name string
}{
	{PermRead, "read"},
	{PermReadHistory, "read_history"},
	{PermManageSubjects, "manage_subjects"},
	{PermManagePolicy, "manage_policy"},
}

func (p Permission) Names() []string {
	names := []string{}
	for _, entry := range permissionNames {
		if p&entry.bit != 0 {
			names = append(names, entry.name)
		}
	}
	return names
}

// ParsePermissions reads a comma-separated list of permission names.
func ParsePermissions(list string) (Permission, error) {
	var p Permission
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
func IsAdminName(name string) bool {
	if name == "" || len(name) > policy.MaxIDLength {
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
