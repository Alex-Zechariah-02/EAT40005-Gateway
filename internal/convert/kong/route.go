// Services and routes: which routes can be expressed, and the operations each path and method becomes.

package kong

import (
	"cmp"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/Alex-Zechariah-02/apiward/internal/exchange"
	"github.com/Alex-Zechariah-02/apiward/internal/semantics"
)

// service is what a route needs from its service.
type service struct {
	name, pointer, path string
	refusal             string // code and message refusing every route, or ""
	serves              bool   // an imported operation came from one of its routes
	plugins             []*plugin
}

func (r *reader) readService(s map[string]any, pointer string) {
	svc := &service{name: cmp.Or(text(s["name"]), text(s["id"]), pointer), pointer: pointer}
	for _, key := range []string{text(s["name"]), text(s["id"])} {
		if key != "" {
			r.services[key] = svc
		}
	}
	svc.path = text(s["path"])
	if address, err := url.Parse(text(s["url"])); err == nil && text(s["url"]) != "" {
		svc.path = address.Path
	}
	switch member := unknownMember(s, serviceAddress, serviceFolded, serviceOther); {
	case member != "":
		svc.refusal = CodeUnknown + ": the service member " + member + " is not known."
	case s["enabled"] == false:
		svc.refusal = CodeNotImported + ": the service is disabled, so Kong serves none of its routes."
	}
	r.carry("service:"+svc.name, pointer, without(s, "routes", "plugins"))
	for _, name := range sortedKeys(s) {
		if known(serviceOther, name) {
			r.add("service:"+svc.name+":"+name, "other", pointer+"/"+name, "", semantics.Distance{
				Class: exchange.NotExpressible, Note: "Kept for the way back to Kong; the gateway does not apply it."})
		}
	}
	for i, p := range list(s["plugins"]) {
		svc.plugins = append(svc.plugins, r.newPlugin(object(p), pointer+"/plugins/"+strconv.Itoa(i), "service."+svc.name))
	}
	for _, key := range []string{text(s["name"]), text(s["id"])} {
		svc.plugins = append(svc.plugins, r.scoped["service:"+key]...)
	}
	r.ordered = append(r.ordered, svc)
	for i, route := range list(s["routes"]) {
		r.readRoute(svc, object(route), pointer+"/routes/"+strconv.Itoa(i))
	}
}

// serviceItem binds a service that serves imported operations to the
// API's upstream; it is written after every route, root-level ones too.
func (r *reader) serviceItem(svc *service) {
	switch {
	case svc.refusal != "":
		r.add("service:"+svc.name, "upstream", svc.pointer, "", semantics.Distance{Class: exchange.NotExpressible, Note: svc.refusal})
	case !svc.serves:
		r.refuse("service:"+svc.name, "upstream", svc.pointer, CodeNotImported, "none of its routes is imported.")
	default:
		r.add("service:"+svc.name, "upstream", svc.pointer, "#", semantics.Distance{Class: exchange.Equivalent,
			Note: "The API is bound to the upstream named by --upstream; the address is kept for the way back to Kong."})
	}
}

// topRoute reads a root-level route, which names its service.
func (r *reader) topRoute(route map[string]any, pointer string) {
	svc := r.services[reference(route["service"])]
	if svc == nil {
		svc = &service{refusal: CodeNotImported + ": the route names no service in the file."}
	}
	r.readRoute(svc, route, pointer)
}

func (r *reader) readRoute(svc *service, route map[string]any, pointer string) {
	name := cmp.Or(text(route["name"]), text(route["id"]), pointer)
	r.carry("route:"+name, pointer, without(route, "plugins"))
	for _, member := range sortedKeys(route) {
		if known(routeOther, member) {
			r.add("route:"+name+":"+member, "other", pointer+"/"+member, "", semantics.Distance{
				Class: exchange.NotExpressible, Note: "Kept for the way back to Kong; the gateway does not apply it."})
		}
	}
	plugins := slices.Clone(svc.plugins)
	for i, p := range list(route["plugins"]) {
		plugins = append(plugins, r.newPlugin(object(p), pointer+"/plugins/"+strconv.Itoa(i), "route."+name))
	}
	for _, key := range []string{text(route["name"]), text(route["id"])} {
		plugins = append(plugins, r.scoped["route:"+key]...)
	}
	if refusal := routeRefusal(svc, route); refusal != "" {
		r.add("route:"+name, "route", pointer, "", semantics.Distance{Class: exchange.NotExpressible, Note: refusal})
		return
	}
	c, refused := r.controls(effective(r.global, plugins))
	if refused != nil {
		r.refuse("route:"+name, "route", pointer, refused.code, refused.message)
		r.causes[refused.source] = refused.code + ": " + refused.message
		return
	}
	before := r.count
	r.operations(name, pointer, route, c)
	svc.serves = svc.serves || r.count > before
}

