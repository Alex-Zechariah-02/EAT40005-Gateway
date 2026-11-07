// The policy import-openapi and export-openapi commands: flags, bounded file reads, and output that never overwrites.

package openapi

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/Alex-Zechariah-02/apiward/internal/policy"
)

// Synopses of the two commands.
const (
	importUsage = `gateway policy import-openapi --spec <file.json|.yaml|.yml> --api <id> --upstream <id>
  --base <policy.json> [--max-body-bytes <n>] [--with-permits] --out <new file> --report <new file>`
	exportUsage = `gateway policy export-openapi --policy <policy.json> --api <id>
  --out <new file.json|.yaml|.yml> --report <new file> [--with-upstream]`
)

// Subcommand is one command: its flags and what it runs once they parse.
type Subcommand struct {
	Usage    string
	Required []string
	Optional map[string]string // flag → default
	Switches []string
	Run      func(values map[string]string, stderr io.Writer) (int, error)
}

var commands = map[string]Subcommand{
	"import-openapi": {importUsage, []string{"spec", "api", "upstream", "base", "out", "report"},
		map[string]string{"max-body-bytes": strconv.Itoa(DefaultBodyBytes)}, []string{"with-permits"}, runImport},
	"export-openapi": {exportUsage, []string{"policy", "api", "out", "report"}, nil, []string{"with-upstream"}, runExport},
}

// Command runs "policy <subcommand>" and returns the exit status. It reads
// only the files named by flags and creates only the out and report files.
func Command(args []string, stderr io.Writer) int {
	return Run(commands, args, stderr)
}

// Run runs the subcommand of the table that args name and returns the exit
// status; an unknown one, or a bad command line, prints the usage.
func Run(table map[string]Subcommand, args []string, stderr io.Writer) int {
	var c Subcommand
	known := len(args) > 0
	if known {
		c, known = table[args[0]]
	}
	if !known {
		var usages []string
		for _, name := range slices.Sorted(maps.Keys(table)) {
			usages = append(usages, table[name].Usage)
		}
		fmt.Fprintln(stderr, "usage: "+strings.Join(usages, "\n       "))
		return ExitFailed
	}
	values, err := c.parse(args[1:])
	if err != nil {
		fmt.Fprintln(stderr, "gateway:", err)
		if errors.Is(err, ErrUsage) {
			fmt.Fprintln(stderr, "usage: "+c.Usage)
		}
		return ExitFailed
	}
	code, err := c.Run(values, stderr)
	if err != nil {
		fmt.Fprintln(stderr, "gateway:", err)
		if errors.Is(err, ErrUsage) {
			fmt.Fprintln(stderr, "usage: "+c.Usage)
		}
		return ExitFailed
	}
	return code
}

// ErrUsage marks an error that the command line itself caused.
var ErrUsage = errors.New("invalid command line")

// single is a flag value that may be given only once.
type single struct {
	value string
	set   bool
}

func (f *single) String() string { return f.value }

func (f *single) Set(value string) error {
	if f.set {
		return errors.New("given more than once")
	}
	f.value, f.set = value, true
	return nil
}

// toggle is a switch that may be given only once.
type toggle struct{ single }

func (f *toggle) IsBoolFlag() bool { return true }

func (c Subcommand) parse(args []string) (map[string]string, error) {
	set := flag.NewFlagSet("gateway", flag.ContinueOnError)
	set.SetOutput(io.Discard)
	values := map[string]string{}
	for _, name := range c.Required {
		set.Var(new(single), name, name)
	}
	for name, value := range c.Optional {
		values[name] = value
		set.Var(new(single), name, name)
	}
	for _, name := range c.Switches {
		set.Var(new(toggle), name, name)
	}
	if err := set.Parse(args); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUsage, err)
	}
	if set.NArg() != 0 {
		return nil, ErrUsage
	}
	set.Visit(func(f *flag.Flag) { values[f.Name] = f.Value.String() })
	for _, name := range c.Required {
		if values[name] == "" {
			return nil, fmt.Errorf("%w: --%s is required", ErrUsage, name)
		}
	}
	return values, nil
}

