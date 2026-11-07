// The decK reader: root members, consumers and plugin scopes, into an exchange document with one ledger item per setting.

package kong

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strconv"

	"github.com/Alex-Zechariah-02/apiward/internal/exchange"
	"github.com/Alex-Zechariah-02/apiward/internal/semantics"
)

// Result is a decK file read into an exchange document and its ledger.
type Result struct {
	Document map[string]any
	Ledger   *exchange.Ledger
}

// reader holds what one Read has found so far.
type reader struct {
	ledger    *exchange.Ledger
	paths     map[string]any       // OpenAPI path → path item
	consumers map[string]*consumer // by username and by id
	scoped    map[string][]*plugin // "service:<ref>" or "route:<ref>" → root-level plugins
	global    []*plugin
	services  map[string]*service // by name and by id
	plugins   []*plugin           // every non-consumer plugin instance
	used      map[string]bool     // sources that have an item
	causes    map[string]string   // plugin pointer → why a route it applies to was refused
	signedIn  bool
	count     int        // operations written
	ordered   []*service // services in file order
}

// consumer is what the reader needs from a consumer: never a credential.
type consumer struct {
	name   string
	groups []string
}

// plugin is one plugin instance and where it applies; name is its kind,
// label its name in item IDs.
type plugin struct {
	name, label, pointer, scope string
	object, config              map[string]any
}

// Read classifies every setting of a decoded decK file. data is the file
// as read, for the ledger's hash. A file of another format version, or
// with defaults that change every entity, is refused.
func Read(root map[string]any, data []byte) (*Result, error) {
	if version, _ := root["_format_version"].(string); version != Version {
		return nil, fmt.Errorf("%s: _format_version must be %q (Kong 3.x decK files)", CodeVersion, Version)
	}
	if info, _ := root["_info"].(map[string]any); info["defaults"] != nil {
		return nil, errors.New("_info.defaults changes the defaults of every entity; write the values on the entities instead")
	}
	sum := sha256.Sum256(data)
	r := &reader{ledger: exchange.New(exchange.Source{Format: Format, FormatVersion: Version, SHA256: hex.EncodeToString(sum[:])}),
		paths: map[string]any{}, consumers: map[string]*consumer{}, scoped: map[string][]*plugin{},
		services: map[string]*service{}, used: map[string]bool{}, causes: map[string]string{}}
	r.readConsumers(list(root["consumers"]))
	r.rootPlugins(list(root["plugins"]))
	for i, s := range list(root["services"]) {
		r.readService(object(s), "#/services/"+strconv.Itoa(i))
	}
	for i, route := range list(root["routes"]) {
		r.topRoute(object(route), "#/routes/"+strconv.Itoa(i))
	}
	for _, svc := range r.ordered {
		r.serviceItem(svc)
	}
	r.unusedPlugins()
	r.rootMembers(root)
	slices.SortFunc(r.ledger.Items, func(a, b exchange.Item) int { return cmp.Compare(a.ID, b.ID) })
	slices.SortFunc(r.ledger.CredentialsSkipped, func(a, b exchange.Skipped) int { return cmp.Compare(a.Type, b.Type) })
	return &Result{Document: r.document(), Ledger: r.ledger}, nil
}

// add records one ledger item; source marks the setting as accounted for.
func (r *reader) add(id, facet, source, target string, d semantics.Distance) {
	r.used[source] = true
	r.ledger.Items = append(r.ledger.Items, exchange.Item{ID: Format + ":" + id, Facet: facet, Class: d.Class,
		Direction: d.Direction, Source: source, Target: target, Note: d.Note})
}

// refuse records a setting that is not expressible, its code first.
func (r *reader) refuse(id, facet, source, code, message string) {
	r.add(id, facet, source, "", semantics.Distance{Class: exchange.NotExpressible, Note: code + ": " + message})
}

// skip counts credentials of one type that were never read.
func (r *reader) skip(kind string, count int) {
	if count == 0 {
		return
	}
	for i := range r.ledger.CredentialsSkipped {
		if r.ledger.CredentialsSkipped[i].Type == kind {
			r.ledger.CredentialsSkipped[i].Count += count
			return
		}
	}
	r.ledger.CredentialsSkipped = append(r.ledger.CredentialsSkipped, exchange.Skipped{Type: kind, Count: count})
}

// readConsumers reads each consumer's name and groups and its RS256 issuer
// keys; credential values are never read, only counted.
func (r *reader) readConsumers(consumers []any) {
	for i, value := range consumers {
		c, pointer := object(value), "#/consumers/"+strconv.Itoa(i)
		name := cmp.Or(text(c["username"]), text(c["custom_id"]), text(c["id"]), strconv.Itoa(i))
		entry := &consumer{name: name}
		for _, acl := range list(c["acls"]) {
			if group := text(object(acl)["group"]); group != "" {
				entry.groups = append(entry.groups, group)
			}
		}
		for _, key := range []string{text(c["username"]), text(c["id"])} {
			if key != "" {
				r.consumers[key] = entry
			}
		}
		for _, kind := range credentialLists {
			r.skip(kind, len(list(c[kind])))
		}
		for j, secret := range list(c["jwt_secrets"]) {
			r.trustKey(object(secret), "consumer:"+name+":"+strconv.Itoa(j))
		}
		r.carry("consumer:"+name, pointer, identity(c))
		note := "Consumers and their credentials stay in the identity provider; they are not migrated."
		if member := unknownMember(c, consumerMembers); member != "" {
			note = CodeUnknown + ": the member " + member + " is not known. " + note
		}
		r.add("consumer:"+name, "auth", pointer, "", semantics.Distance{Class: exchange.NotExpressible, Note: note})
		for j, p := range list(c["plugins"]) {
			r.consumerPlugin(object(p), pointer+"/plugins/"+strconv.Itoa(j), "consumer."+name)
		}
	}
}

