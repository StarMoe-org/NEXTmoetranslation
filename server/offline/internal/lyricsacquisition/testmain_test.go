package lyricsacquisition

import (
	"fmt"
	"os"
	"testing"
)

func TestMain(main *testing.M) {
	if crashRoot := os.Getenv(metadataCrashHelperEnvironment); crashRoot != "" {
		privateTemp := fmt.Sprintf("%s.helper-tmp-%d", crashRoot, os.Getpid())
		if err := os.Mkdir(privateTemp, 0o700); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		if err := os.Setenv("TMPDIR", privateTemp); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		// The crash helper deliberately exits from inside the test. Its temp
		// directory is therefore placed beside the ledger, underneath the
		// parent test's t.TempDir tree, instead of leaking a /private/tmp root.
		os.Exit(main.Run())
	}

	privateTemp, err := os.MkdirTemp("/private/tmp", "lyricsacquisition-tests-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if err := os.Chmod(privateTemp, 0o700); err != nil {
		fmt.Fprintln(os.Stderr, err)
		_ = os.RemoveAll(privateTemp)
		os.Exit(2)
	}
	if err := os.Setenv("TMPDIR", privateTemp); err != nil {
		fmt.Fprintln(os.Stderr, err)
		_ = os.RemoveAll(privateTemp)
		os.Exit(2)
	}
	code := main.Run()
	if err := os.RemoveAll(privateTemp); err != nil && code == 0 {
		fmt.Fprintln(os.Stderr, err)
		code = 2
	}
	os.Exit(code)
}
