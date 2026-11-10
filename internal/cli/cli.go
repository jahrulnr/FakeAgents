// Package cli implements the fakeagents command line interface.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"

	"github.com/jahrulnr/fakeagents/internal/app"
	"github.com/jahrulnr/fakeagents/internal/config"
	"github.com/jahrulnr/fakeagents/internal/httpapi"
	"github.com/jahrulnr/fakeagents/internal/scenario"
	"github.com/jahrulnr/fakeagents/internal/version"
)

const usageText = `FakeAgents - deterministic OpenAI and Anthropic API simulator

Usage:
  fakeagents [command] [flags]

Commands:
  serve        Start the API server (default)
  validate     Validate configuration and templates
  init         Write a starter configuration file
  scenarios    List configured scenarios and triggers
  config       Print the resolved configuration (secrets redacted)
  version      Print version information
  help         Show this help

Serve flags:
  -c, --config PATH        Configuration file (JSON or YAML)
      --host HOST          Listen address override
      --port PORT          Listen port override (0 picks a free port)
      --log-level LEVEL    debug, info, warn, error
      --log-format FORMAT  text or json
      --scenario NAME      Force a scenario for every request
      --require-header N   Require a header (repeatable)
      --strict             Reject models that are not in the catalog
      --no-latency         Disable simulated latency
      --print-config       Print the resolved configuration and exit
  -h, --help               Show help

Environment:
  FAKEAGENTS_CONFIG, FAKEAGENTS_HOST, FAKEAGENTS_PORT, FAKEAGENTS_LOG_LEVEL,
  FAKEAGENTS_LOG_FORMAT, FAKEAGENTS_SCENARIO, FAKEAGENTS_AUTH_MODE,
  FAKEAGENTS_REQUIRE_HEADERS

Examples:
  fakeagents init
  fakeagents serve -c fakeagents.json
  fakeagents serve -c fakeagents.json --require-header x-opencode-session
  fakeagents validate -c configs/fakeagents.yaml
  fakeagents scenarios -c fakeagents.json
`

// Run executes the CLI and returns a process exit code.
func Run(args []string) int {
	if len(args) == 0 {
		return serve(nil)
	}
	command := args[0]
	switch command {
	case "help", "-h", "--help":
		fmt.Print(usageText)
		return 0
	case "version", "--version", "-v":
		fmt.Println(version.String())
		return 0
	case "serve":
		return serve(args[1:])
	case "validate":
		return validate(args[1:])
	case "init":
		return initConfig(args[1:])
	case "scenarios":
		return listScenarios(args[1:])
	case "config":
		return printConfig(args[1:])
	default:
		if strings.HasPrefix(command, "-") {
			return serve(args)
		}
		fmt.Fprintf(os.Stderr, "fakeagents: unknown command %q\n\n%s", command, usageText)
		return 2
	}
}

type serveOptions struct {
	configShort   string
	configLong    string
	host          string
	port          int
	logLevel      string
	logFormat     string
	scenario      string
	requireHeader stringList
	strict        bool
	noLatency     bool
	printConfig   bool
}

func serve(args []string) int {
	options := serveOptions{port: -1}
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.StringVar(&options.configShort, "c", "", "configuration file")
	fs.StringVar(&options.configLong, "config", "", "configuration file")
	fs.StringVar(&options.host, "host", "", "listen address")
	fs.IntVar(&options.port, "port", -1, "listen port")
	fs.StringVar(&options.logLevel, "log-level", "", "log level")
	fs.StringVar(&options.logFormat, "log-format", "", "log format")
	fs.StringVar(&options.scenario, "scenario", "", "force a scenario")
	fs.Var(&options.requireHeader, "require-header", "require a header (repeatable)")
	fs.BoolVar(&options.strict, "strict", false, "strict model validation")
	fs.BoolVar(&options.noLatency, "no-latency", false, "disable simulated latency")
	fs.BoolVar(&options.printConfig, "print-config", false, "print resolved configuration and exit")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	path := firstNonEmpty(options.configLong, options.configShort, os.Getenv("FAKEAGENTS_CONFIG"))
	cfg, err := config.Load(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fakeagents: %v\n", err)
		return 2
	}
	applyServeOverrides(cfg, options)
	if err := cfg.Validate(); err != nil {
		fmt.Fprintf(os.Stderr, "fakeagents: %v\n", err)
		return 2
	}
	if options.printConfig {
		return writeJSON(cfg.Redacted())
	}

	application, err := app.New(cfg, buildInfo())
	if err != nil {
		fmt.Fprintf(os.Stderr, "fakeagents: %v\n", err)
		return 2
	}
	defer application.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := application.Run(ctx); err != nil {
		application.Logger.Error("server stopped", "error", err)
		return 1
	}
	return 0
}

