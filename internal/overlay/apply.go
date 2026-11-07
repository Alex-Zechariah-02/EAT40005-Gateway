// Applying decisions: Overlay update merges, in order, on a copy, refusing
// any change that would widen access or rewrite a ledger fact.

package overlay

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"

	"github.com/Alex-Zechariah-02/apiward/internal/exchange"
)

// Operation members a decision may change, each only towards stricter.
const (
	accessMember = "x-apiward-access"
	rateMember   = "x-apiward-rate"
	bytesMember  = "x-apiward-max-bytes"
)

// Apply returns a copy of the document with every action applied in order,
// and the actions that selected nothing ("actions[<i>]: <target>"). An
// action that selects more than one node, merges incompatible values, or
// would widen an operation or rewrite a ledger item is an error, and nothing
// is returned. defaultBytes is the body limit of an operation without
// x-apiward-max-bytes.
func (d *Decisions) Apply(document map[string]any, defaultBytes int64) (map[string]any, []string, error) {
	out := clone(document).(map[string]any)
	stale := []string{}
	for i, a := range d.actions {
		found := a.target.selectNodes(out)
		switch {
		case len(found) == 0:
			stale = append(stale, fmt.Sprintf("actions[%d]: %s", i, a.text))
			continue
		case len(found) > 1:
			return nil, nil, fmt.Errorf("actions[%d]: the target selects %d nodes; a decision must select exactly one", i, len(found))
		}
		if err := applyOne(out, found[0], a.update, defaultBytes); err != nil {
			return nil, nil, fmt.Errorf("actions[%d]: %w", i, err)
		}
	}
	return out, stale, nil
}

// applyOne merges update into the node at location and checks the
// operation or ledger item that holds it.
func applyOne(document map[string]any, location []any, update any, defaultBytes int64) error {
	scope := location[:3]
	holder, _ := at(document, scope)
	before, isObject := holder.(map[string]any)
	if !isObject {
		return errors.New("the target is not inside an operation or a ledger item")
	}
	if location[0] == exchange.Member {
		if err := acceptance(before, update); err != nil {
			return err
		}
	} else if !slices.Contains(methods, location[2].(string)) {
		return errors.New("the target is not an operation")
	}
	before = clone(before).(map[string]any)
	node, _ := at(document, location)
	merged, err := merge(node, update)
	if err != nil {
		return err
	}
	put(document, location, merged)
	if location[0] == exchange.Member {
		return nil
	}
	after, _ := at(document, scope)
	return stricter(before, after.(map[string]any), defaultBytes)
}

// acceptance admits only {accepted: true, direction: looser} on a looser
// approximation: the one fact about a ledger item a decision may record.
func acceptance(item map[string]any, update any) error {
	want := map[string]any{"accepted": true, "direction": string(exchange.Looser)}
	if !reflect.DeepEqual(update, want) {
		return errors.New(`a decision on a ledger item must be exactly {"accepted": true, "direction": "looser"}`)
	}
	if item["class"] != string(exchange.Approximated) || item["direction"] != string(exchange.Looser) {
		return errors.New("only a looser approximation can be accepted")
	}
	return nil
}

// stricter refuses any change to an operation except a stricter access,
// rate or body limit.
func stricter(before, after map[string]any, defaultBytes int64) error {
	for name := range union(before, after) {
		old, value := before[name], after[name]
		if reflect.DeepEqual(old, value) {
			continue
		}
		var err error
		switch name {
		case accessMember:
			if value != "deny" {
				err = errors.New("x-apiward-access may only be set to deny")
			}
		case rateMember:
			err = tighterRate(old, value)
		case bytesMember:
			limit, known := integer(old)
			if !known {
				limit, known = defaultBytes, old == nil
			}
			if size, valid := integer(value); !known || !valid || size > limit {
				err = fmt.Errorf("x-apiward-max-bytes may only be lowered (now %d)", limit)
			}
		default:
			err = fmt.Errorf("a decision may change only %s, %s and %s of an operation", accessMember, rateMember, bytesMember)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// rate is the part of a rate rule that decides how strict it is.
type rate struct {
	algorithm, key string
	count, window  int64
}

func rateOf(value any) (rate, bool) {
	object, _ := value.(map[string]any)
	algorithm, _ := object["algorithm"].(string)
	key, _ := object["key"].(string)
	count, countOK := integer(object["count"])
	window, windowOK := integer(object["window_seconds"])
	return rate{algorithm, key, count, window}, object != nil && countOK && windowOK
}

// tighterRate admits a new rule where none was, or the same algorithm and
// key with no higher count and no shorter window.
func tighterRate(old, value any) error {
	next, valid := rateOf(value)
	if !valid {
		return errors.New("x-apiward-rate must be a rate rule with integer count and window_seconds")
	}
	if old == nil {
		return nil
	}
	current, known := rateOf(old)
	if !known || next.algorithm != current.algorithm || next.key != current.key ||
		next.count > current.count || next.window < current.window {
		return errors.New("x-apiward-rate may only lower count or lengthen window_seconds, with the same algorithm and key")
	}
	return nil
}

func integer(value any) (int64, bool) {
	number, isNumber := value.(json.Number)
	n, err := number.Int64()
	return n, isNumber && err == nil
}

func union(a, b map[string]any) map[string]bool {
	names := map[string]bool{}
	for name := range a {
		names[name] = true
	}
	for name := range b {
		names[name] = true
	}
	return names
}

// merge applies an Overlay update to a node: objects merge recursively,
// arrays concatenate (or take one more element), primitives are replaced.
// An object or array never becomes another kind.
func merge(node, update any) (any, error) {
	switch current := node.(type) {
	case map[string]any:
		object, isObject := update.(map[string]any)
		if !isObject {
			return nil, errors.New("the update for an object must be an object")
		}
		for name, value := range object {
			old, present := current[name]
			if !present {
				current[name] = clone(value)
				continue
			}
			merged, err := merge(old, value)
			if err != nil {
				return nil, err
			}
			current[name] = merged
		}
		return current, nil
	case []any:
		if list, isList := update.([]any); isList {
			return append(current, clone(list).([]any)...), nil
		}
		return append(current, clone(update)), nil
	}
	if primitive(update) {
		return update, nil
	}
	return nil, errors.New("the update for a single value must be a single value")
}

func primitive(value any) bool {
	switch value.(type) {
	case map[string]any, []any:
		return false
	}
	return true
}

// put stores value at location, whose parent exists.
func put(document map[string]any, location []any, value any) {
	parent, _ := at(document, location[:len(location)-1])
	switch p := parent.(type) {
	case map[string]any:
		p[location[len(location)-1].(string)] = value
	case []any:
		p[location[len(location)-1].(int)] = value
	}
}

// clone deep-copies a decoded JSON value.
func clone(value any) any {
	switch v := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for name, child := range v {
			out[name] = clone(child)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, child := range v {
			out[i] = clone(child)
		}
		return out
	}
	return value
}
