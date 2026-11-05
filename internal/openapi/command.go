// The policy import-openapi command: flags, bounded file reads, and output that never overwrites.

package openapi

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strconv"

	"github.com/Alex-Zechariah-02/apiward/internal/policy"
)

// usage is the command's synopsis.
const usage = `gateway policy import-openapi --spec <file.json|.yaml|.yml> --api <id> --upstream <id>
  --base <policy.json> [--max-body-bytes <n>] --out <new file> --report <new file>`

// Command runs "policy <subcommand>" and returns the exit status. It reads
// only the spec and base files and creates only the out and report files.
func Command(args []string, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "import-openapi" {
		fmt.Fprintln(stderr, "usage: "+usage)
		return ExitFailed
	}
	values, err := parseFlags(args[1:])
	if err != nil {
		fmt.Fprintln(stderr, "gateway:", err)
		if errors.Is(err, errUsage) {
			fmt.Fprintln(stderr, "usage: "+usage)
		}
		return ExitFailed
	}
	report, err := run(values)
	if err != nil {
		fmt.Fprintln(stderr, "gateway:", err)
		return ExitFailed
	}
	fmt.Fprintf(stderr, "imported %d of %d operations (%d widened, %d refused); see %s\n",
		len(report.Mapped)+len(report.Widened), report.OperationsInSpec, len(report.Widened), len(report.Refused), values["report"])
	return report.Exit()
}

var errUsage = errors.New("invalid command line")

var required = []string{"spec", "api", "upstream", "base", "out", "report"}

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

func parseFlags(args []string) (map[string]string, error) {
	set := flag.NewFlagSet("import-openapi", flag.ContinueOnError)
	set.SetOutput(io.Discard)
	parsed := map[string]*single{}
	for _, name := range append(required, "max-body-bytes") {
		parsed[name] = new(single)
		set.Var(parsed[name], name, name)
	}
	if err := set.Parse(args); err != nil {
		return nil, fmt.Errorf("%w: %v", errUsage, err)
	}
	if set.NArg() != 0 {
		return nil, errUsage
	}
	values := map[string]string{"max-body-bytes": strconv.Itoa(DefaultBodyBytes)}
	for name, value := range parsed {
		if value.set {
			values[name] = value.value
		}
	}
	for _, name := range required {
		if values[name] == "" {
			return nil, fmt.Errorf("--%s is required", name)
		}
	}
	return values, nil
}

// run imports and writes both files, or writes neither.
func run(values map[string]string) (*Report, error) {
	format, err := FormatOf(values["spec"])
	if err != nil {
		return nil, err
	}
	maxBody, err := strconv.ParseInt(values["max-body-bytes"], 10, 64)
	if err != nil {
		return nil, fmt.Errorf("max-body-bytes must be 1 to %d", policy.MaxBodyLimit)
	}
	spec, err := readLimited(values["spec"], MaxSpecBytes, "spec")
	if err != nil {
		return nil, err
	}
	baseDocument, err := readLimited(values["base"], policy.MaxPolicyBytes, "base")
	if err != nil {
		return nil, err
	}
	base, err := policy.DecodePolicy(baseDocument)
	if err != nil {
		return nil, fmt.Errorf("base policy refused: %w", err)
	}
	result, report, err := Import(spec, format, Options{API: values["api"], Upstream: values["upstream"], Base: base, MaxBodyBytes: maxBody})
	if err != nil {
		return nil, err
	}
	document, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return nil, err
	}
	record, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return nil, err
	}
	return report, writeNew(map[string][]byte{values["out"]: append(document, '\n'), values["report"]: append(record, '\n')})
}

// readLimited reads at most limit bytes and refuses a longer file.
func readLimited(path string, limit int64, name string) ([]byte, error) {
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
		if name == "spec" {
			return nil, errors.New("the description is larger than 1 MiB")
		}
		return nil, fmt.Errorf("%s file is larger than %d bytes", name, limit)
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
