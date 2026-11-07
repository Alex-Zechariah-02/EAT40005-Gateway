// Tests of the path primitives: matching, conversion to gateway targets, and path transforms.

package semantics

import (
	"slices"
	"strings"
	"testing"

	"github.com/Alex-Zechariah-02/apiward/internal/exchange"
)

func TestPathMatches(t *testing.T) {
	cases := []struct {
		rule PathRule
		yes  []string
		no   []string
	}{
		{PathRule{Exact, "/a"}, []string{"/a"}, []string{"/a/", "/a/b", "/ab"}},
		{PathRule{ElementPrefix, "/a"}, []string{"/a", "/a/", "/a/b/c"}, []string{"/ab", "/"}},
		{PathRule{ElementPrefix, "/a/"}, []string{"/a", "/a/b"}, []string{"/ab"}},
		{PathRule{StringPrefix, "/a"}, []string{"/a", "/ab", "/a/b"}, []string{"/b"}},
		{PathRule{DeeperOnly, "/a/*"}, []string{"/a/b", "/a/b/c"}, []string{"/a", "/a/", "/ab"}},
		{PathRule{LastSegmentTemplate, "/a/{id}"}, []string{"/a/1"}, []string{"/a", "/a/", "/a/1/2"}},
		{PathRule{Regex, "/a/[0-9]+"}, []string{"/a/12"}, []string{"/a/x", "/b/a/12", "/a/12/x"}},
	}
	for _, c := range cases {
		for _, path := range c.yes {
			if !c.rule.Matches(path) {
				t.Errorf("%+v does not match %s", c.rule, path)
			}
		}
		for _, path := range c.no {
			if c.rule.Matches(path) {
				t.Errorf("%+v matches %s", c.rule, path)
			}
		}
	}
}

func TestPathConvert(t *testing.T) {
	cases := []struct {
		rule      PathRule
		targets   []string
		class     exchange.Class
		direction exchange.Direction
	}{
		{PathRule{Exact, "/a"}, []string{"/a"}, exchange.Exact, ""},
		{PathRule{DeeperOnly, "/a/*"}, []string{"/a/*"}, exchange.Exact, ""},
		{PathRule{ElementPrefix, "/a"}, []string{"/a", "/a/*"}, exchange.Approximated, exchange.Stricter},
		{PathRule{StringPrefix, "/a"}, []string{"/a", "/a/*"}, exchange.Approximated, exchange.Stricter},
		{PathRule{StringPrefix, "/a/"}, []string{"/a/*"}, exchange.Approximated, exchange.Stricter},
		{PathRule{ElementPrefix, "/"}, []string{"/", "/*"}, exchange.Equivalent, ""},
		{PathRule{LastSegmentTemplate, "/a/{id}"}, []string{"/a/*"}, exchange.Approximated, exchange.Looser},
		{PathRule{LastSegmentTemplate, "/a/{id}/b"}, nil, exchange.NotExpressible, ""},
		{PathRule{Regex, "/a/.*"}, nil, exchange.NotExpressible, ""},
	}
	for _, c := range cases {
		targets, distance := c.rule.Convert()
		if !slices.Equal(targets, c.targets) || distance.Class != c.class || distance.Direction != c.direction || distance.Note == "" && c.class != exchange.Exact {
			t.Errorf("%+v: %v %+v", c.rule, targets, distance)
		}
		for _, target := range targets {
			converted := PathRule{Exact, target}
			if prefix, ok := strings.CutSuffix(target, "*"); ok {
				converted = PathRule{DeeperOnly, prefix + "*"}
			}
			// Stricter and exact conversions never match a path the source refuses.
			for _, path := range []string{"/a", "/a/", "/a/b", "/ab", "/a/b/c", "/"} {
				if c.direction != exchange.Looser && converted.Matches(path) && !c.rule.Matches(path) {
					t.Errorf("%+v: target %s matches %s, which the source refuses", c.rule, target, path)
				}
			}
		}
	}
}

func TestTransform(t *testing.T) {
	cases := []struct {
		transform Transform
		in, out   string
		class     exchange.Class
	}{
		{Transform{Kind: Unchanged}, "/a/b", "/a/b", exchange.Exact},
		{Transform{Kind: StripPrefix, Prefix: "/api"}, "/api/orders", "/orders", exchange.NotExpressible},
		{Transform{Kind: StripPrefix, Prefix: "/api"}, "/api", "/", exchange.NotExpressible},
		{Transform{Kind: Replace, Prefix: "/v1", With: "/internal"}, "/v1/x", "/internal/x", exchange.NotExpressible},
		{Transform{Kind: Replace, Prefix: "/v1", With: "/internal"}, "/v2/x", "/v2/x", exchange.NotExpressible},
	}
	for _, c := range cases {
		if got := c.transform.Apply(c.in); got != c.out {
			t.Errorf("%+v(%s) = %s, want %s", c.transform, c.in, got, c.out)
		}
		if distance := c.transform.Convert(); distance.Class != c.class {
			t.Errorf("%+v: %+v", c.transform, distance)
		}
	}
}