// identity is the part of a consumer that decides access on the way back
// to Kong: its names, groups and RS256 issuer keys, never a secret.
func identity(c map[string]any) map[string]any {
	out := map[string]any{}
	for _, name := range []string{"username", "custom_id", "id"} {
		if c[name] != nil {
			out[name] = c[name]
		}
	}
	var groups, keys []any
	for _, acl := range list(c["acls"]) {
		groups = append(groups, map[string]any{"group": object(acl)["group"]})
	}
	for _, secret := range list(c["jwt_secrets"]) {
		s := object(secret)
		if _, err := exchange.RSAPublicKey(text(s["rsa_public_key"])); err == nil && text(s["algorithm"]) == "RS256" {
			keys = append(keys, map[string]any{"key": s["key"], "algorithm": "RS256", "rsa_public_key": s["rsa_public_key"]})
		}
	}
	if groups != nil {
		out["acls"] = groups
	}
	if keys != nil {
		out["jwt_secrets"] = keys
	}
	return out
}

// trustKey adds an RS256 credential whose key is the issuer as a trusted
// issuer key. Only algorithm, key and the public key are read; secret never.
func (r *reader) trustKey(secret map[string]any, id string) {
	issuer, pem := text(secret["key"]), text(secret["rsa_public_key"])
	if text(secret["algorithm"]) != "RS256" || issuer == "" {
		return
	}
	if _, err := exchange.RSAPublicKey(pem); err != nil {
		return
	}
	issuers := &r.ledger.Trust.Issuers
	index := slices.IndexFunc(*issuers, func(i exchange.Issuer) bool { return i.Issuer == issuer })
	if index < 0 {
		*issuers = append(*issuers, exchange.Issuer{Issuer: issuer, PublicKeyIDs: []string{}, PublicKeys: []string{}})
		index = len(*issuers) - 1
	}
	(*issuers)[index].PublicKeyIDs = append((*issuers)[index].PublicKeyIDs, id)
	(*issuers)[index].PublicKeys = append((*issuers)[index].PublicKeys, pem)
}

func (r *reader) consumerPlugin(p map[string]any, pointer, scope string) {
	r.refuse("plugin:"+scope+":"+pluginName(p), "other", pointer, CodeConsumer,
		"A plugin for one consumer cannot be expressed; the gateway applies the same rules to every caller.")
}

// rootPlugins sorts the root plugin list by scope: global, a service, a
// route, or a consumer (refused).
func (r *reader) rootPlugins(plugins []any) {
	for i, value := range plugins {
		p, pointer := object(value), "#/plugins/"+strconv.Itoa(i)
		if p["consumer"] != nil || p["consumer_group"] != nil {
			r.consumerPlugin(p, pointer, "consumer."+cmp.Or(reference(p["consumer"]), reference(p["consumer_group"])))
			continue
		}
		entry := r.newPlugin(p, pointer, "global")
		switch {
		case p["route"] != nil:
			entry.scope = "route." + reference(p["route"])
			r.scoped["route:"+reference(p["route"])] = append(r.scoped["route:"+reference(p["route"])], entry)
		case p["service"] != nil:
			entry.scope = "service." + reference(p["service"])
			r.scoped["service:"+reference(p["service"])] = append(r.scoped["service:"+reference(p["service"])], entry)
		default:
			r.global = append(r.global, entry)
		}
	}
}

func (r *reader) newPlugin(p map[string]any, pointer, scope string) *plugin {
	entry := &plugin{name: text(p["name"]), label: pluginName(p), pointer: pointer, scope: scope, object: p, config: object(p["config"])}
	r.plugins = append(r.plugins, entry)
	r.carry("plugin:"+entry.label, pointer, p)
	return entry
}

// unusedPlugins gives an item to each plugin that no imported operation
// used, so every instance is accounted for.
func (r *reader) unusedPlugins() {
	for _, p := range r.plugins {
		if r.used[p.pointer] {
			continue
		}
		note := CodeNotImported + ": it applies to no imported operation."
		if _, isMapped := mapped[p.name]; !isMapped {
			note = "Carried for the way back to Kong; the gateway does not enforce it."
		}
		if cause := r.causes[p.pointer]; cause != "" {
			note = cause
		}
		r.add("plugin:"+p.scope+":"+p.label, "other", p.pointer, "", semantics.Distance{Class: exchange.NotExpressible, Note: note})
	}
}

// rootMembers accounts for the root members other than services, routes,
// plugins and consumers: carried, outside the API configuration, or unknown.
func (r *reader) rootMembers(root map[string]any) {
	for _, name := range sortedKeys(root) {
		switch {
		case name == "_format_version" || slices.Contains([]string{"services", "routes", "plugins", "consumers"}, name):
		case known(rootScope, name):
			r.carry("root", "#/"+name, root[name])
			r.add("member:"+name, "other", "#/"+name, "", semantics.Distance{Class: exchange.NotExpressible,
				Note: "Carried for the way back to Kong."})
		case known(rootOther, name):
			r.outOfScope(name, list(root[name]))
		default:
			r.refuse("member:"+name, "other", "#/"+name, CodeUnknown, "The root member is not a decK member.")
		}
	}
}

func (r *reader) outOfScope(name string, entries []any) {
	if name == "certificates" || name == "keys" {
		r.skip(name, len(entries))
	}
	for i, value := range entries {
		label := cmp.Or(text(object(value)["name"]), text(object(value)["id"]), strconv.Itoa(i))
		r.refuse(name+":"+label, "other", "#/"+name+"/"+strconv.Itoa(i), CodeOutOfScope,
			"Not part of the API configuration; not migrated.")
	}
}