func applyServeOverrides(cfg *config.Config, options serveOptions) {
	if options.host != "" {
		cfg.Server.Host = options.host
	} else if value := os.Getenv("FAKEAGENTS_HOST"); value != "" {
		cfg.Server.Host = value
	}
	if options.port >= 0 {
		cfg.Server.Port = options.port
	} else if value := os.Getenv("FAKEAGENTS_PORT"); value != "" {
		if port, err := strconv.Atoi(value); err == nil {
			cfg.Server.Port = port
		}
	}
	if options.logLevel != "" {
		cfg.Logging.Level = options.logLevel
	} else if value := os.Getenv("FAKEAGENTS_LOG_LEVEL"); value != "" {
		cfg.Logging.Level = value
	}
	if options.logFormat != "" {
		cfg.Logging.Format = options.logFormat
	} else if value := os.Getenv("FAKEAGENTS_LOG_FORMAT"); value != "" {
		cfg.Logging.Format = value
	}
	if options.scenario != "" {
		cfg.Defaults.Scenario = options.scenario
	} else if value := os.Getenv("FAKEAGENTS_SCENARIO"); value != "" {
		cfg.Defaults.Scenario = value
	}
	if value := os.Getenv("FAKEAGENTS_AUTH_MODE"); value != "" {
		cfg.Auth.Mode = value
	}
	if options.strict {
		cfg.Defaults.StrictModels = true
	}
	if options.noLatency {
		disabled := false
		cfg.Defaults.Latency.Enabled = &disabled
	}
	for _, name := range options.requireHeader {
		cfg.RequiredHeaders = append(cfg.RequiredHeaders, config.HeaderRule{
			Name:    strings.TrimSpace(name),
			Status:  400,
			Message: "Missing required header: " + strings.TrimSpace(name),
		})
	}
	if value := os.Getenv("FAKEAGENTS_REQUIRE_HEADERS"); value != "" {
		for name := range strings.SplitSeq(value, ",") {
			name = strings.TrimSpace(name)
			if name == "" {
				continue
			}
			cfg.RequiredHeaders = append(cfg.RequiredHeaders, config.HeaderRule{
				Name:    name,
				Status:  400,
				Message: "Missing required header: " + name,
			})
		}
	}
}

func validate(args []string) int {
	fs := flag.NewFlagSet("validate", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var short, long string
	fs.StringVar(&short, "c", "", "configuration file")
	fs.StringVar(&long, "config", "", "configuration file")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	path := firstNonEmpty(long, short, os.Getenv("FAKEAGENTS_CONFIG"))
	cfg, err := config.Load(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fakeagents: %v\n", err)
		return 2
	}
	application, err := app.New(cfg, buildInfo())
	if err != nil {
		fmt.Fprintf(os.Stderr, "fakeagents: %v\n", err)
		return 2
	}
	defer application.Close()
	fmt.Printf("configuration OK: %d models, %d scenarios, %d required header rules\n",
		len(cfg.Models), len(cfg.Scenarios), len(cfg.RequiredHeaders))
	return 0
}

func initConfig(args []string) int {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var output, format string
	var force bool
	fs.StringVar(&output, "o", "fakeagents.json", "output file")
	fs.StringVar(&output, "output", "fakeagents.json", "output file")
	fs.StringVar(&format, "format", "", "json or yaml")
	fs.BoolVar(&force, "force", false, "overwrite an existing file")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if !force {
		if _, err := os.Stat(output); err == nil {
			fmt.Fprintf(os.Stderr, "fakeagents: %s already exists (use --force to overwrite)\n", output)
			return 2
		}
	}
	if format == "" {
		switch strings.ToLower(strings.TrimPrefix(filepath.Ext(output), ".")) {
		case "yaml", "yml":
			format = "yaml"
		default:
			format = "json"
		}
	}
	cfg := config.Default()
	cfg.Schema = "docs/config.schema.json"
	if err := config.Save(output, cfg, format); err != nil {
		fmt.Fprintf(os.Stderr, "fakeagents: %v\n", err)
		return 1
	}
	fmt.Printf("wrote %s\n\nNext steps:\n  fakeagents validate -c %s\n  fakeagents serve -c %s\n", output, output, output)
	return 0
}

func listScenarios(args []string) int {
	fs := flag.NewFlagSet("scenarios", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var short, long string
	fs.StringVar(&short, "c", "", "configuration file")
	fs.StringVar(&long, "config", "", "configuration file")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	path := firstNonEmpty(long, short, os.Getenv("FAKEAGENTS_CONFIG"))
	cfg, err := config.Load(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fakeagents: %v\n", err)
		return 2
	}
	set, err := scenario.Compile(cfg.Scenarios)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fakeagents: %v\n", err)
		return 2
	}
	writer := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(writer, "NAME\tTYPE\tMATCH\tTRIGGERS")
	for _, compiled := range set.All() {
		name := compiled.Name()
		if strings.EqualFold(cfg.Defaults.Scenario, name) {
			name += " *"
		}
		match := "named only"
		if hasMatch(compiled) {
			match = "automatic"
		}
		triggers := append([]string{compiled.Name()}, compiled.Spec.Match.Triggers...)
		fmt.Fprintf(writer, "%s\t%s\t%s\t%s\n", name, compiled.Type, match, strings.Join(uniqueStrings(triggers), ", "))
	}
	_ = writer.Flush()
	fmt.Println("\n* forced by defaults.scenario")
	return 0
}

func printConfig(args []string) int {
	fs := flag.NewFlagSet("config", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var short, long string
	fs.StringVar(&short, "c", "", "configuration file")
	fs.StringVar(&long, "config", "", "configuration file")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	path := firstNonEmpty(long, short, os.Getenv("FAKEAGENTS_CONFIG"))
	cfg, err := config.Load(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fakeagents: %v\n", err)
		return 2
	}
	return writeJSON(cfg.Redacted())
}

func writeJSON(value any) int {
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		fmt.Fprintf(os.Stderr, "fakeagents: %v\n", err)
		return 1
	}
	return 0
}

func buildInfo() httpapi.BuildInfo {
	return httpapi.BuildInfo{
		Version: version.Version,
		Commit:  version.Commit,
		Date:    version.Date,
		Go:      runtime.Version(),
	}
}

func hasMatch(compiled *scenario.Compiled) bool {
	return compiled.HasMatchCriteria()
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]bool, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		key := strings.ToLower(value)
		if value == "" || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, value)
	}
	return out
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }

func (s *stringList) Set(value string) error {
	*s = append(*s, value)
	return nil
}
