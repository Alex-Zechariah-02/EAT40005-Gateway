// Reading a decisions file: an Overlay 1.2.0 document restricted to update
// actions on operations and ledger items, checked once before any is applied.

package overlay

import (
	"errors"
	"fmt"
	"slices"
)

// Version is the only Overlay version read.
const Version = "1.2.0"

// MaxActions bounds the actions of one file; each scans the document once.
const MaxActions = 4096

// Decisions is a decisions file that passed Parse; only Parse makes one.
type Decisions struct {
	actions []action
}

type action struct {
	text   string // the target as written, for the stale list
	target target
	update any
}

// Parse checks a decoded decisions file against the profile: version
// 1.2.0, an info object, one to MaxActions update actions with an allowed
// target, and no other members (no remove, copy, components or extensions).
func Parse(root map[string]any) (*Decisions, error) {
	if err := members(root, "the decisions file", "overlay", "info", "actions", "$self", "extends"); err != nil {
		return nil, err
	}
	if root["overlay"] != Version {
		return nil, errors.New("the decisions file must state overlay: " + Version)
	}
	if err := checkInfo(root["info"]); err != nil {
		return nil, err
	}
	for _, name := range []string{"$self", "extends"} {
		if _, isText := root[name].(string); root[name] != nil && !isText {
			return nil, fmt.Errorf("%s must be text", name)
		}
	}
	list, _ := root["actions"].([]any)
	if len(list) == 0 || len(list) > MaxActions {
		return nil, fmt.Errorf("actions must list 1 to %d actions", MaxActions)
	}
	d := &Decisions{}
	for i, value := range list {
		a, err := parseAction(value)
		if err != nil {
			return nil, fmt.Errorf("actions[%d]: %w", i, err)
		}
		d.actions = append(d.actions, a)
	}
	return d, nil
}

func checkInfo(value any) error {
	info, isObject := value.(map[string]any)
	if !isObject {
		return errors.New("the decisions file needs an info object")
	}
	if err := members(info, "info", "title", "version", "description"); err != nil {
		return err
	}
	for _, name := range []string{"title", "version"} {
		if text, _ := info[name].(string); text == "" {
			return fmt.Errorf("info needs a %s", name)
		}
	}
	if _, isText := info["description"].(string); info["description"] != nil && !isText {
		return errors.New("info description must be text")
	}
	return nil
}

func parseAction(value any) (action, error) {
	object, isObject := value.(map[string]any)
	if !isObject {
		return action{}, errors.New("an action must be an object")
	}
	for _, name := range []string{"remove", "copy"} {
		if _, present := object[name]; present {
			return action{}, fmt.Errorf("%s actions are not accepted: a decision never deletes or copies settings", name)
		}
	}
	if err := members(object, "an action", "target", "description", "update"); err != nil {
		return action{}, err
	}
	if _, isText := object["description"].(string); object["description"] != nil && !isText {
		return action{}, errors.New("description must be text")
	}
	update, present := object["update"]
	if !present {
		return action{}, errors.New("an action needs an update")
	}
	text, _ := object["target"].(string)
	t, err := parseTarget(text)
	if err != nil {
		return action{}, err
	}
	return action{text: text, target: t, update: update}, nil
}

// members refuses any member of object not in allowed.
func members(object map[string]any, where string, allowed ...string) error {
	for name := range object {
		if !slices.Contains(allowed, name) {
			return fmt.Errorf("%s has the member %.64q, which is not accepted", where, name)
		}
	}
	return nil
}
