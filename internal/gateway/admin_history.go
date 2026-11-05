// Admin API change history reads and filters.

package gateway

import (
	"net/http"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/Alex-Zechariah-02/apiward/internal/policy"
)

// handleHistory lists recorded transitions, oldest first unless order=desc,
// optionally only those of the comma-separated kinds, the outcome or the
// actor given. Filters apply before the page limit, and the cursor is the
// last entry's seq in the chosen order.
func (c *Controller) handleHistory(w http.ResponseWriter, r *http.Request, who caller) {
	after, limit, ok := page(w, r, "order", "kind", "outcome", "actor")
	if !ok {
		return
	}
	cursor, ok := numericCursor(w, after)
	if !ok {
		return
	}
	filter, ok := historyFilterOf(r)
	if !ok {
		adminError(w, http.StatusBadRequest, "bad_request", "page")
		return
	}
	items, err := c.store.history(r.Context(), filter, cursor, limit)
	if err != nil {
		adminError(w, http.StatusServiceUnavailable, "unavailable", "")
		return
	}
	result := list{Items: items}
	if len(items) == limit {
		result.Next = strconv.FormatInt(items[len(items)-1].Seq, 10)
	}
	writeJSON(w, http.StatusOK, result)
}

// maxHistoryKinds bounds the kinds one history listing may select.
const maxHistoryKinds = 16

// historyFilterOf reads the history order and filters, which page has
// admitted as single parameters. A parameter that is present must be valid.
func historyFilterOf(r *http.Request) (historyFilter, bool) {
	query := r.URL.Query()
	var filter historyFilter
	if query.Has("order") {
		switch query.Get("order") {
		case "asc":
		case "desc":
			filter.newestFirst = true
		default:
			return filter, false
		}
	}
	if query.Has("kind") {
		filter.kinds = strings.Split(query.Get("kind"), ",")
		invalid := func(kind string) bool { return !historyKind(kind) }
		if len(filter.kinds) > maxHistoryKinds || slices.ContainsFunc(filter.kinds, invalid) {
			return filter, false
		}
	}
	if query.Has("outcome") {
		filter.outcome = query.Get("outcome")
		if filter.outcome != "committed" && filter.outcome != "rejected" {
			return filter, false
		}
	}
	if query.Has("actor") {
		filter.actor = query.Get("actor")
		if filter.actor == "" || len(filter.actor) > policy.MaxIDLength || !utf8.ValidString(filter.actor) {
			return filter, false
		}
	}
	return filter, true
}

// historyKind reports whether kind is shaped like a recorded transition
// kind, such as "cpa.confirm". An unknown but well-formed kind matches no
// entry.
func historyKind(kind string) bool {
	if kind == "" || len(kind) > policy.MaxIDLength {
		return false
	}
	for _, r := range kind {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '.' && r != '_' {
			return false
		}
	}
	return true
}

func (c *Controller) handleHistoryEntry(w http.ResponseWriter, r *http.Request, who caller) {
	entry, found, err := c.store.historyEntry(r.Context(), r.PathValue("id"))
	switch {
	case err != nil:
		adminError(w, http.StatusServiceUnavailable, "unavailable", "")
	case !found:
		adminError(w, http.StatusNotFound, "not_found", "")
	default:
		writeJSON(w, http.StatusOK, entry)
	}
}
