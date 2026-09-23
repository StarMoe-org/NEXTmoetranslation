package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"syscall"
)

const (
	Version     = "1.0.0"
	ProgramName = "lyricsctl"
)

// SubcommandHandler executes a subcommand with the provided arguments and streams.
type SubcommandHandler func(ctx context.Context, args []string, stdout, stderr io.Writer) error

// Command represents a subcommand supported by lyricsctl. Flags are owned by
// the delegate binary and forwarded verbatim; lyricsctl does not mirror them.
type Command struct {
	Name        string
	BinaryName  string
	Description string
	Handler     SubcommandHandler
}

// CLI holds the configuration and registered subcommands for lyricsctl.
type CLI struct {
	commands map[string]*Command
}

// NewDefaultCLI creates a CLI instance with all standard subcommands registered.
func NewDefaultCLI() *CLI {
	cli := &CLI{
		commands: make(map[string]*Command),
	}

	cli.Register(&Command{
		Name:        "preflight",
		BinaryName:  "lyrics-preflight",
		Description: "Perform source discovery, catalog verification, preflight checks, and candidate report generation",
	})

	cli.Register(&Command{
		Name:        "stage",
		BinaryName:  "lyrics-stage",
		Description: "Fetch fixed candidate revisions from preflight report and assemble staging manifest",
	})

	cli.Register(&Command{
		Name:        "import-stage",
		BinaryName:  "lyrics-import-stage",
		Description: "Commit staged lyrics into the local database with backup, receipt, and audit logging",
	})

	cli.Register(&Command{
		Name:        "validate",
		BinaryName:  "lyrics-validate",
		Description: "Validate staging manifest self-consistency, schema integrity, and batch hash",
	})

	cli.Register(&Command{
		Name:        "catalog-filter",
		BinaryName:  "lyrics-catalog-filter",
		Description: "Filter catalog database against target mapping report and produce filtered catalog snapshot and receipt",
	})

	cli.Register(&Command{
		Name:        "candidate",
		BinaryName:  "lyrics-recovery-public-candidate",
		Description: "Generate strict Public v3 candidate bundle and optional v2 compatibility artifacts from recovery database",
	})

	cli.Register(&Command{
		Name:        "recovery",
		BinaryName:  "lyrics-recovery",
		Description: "Coordinate lyrics recovery phases, state provisioning, replay, and migration",
	})

	return cli
}

// Register adds a command to the CLI registry.
func (cli *CLI) Register(cmd *Command) {
	if cmd.Handler == nil && cmd.BinaryName != "" {
		binaryName := cmd.BinaryName
		cmd.Handler = func(ctx context.Context, args []string, stdout, stderr io.Writer) error {
			return executeBinaryDelegate(ctx, binaryName, args, stdout, stderr)
		}
	}
	cli.commands[cmd.Name] = cmd
}

// FindCommand retrieves a command by name.
func (cli *CLI) FindCommand(name string) *Command {
	return cli.commands[name]
}

// Commands returns all registered commands sorted by name.
func (cli *CLI) Commands() []*Command {
	list := make([]*Command, 0, len(cli.commands))
	for _, cmd := range cli.commands {
		list = append(list, cmd)
	}
	sort.Slice(list, func(i, j int) bool {
		return list[i].Name < list[j].Name
	})
	return list
}

// Dispatch routes the command-line invocation to the proper subcommand handler.
func (cli *CLI) Dispatch(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		cli.PrintUsage(stderr)
		return errors.New("no subcommand specified; run 'lyricsctl help' for usage")
	}

	cmdName := args[0]

	// Handle version
	if cmdName == "version" || cmdName == "--version" || cmdName == "-v" {
		fmt.Fprintf(stdout, "%s version %s\n", ProgramName, Version)
		return nil
	}

	// Handle help
	if cmdName == "help" || cmdName == "--help" || cmdName == "-h" {
		if len(args) > 1 {
			targetName := args[1]
			targetCmd := cli.FindCommand(targetName)
			if targetCmd != nil {
				cli.PrintCommandHelp(stdout, targetCmd)
				return nil
			}
			fmt.Fprintf(stderr, "unknown subcommand %q for help\n\n", targetName)
			cli.PrintUsage(stderr)
			return fmt.Errorf("unknown subcommand %q", targetName)
		}
		cli.PrintUsage(stdout)
		return nil
	}

	cmd := cli.FindCommand(cmdName)
	if cmd == nil {
		fmt.Fprintf(stderr, "unknown subcommand %q\n\n", cmdName)
		cli.PrintUsage(stderr)
		return fmt.Errorf("unknown subcommand %q; run 'lyricsctl help' for available commands", cmdName)
	}

	subArgs := args[1:]

	if cmd.Handler == nil {
		return fmt.Errorf("subcommand %q has no registered handler", cmd.Name)
	}

	return cmd.Handler(ctx, subArgs, stdout, stderr)
}

// PrintUsage outputs the root usage guide listing all subcommands.
func (cli *CLI) PrintUsage(w io.Writer) {
	fmt.Fprintf(w, "Usage: %s <subcommand> [flags]\n\n", ProgramName)
	fmt.Fprintf(w, "Unified management CLI for lyrics preflight, staging, import, verification, and recovery.\n\n")
	fmt.Fprintf(w, "Available Subcommands:\n")

	for _, cmd := range cli.Commands() {
		fmt.Fprintf(w, "  %-16s %s\n", cmd.Name, cmd.Description)
	}
	fmt.Fprintf(w, "  %-16s %s\n", "version", "Print version information")
	fmt.Fprintf(w, "  %-16s %s\n", "help", "Show help for lyricsctl or a specific subcommand")

	fmt.Fprintf(w, "\nRun '%s help <subcommand>' or '%s <subcommand> --help' for details on a specific subcommand.\n", ProgramName, ProgramName)
}

// PrintCommandHelp outputs the summary for one subcommand. The delegate binary
// owns the flag definitions, so flag help comes from `lyricsctl <sub> -h`.
func (cli *CLI) PrintCommandHelp(w io.Writer, cmd *Command) {
	fmt.Fprintf(w, "Usage: %s %s [flags]\n\n", ProgramName, cmd.Name)
	fmt.Fprintf(w, "%s\n\n", cmd.Description)
	fmt.Fprintf(w, "Flags are defined and validated by the %s delegate; run '%s %s -h' to list them.\n",
		cmd.BinaryName, ProgramName, cmd.Name)
}

func executeBinaryDelegate(ctx context.Context, binaryName string, args []string, stdout, stderr io.Writer) error {
	path, err := resolveBinaryPath(binaryName)
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.Stdin = os.Stdin
	return cmd.Run()
}

func resolveBinaryPath(binaryName string) (string, error) {
	// Look in directory of current running executable first
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		candidate := filepath.Join(dir, binaryName)
		if isExecutableFile(candidate) {
			return candidate, nil
		}
	}
	// Look in PATH
	if path, err := exec.LookPath(binaryName); err == nil {
		return path, nil
	}
	return "", fmt.Errorf("subcommand delegate binary %q not found in executable directory or PATH", binaryName)
}

func isExecutableFile(path string) bool {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return false
	}
	return info.Mode()&0111 != 0
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cli := NewDefaultCLI()
	if err := cli.Dispatch(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		os.Exit(1)
	}
}
