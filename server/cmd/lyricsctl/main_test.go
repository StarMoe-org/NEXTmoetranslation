package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestDispatchNoArguments(t *testing.T) {
	cli := NewDefaultCLI()
	var stdout, stderr bytes.Buffer

	err := cli.Dispatch(context.Background(), []string{}, &stdout, &stderr)
	if err == nil {
		t.Fatal("expected error when no arguments are passed")
	}
	if !strings.Contains(err.Error(), "no subcommand specified") {
		t.Fatalf("unexpected error message: %v", err)
	}
	if !strings.Contains(stderr.String(), "Usage: lyricsctl <subcommand>") {
		t.Fatalf("expected stderr to contain usage, got: %s", stderr.String())
	}
}

func TestDispatchVersion(t *testing.T) {
	cli := NewDefaultCLI()
	tests := [][]string{
		{"version"},
		{"--version"},
		{"-v"},
	}

	for _, testArgs := range tests {
		t.Run(strings.Join(testArgs, "_"), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			err := cli.Dispatch(context.Background(), testArgs, &stdout, &stderr)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			expected := fmt.Sprintf("lyricsctl version %s\n", Version)
			if stdout.String() != expected {
				t.Fatalf("expected stdout %q, got %q", expected, stdout.String())
			}
		})
	}
}

func TestDispatchHelpRoot(t *testing.T) {
	cli := NewDefaultCLI()
	tests := [][]string{
		{"help"},
		{"--help"},
		{"-h"},
	}

	for _, testArgs := range tests {
		t.Run(strings.Join(testArgs, "_"), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			err := cli.Dispatch(context.Background(), testArgs, &stdout, &stderr)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			output := stdout.String()
			for _, sub := range []string{"preflight", "stage", "import-stage", "validate", "catalog-filter", "candidate", "recovery", "version", "help"} {
				if !strings.Contains(output, sub) {
					t.Errorf("expected usage output to mention subcommand %q", sub)
				}
			}
		})
	}
}

func TestDispatchSubcommandHelp(t *testing.T) {
	cli := NewDefaultCLI()
	subcommands := []string{"preflight", "stage", "import-stage", "validate", "catalog-filter", "candidate", "recovery"}

	for _, sub := range subcommands {
		t.Run(sub, func(t *testing.T) {
			var forwarded [][]string
			if err := cli.SetHandler(sub, func(_ context.Context, args []string, _, _ io.Writer) error {
				forwarded = append(forwarded, append([]string(nil), args...))
				return nil
			}); err != nil {
				t.Fatal(err)
			}

			// 'lyricsctl help <subcommand>' is answered by lyricsctl itself.
			var stdout, stderr bytes.Buffer
			if err := cli.Dispatch(context.Background(), []string{"help", sub}, &stdout, &stderr); err != nil {
				t.Fatalf("help %s error: %v", sub, err)
			}
			binary := cli.FindCommand(sub).BinaryName
			if !strings.Contains(stdout.String(), "Usage: lyricsctl "+sub) || !strings.Contains(stdout.String(), binary) {
				t.Fatalf("help %s expected usage header naming %s, got: %s", sub, binary, stdout.String())
			}
			if len(forwarded) != 0 {
				t.Fatalf("help %s must not invoke the delegate, got %v", sub, forwarded)
			}

			// Help flags on the subcommand belong to the delegate and are forwarded untouched.
			for _, flag := range []string{"--help", "-h", "-help"} {
				if err := cli.Dispatch(context.Background(), []string{sub, flag}, &stdout, &stderr); err != nil {
					t.Fatalf("%s %s error: %v", sub, flag, err)
				}
			}
			want := [][]string{{"--help"}, {"-h"}, {"-help"}}
			if !reflect.DeepEqual(forwarded, want) {
				t.Fatalf("%s help flags forwarded=%v want %v", sub, forwarded, want)
			}
		})
	}
}

func TestDispatchHelpUnknownSubcommand(t *testing.T) {
	cli := NewDefaultCLI()
	var stdout, stderr bytes.Buffer

	err := cli.Dispatch(context.Background(), []string{"help", "unknown-command"}, &stdout, &stderr)
	if err == nil {
		t.Fatal("expected error for help on unknown subcommand")
	}
	if !strings.Contains(err.Error(), "unknown subcommand \"unknown-command\"") {
		t.Fatalf("unexpected error message: %v", err)
	}
	if !strings.Contains(stderr.String(), "unknown subcommand \"unknown-command\" for help") {
		t.Fatalf("expected stderr to contain error notice, got: %s", stderr.String())
	}
}

