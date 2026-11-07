// YAML input: same result as JSON, refused constructs, limits and core-schema scalars.

package openapi

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestImportYAML(t *testing.T) {
	t.Run("same result as JSON", func(t *testing.T) {
		for _, name := range []string{"orders", "refusals"} {
			fromJSON, jsonReport, err := Import(fixture(t, name+".json"), JSON, testOptions())
			if err != nil {
				t.Fatal(err)
			}
			fromYAML, yamlReport, err := Import(fixture(t, name+".yaml"), YAML, testOptions())
			if err != nil {
				t.Fatal(err)
			}
			if jsonReport.SourceFormat != "json" || yamlReport.SourceFormat != "yaml" {
				t.Fatalf("%s: source formats %q and %q", name, jsonReport.SourceFormat, yamlReport.SourceFormat)
			}
			yamlReport.SourceFormat = jsonReport.SourceFormat
			for _, pair := range [][2]any{{fromJSON, fromYAML}, {jsonReport, yamlReport}} {
				a, _ := json.Marshal(pair[0])
				b, _ := json.Marshal(pair[1])
				if !bytes.Equal(a, b) {
					t.Fatalf("%s: JSON and YAML differ:\n%s\n%s", name, a, b)
				}
			}
		}
	})

	t.Run("refused constructs", func(t *testing.T) {
		cases := []struct{ name, spec, want string }{
			{"anchor and alias", "a: &x 1\nb: *x\n", "line 1: anchors"},
			{"merge key", "base: 1\nm:\n  <<: {p: 2}\n", "line 3: merge keys"},
			{"custom tag", "a: 1\nb: !custom 1\n", "line 2: tags"},
			{"standard tag", "b: !!str 2\n", "line 1: tags"},
			{"second document", "a: 1\n---\nb: 2\n", "line 3: more than one document"},
			{"duplicate key", "a: 1\na: 2\n", "line 2: duplicate key"},
			{"integer key", "paths: {}\n200: x\n", "line 2: key is not a string"},
			{"boolean key", "true: y\n", "line 1: key is not a string"},
			{"sequence key", "? [1]\n: z\n", "line 1: key is not a string"},
			{"not a number", "a: 1\nb: .nan\n", "line 2: .nan is not representable in JSON"},
			{"infinity", "b: -.Inf\n", "line 1: -.Inf is not representable in JSON"},
			{"too deep", "a: " + strings.Repeat("[", 16) + strings.Repeat("]", 16) + "\n", "line 1: nested deeper than 16"},
			{"long float", "a: ." + strings.Repeat("7", 32) + "\n", "line 1: number longer than 32 characters"},
			{"long octal", "a: 0o" + strings.Repeat("7", 31) + "\n", "line 1: number longer than 32 characters"},
			{"malformed", "a: [1\n", "not valid YAML"},
			{"empty", "", "empty"},
		}
		for _, c := range cases {
			_, err := Decode([]byte(c.spec), YAML, MaxSpecBytes)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("%s: error %v, want %q", c.name, err, c.want)
			}
		}
	})

	t.Run("size limit", func(t *testing.T) {
		big := "# " + strings.Repeat("x", MaxSpecBytes) + "\nopenapi: 3.1.0\n"
		_, _, err := Import([]byte(big), YAML, testOptions())
		if err == nil || !strings.Contains(err.Error(), "1 MiB") {
			t.Fatalf("error %v", err)
		}
	})

	t.Run("core schema scalars", func(t *testing.T) {
		spec := "a: yes\nb: 0777\nc: 0o17\nd: 0x1F\ne: 1.50\nf: +3\ng: ~\nh: Null\ni: TRUE\nj: 1_000\nk: .5\nl: '3'\nm: \"null\"\nn: 1e3\no: -0\np:\nq: |\n  text\n"
		got, err := Decode([]byte(spec), YAML, MaxSpecBytes)
		if err != nil {
			t.Fatal(err)
		}
		want := map[string]any{
			"a": "yes", "b": json.Number("777"), "c": json.Number("15"), "d": json.Number("31"),
			"e": json.Number("1.50"), "f": json.Number("3"), "g": nil, "h": nil, "i": true,
			"j": "1_000", "k": json.Number("0.5"), "l": "3", "m": "null", "n": json.Number("1e3"),
			"o": json.Number("-0"), "p": nil, "q": "text\n",
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %#v", got)
		}
	})

	t.Run("document markers", func(t *testing.T) {
		got, err := Decode([]byte("---\na: 1\n...\n"), YAML, MaxSpecBytes)
		if err != nil || !reflect.DeepEqual(got, map[string]any{"a": json.Number("1")}) {
			t.Fatalf("got %#v, %v", got, err)
		}
	})
}