func runImport(values map[string]string, stderr io.Writer) (int, error) {
	format, err := FormatOf(values["spec"])
	if err != nil {
		return ExitFailed, err
	}
	maxBody, err := strconv.ParseInt(values["max-body-bytes"], 10, 64)
	if err != nil {
		return ExitFailed, fmt.Errorf("max-body-bytes must be 1 to %d", policy.MaxBodyLimit)
	}
	spec, err := ReadLimited(values["spec"], MaxSpecBytes, "spec")
	if err != nil {
		return ExitFailed, err
	}
	base, err := ReadPolicy(values["base"], "base")
	if err != nil {
		return ExitFailed, err
	}
	result, report, err := Import(spec, format, Options{API: values["api"], Upstream: values["upstream"], Base: base,
		MaxBodyBytes: maxBody, WithPermits: values["with-permits"] == "true"})
	if err != nil {
		return ExitFailed, err
	}
	if err := Write(values["out"], result, values["report"], report); err != nil {
		return ExitFailed, err
	}
	fmt.Fprintf(stderr, "imported %d of %d operations (%d widened, %d refused); see %s\n",
		len(report.Mapped)+len(report.Widened), report.OperationsInSpec, len(report.Widened), len(report.Refused), values["report"])
	return report.Exit(), nil
}

func runExport(values map[string]string, stderr io.Writer) (int, error) {
	format, err := FormatOf(values["out"])
	if err != nil {
		return ExitFailed, err
	}
	source, err := ReadPolicy(values["policy"], "policy")
	if err != nil {
		return ExitFailed, err
	}
	document, report, err := Export(source, ExportOptions{API: values["api"], WithUpstream: values["with-upstream"] == "true"})
	if err != nil {
		return ExitFailed, err
	}
	data, err := Encode(document, format)
	if err != nil {
		return ExitFailed, err
	}
	if err := Write(values["out"], json.RawMessage(data), values["report"], report); err != nil {
		return ExitFailed, err
	}
	fmt.Fprintf(stderr, "exported %d of %d operations (%d changed); see %s\n",
		len(report.Exported), report.Operations, len(report.Changed), values["report"])
	return report.Exit(), nil
}

// ReadPolicy reads and strictly decodes a policy file.
func ReadPolicy(path, name string) (policy.Policy, error) {
	document, err := ReadLimited(path, policy.MaxPolicyBytes, name)
	if err != nil {
		return policy.Policy{}, err
	}
	p, err := policy.DecodePolicy(document)
	if err != nil {
		return policy.Policy{}, fmt.Errorf("%s policy refused: %w", name, err)
	}
	return p, nil
}

// Write encodes the output and the report as 2-space JSON and writes both
// new files, or neither. Raw output is written as given.
func Write(outPath string, output any, reportPath string, report any) error {
	if outPath == reportPath {
		return errors.New("the output and the report must be different files")
	}
	files := map[string][]byte{}
	for path, value := range map[string]any{outPath: output, reportPath: report} {
		if raw, ok := value.(json.RawMessage); ok {
			files[path] = raw
			continue
		}
		data, err := indent(value)
		if err != nil {
			return err
		}
		files[path] = data
	}
	return writeNew(files)
}

// WriteOne writes one new file as 2-space JSON, never over an existing one.
func WriteOne(path string, value any) error {
	data, err := indent(value)
	if err != nil {
		return err
	}
	return writeNew(map[string][]byte{path: data})
}

// indent is the written form of a JSON output: 2-space indent, final newline.
func indent(value any) ([]byte, error) {
	data, err := json.MarshalIndent(value, "", "  ")
	return append(data, '\n'), err
}

// ReadLimited reads at most limit bytes and refuses a longer file.
func ReadLimited(path string, limit int64, name string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("%s file: %w", name, err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, fmt.Errorf("%s file: %w", name, err)
	}
	if int64(len(data)) > limit {
		if limit%(1<<20) == 0 {
			return nil, fmt.Errorf("the %s file is larger than %d MiB", name, limit>>20)
		}
		return nil, fmt.Errorf("the %s file is larger than %d bytes", name, limit)
	}
	return data, nil
}

// writeNew creates every file exclusively before writing any, and removes
// the files it created if any step fails, so no existing file is changed
// and a failure leaves no partial output.
func writeNew(files map[string][]byte) error {
	opened := map[string]*os.File{}
	var failure error
	for path := range files {
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, fs.ErrExist) {
			failure = fmt.Errorf("%s already exists; choose a new file", path)
			break
		}
		if err != nil {
			failure = err
			break
		}
		opened[path] = file
	}
	for path, file := range opened {
		if failure == nil {
			_, failure = file.Write(files[path])
		}
		if err := file.Sync(); failure == nil {
			failure = err
		}
		if err := file.Close(); failure == nil {
			failure = err
		}
	}
	if failure != nil {
		for path := range opened {
			os.Remove(path)
		}
	}
	return failure
}