func TestDispatchUnknownSubcommand(t *testing.T) {
	cli := NewDefaultCLI()
	var stdout, stderr bytes.Buffer

	err := cli.Dispatch(context.Background(), []string{"foobar"}, &stdout, &stderr)
	if err == nil {
		t.Fatal("expected error for unknown subcommand")
	}
	if !strings.Contains(err.Error(), "unknown subcommand \"foobar\"") {
		t.Fatalf("unexpected error message: %v", err)
	}
	if !strings.Contains(stderr.String(), "unknown subcommand \"foobar\"") {
		t.Fatalf("expected stderr to report unknown subcommand, got: %s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "Available Subcommands:") {
		t.Fatalf("expected stderr to include available subcommands list")
	}
}

// lyricsctl used to pre-validate arguments against a hand-copied flag table,
// so spellings that only the delegate knows (for example lyrics-recovery's
// -expected-plan-sha256 or lyrics-catalog-filter's -output) were rejected
// before the delegate ever ran. Arguments now reach the delegate verbatim.
func TestDispatchForwardsDelegateOnlyFlagsVerbatim(t *testing.T) {
	cli := NewDefaultCLI()
	cases := map[string][]string{
		"recovery":       {"-mode", "check", "-expected-plan-sha256", strings.Repeat("a", 64), "-live-canary-authorization", "token"},
		"catalog-filter": {"-source-catalog", "/tmp/catalog.db", "-expected-source-catalog-sha256", strings.Repeat("b", 64), "-output", "/tmp/out"},
		"validate":       {"-unrecognized-flag-xyz=123"},
	}
	for sub, args := range cases {
		t.Run(sub, func(t *testing.T) {
			var got []string
			if err := cli.SetHandler(sub, func(_ context.Context, forwarded []string, _, _ io.Writer) error {
				got = append([]string(nil), forwarded...)
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			var stdout, stderr bytes.Buffer
			if err := cli.Dispatch(context.Background(), append([]string{sub}, args...), &stdout, &stderr); err != nil {
				t.Fatalf("dispatch %s: %v (stderr=%s)", sub, err, stderr.String())
			}
			if !reflect.DeepEqual(got, args) {
				t.Fatalf("%s forwarded=%v want %v", sub, got, args)
			}
		})
	}
}

func TestSubcommandsDispatchToRegisteredHandlers(t *testing.T) {
	cli := NewDefaultCLI()
	dispatched := make(map[string][]string)

	subcommands := map[string][]string{
		"preflight": {
			"-db", "/tmp/test.db",
			"-output", "/tmp/preflight.json",
			"-concurrency", "8",
			"-max-attempts", "4",
			"-resume-unique-complete",
		},
		"stage": {
			"-report", "/tmp/preflight.json",
			"-db", "/tmp/test.db",
			"-output", "/tmp/stage.json",
			"-evidence-receipt-output", "/tmp/evidence.json",
			"-concurrency", "2",
		},
		"import-stage": {
			"-manifest", "/tmp/stage.json",
			"-db", "/tmp/test.db",
			"-backup", "/tmp/backup.db",
			"-backup-sha256", "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
			"-receipt", "/tmp/receipt.json",
			"-operator", "test-operator",
			"-confirm-local-offline",
		},
		"validate": {
			"-input", "/tmp/stage.json",
		},
		"catalog-filter": {
			"-source-catalog", "/tmp/catalog.db",
			"-source-catalog-sha256", "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
			"-target-map", "/tmp/target.json",
			"-target-map-sha256", "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
			"-output-root", "/tmp/output",
		},
		"candidate": {
			"-database", "/tmp/recovery.db",
			"-batch-sha256", "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
			"-output-directory", "/tmp/v3-candidate",
			"-v2-compat-output-directory", "/tmp/v2-candidate",
		},
		"recovery": {
			"-mode", "check",
			"-plan", "/tmp/plan.json",
			"-plan-sha256", "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
			"-catalog", "/tmp/catalog.db",
			"-catalog-count", "100",
			"-live-canary-token", "token-abc",
		},
	}

	for name, args := range subcommands {
		cmdName := name
		expectedArgs := args
		err := cli.SetHandler(cmdName, func(ctx context.Context, passedArgs []string, stdout, stderr io.Writer) error {
			dispatched[cmdName] = passedArgs
			fmt.Fprintf(stdout, "handler executed for %s\n", cmdName)
			return nil
		})
		if err != nil {
			t.Fatalf("failed to set handler for %s: %v", cmdName, err)
		}

		var stdout, stderr bytes.Buffer
		fullArgs := append([]string{cmdName}, expectedArgs...)
		err = cli.Dispatch(context.Background(), fullArgs, &stdout, &stderr)
		if err != nil {
			t.Fatalf("dispatch %s failed: %v", cmdName, err)
		}
		if !strings.Contains(stdout.String(), "handler executed for "+cmdName) {
			t.Fatalf("expected stdout from handler for %s, got: %s", cmdName, stdout.String())
		}
		if len(dispatched[cmdName]) != len(expectedArgs) {
			t.Fatalf("expected %d args passed to handler %s, got %d", len(expectedArgs), cmdName, len(dispatched[cmdName]))
		}
		for i, a := range expectedArgs {
			if dispatched[cmdName][i] != a {
				t.Errorf("%s arg[%d]: expected %q, got %q", cmdName, i, a, dispatched[cmdName][i])
			}
		}
	}
}

func TestSetHandlerUnknownCommand(t *testing.T) {
	cli := NewDefaultCLI()
	err := cli.SetHandler("nonexistent", func(ctx context.Context, args []string, stdout, stderr io.Writer) error {
		return nil
	})
	if err == nil {
		t.Fatal("expected error setting handler on nonexistent subcommand")
	}
}

func TestBinaryResolution(t *testing.T) {
	// Test resolving a binary that definitely does not exist
	_, err := resolveBinaryPath("nonexistent-test-binary-12345")
	if err == nil {
		t.Fatal("expected error resolving nonexistent binary")
	}
	if !strings.Contains(err.Error(), "not found in executable directory or PATH") {
		t.Fatalf("unexpected error message: %v", err)
	}

	// Test resolving a standard utility in PATH
	shPath, err := resolveBinaryPath("sh")
	if err != nil {
		t.Fatalf("expected to resolve 'sh' from PATH, got error: %v", err)
	}
	if shPath == "" {
		t.Fatal("resolved empty path for 'sh'")
	}
}

func TestDefaultDelegateWhenBinaryMissing(t *testing.T) {
	cli := NewDefaultCLI()
	// Set binary name to something that doesn't exist
	cli.Register(&Command{
		Name:        "test-missing-binary",
		BinaryName:  "definitely-nonexistent-binary-99999",
		Description: "test command with missing binary",
	})

	var stdout, stderr bytes.Buffer
	err := cli.Dispatch(context.Background(), []string{"test-missing-binary"}, &stdout, &stderr)
	if err == nil {
		t.Fatal("expected error when delegate binary is missing")
	}
	if !strings.Contains(err.Error(), "not found in executable directory or PATH") {
		t.Fatalf("unexpected error message: %v", err)
	}
}

func TestSubcommandHandlerErrorPropagation(t *testing.T) {
	cli := NewDefaultCLI()
	expectedErr := errors.New("custom handler failure")
	cli.Register(&Command{
		Name:        "failing-cmd",
		Description: "command that always fails",
		Handler: func(ctx context.Context, args []string, stdout, stderr io.Writer) error {
			return expectedErr
		},
	})

	var stdout, stderr bytes.Buffer
	err := cli.Dispatch(context.Background(), []string{"failing-cmd"}, &stdout, &stderr)
	if !errors.Is(err, expectedErr) {
		t.Fatalf("expected error %v, got %v", expectedErr, err)
	}
}

func TestExecutableIntegration(t *testing.T) {
	tempDir := t.TempDir()
	binaryPath := filepath.Join(tempDir, "lyricsctl")

	for target, out := range map[string]string{".": binaryPath, "../lyrics-validate": filepath.Join(tempDir, "lyrics-validate")} {
		buildCmd := exec.Command("go", "build", "-o", out, target)
		buildCmd.Env = os.Environ()
		buildCmd.Dir = "."
		if output, err := buildCmd.CombinedOutput(); err != nil {
			t.Fatalf("failed to build %s: %v\nOutput: %s", target, err, string(output))
		}
	}

	// Test 1: Run version
	cmd := exec.Command(binaryPath, "version")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("lyricsctl version failed: %v, output: %s", err, string(out))
	}
	if !strings.Contains(string(out), "lyricsctl version "+Version) {
		t.Fatalf("unexpected version output: %s", string(out))
	}

	// Test 2: Run help
	cmd = exec.Command(binaryPath, "help")
	out, err = cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("lyricsctl help failed: %v, output: %s", err, string(out))
	}
	if !strings.Contains(string(out), "Usage: lyricsctl <subcommand>") {
		t.Fatalf("unexpected help output: %s", string(out))
	}

	// Test 3: Run subcommand help
	cmd = exec.Command(binaryPath, "help", "preflight")
	out, err = cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("lyricsctl help preflight failed: %v, output: %s", err, string(out))
	}
	if !strings.Contains(string(out), "Usage: lyricsctl preflight") {
		t.Fatalf("unexpected preflight help output: %s", string(out))
	}

	// Test 4: Unknown subcommand exits with non-zero code
	cmd = exec.Command(binaryPath, "unknown-subcmd")
	out, err = cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected failure for unknown subcommand")
	}
	if !strings.Contains(string(out), "unknown subcommand \"unknown-subcmd\"") {
		t.Fatalf("unexpected unknown subcommand output: %s", string(out))
	}

	// Test 5: The delegate, not lyricsctl, rejects an invalid flag.
	cmd = exec.Command(binaryPath, "validate", "-nonexistent-flag")
	out, err = cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected failure for invalid flag")
	}
	if !strings.Contains(string(out), "lyrics validate:") || strings.Contains(string(out), "delegate binary") {
		t.Fatalf("expected lyrics-validate to reject the flag, got: %s", string(out))
	}
}