// routeRefusal returns why a route cannot be expressed, or "".
func routeRefusal(svc *service, route map[string]any) string {
	if svc.refusal != "" {
		return svc.refusal
	}
	if member := unknownMember(route, routeFolded, routeMatch, routeOther); member != "" {
		return CodeUnknown + ": the route member " + member + " is not known."
	}
	for _, member := range routeMatch {
		if present(route[member]) {
			return CodeMatch + ": routes selected by " + member + " cannot be expressed yet."
		}
	}
	if protocols := strings.Join(texts(route["protocols"]), ","); route["protocols"] != nil && protocols != "http,https" && protocols != "https,http" {
		return CodeMatch + ": only routes for both http and https can be expressed (the listener decides TLS)."
	}
	for _, path := range texts(route["paths"]) {
		_, _, anchored := anchoredTarget(path)
		if strings.HasPrefix(path, "~") && !anchored {
			return CodeMatch + ": regular-expression paths cannot be expressed."
		}
		strip := route["strip_path"] != false // Kong's default is true
		if (strip && path != "/" && !anchored) || (svc.path != "" && svc.path != "/") {
			return CodeTransform + ": Kong forwards " + path + " under a different path; the gateway forwards the path unchanged."
		}
	}
	return ""
}

// operations writes one operation per target of each path and method,
// with its items.
func (r *reader) operations(name, pointer string, route map[string]any, c *controls) {
	paths := texts(route["paths"])
	if len(paths) == 0 {
		paths = []string{"/"} // a route without paths takes every path
	}
	routeMethods, methodNote := texts(route["methods"]), ""
	if len(routeMethods) == 0 {
		routeMethods, methodNote = methods, " Kong takes any method here; the gateway refuses TRACE, CONNECT and others."
	}
	index := 0
	for _, path := range paths {
		rule := semantics.PathRule{Match: semantics.StringPrefix, Value: path}
		if target, match, anchored := anchoredTarget(path); anchored {
			rule = semantics.PathRule{Match: match, Value: target}
		}
		targets, d := rule.Convert()
		if methodNote != "" && d.Class != exchange.Approximated {
			d = semantics.Distance{Class: exchange.Approximated, Direction: exchange.Stricter}
		}
		d.Note = strings.TrimSpace(d.Note + methodNote)
		for _, target := range targets {
			for _, method := range routeMethods {
				r.operation(name, pointer, strings.ToUpper(method), target, index, d, c)
			}
			index++
		}
	}
}

func (r *reader) operation(name, pointer, method, target string, index int, d semantics.Distance, c *controls) {
	path, prefix := strings.CutSuffix(target, "*")
	if prefix {
		path += "{rest}"
	}
	id := "operation:" + name + ":" + method + " " + path
	if !slices.Contains(methods, method) {
		r.refuse(id, "route", pointer, CodeMatch, "The gateway never forwards "+method+".")
		return
	}
	item := object(r.paths[path])
	if item[strings.ToLower(method)] != nil {
		r.refuse(id, "route", pointer, CodeCollision, "Another route already takes "+method+" "+path+"; Kong's precedence cannot be expressed.")
		return
	}
	operationID := name + "." + strings.ToLower(method)
	if index > 0 {
		operationID = name + "-" + strconv.Itoa(index) + "." + strings.ToLower(method)
	}
	op := c.describe(operationID, method, target, prefix)
	if item == nil {
		item = map[string]any{}
		r.paths[path] = item
	}
	item[strings.ToLower(method)] = op
	r.count++
	r.signedIn = r.signedIn || c.auth
	at := "#/paths/" + escape(path) + "/" + strings.ToLower(method)
	r.add(id, "route", pointer, at, d)
	for _, e := range c.items {
		if e.bodyOnly && !slices.Contains(bodyMethods, method) {
			continue
		}
		r.add(id+":"+e.suffix, e.facet, e.source, at, e.d)
	}
	r.add(id+":body", "validation", pointer, at, c.bodyDistance(method))
}

// escape encodes one JSON pointer segment (RFC 6901).
func escape(segment string) string {
	return strings.NewReplacer("~", "~0", "/", "~1").Replace(segment)
}

// anchoredTarget reads the two regular-expression paths the writer emits:
// ~<literal>$ (exact) and ~<literal>/.+ (deeper paths only). Any other
// regular expression is not one of them.
func anchoredTarget(path string) (string, semantics.Match, bool) {
	pattern, isRegex := strings.CutPrefix(path, "~")
	if !isRegex {
		return "", "", false
	}
	if body, exact := strings.CutSuffix(pattern, "$"); exact {
		literal, ok := unquote(body)
		return literal, semantics.Exact, ok && strings.HasPrefix(literal, "/")
	}
	if body, deeper := strings.CutSuffix(pattern, ".+"); deeper {
		literal, ok := unquote(body)
		return literal + "*", semantics.DeeperOnly, ok && strings.HasPrefix(literal, "/") && strings.HasSuffix(literal, "/")
	}
	return "", "", false
}

// unquote reverses regexp.QuoteMeta: it accepts escaped metacharacters
// and refuses any unescaped one, so only a literal path passes.
func unquote(pattern string) (string, bool) {
	const meta = `\.+*?()|[]{}^$`
	const backslash = '\\'
	var b strings.Builder
	for i := 0; i < len(pattern); i++ {
		c := pattern[i]
		switch {
		case c == backslash && i+1 < len(pattern) && strings.IndexByte(meta, pattern[i+1]) >= 0:
			i++
			b.WriteByte(pattern[i])
		case strings.IndexByte(meta, c) >= 0:
			return "", false
		default:
			b.WriteByte(c)
		}
	}
	return b.String(), true
}
