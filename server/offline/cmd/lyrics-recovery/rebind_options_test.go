package main

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestParseOptionsOfflineRebindSourcesAreExplicitAndDisjoint(t *testing.T) {
	arguments := rebindOptionArguments(t)
	parsed, err := parseOptions(arguments)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.mode != "rebind" || parsed.rebindSourceLedgerPath == "" ||
		parsed.rebindSourceAcquisitionSetPath == "" || parsed.rebindSourceLedgerPath == parsed.ledgerPath ||
		parsed.rebindSourceAcquisitionSetPath == parsed.acquisitionSetPath {
		t.Fatalf("parsed offline rebind options=%+v", parsed)
	}
	supplementArguments := append(append([]string(nil), arguments...),
		"-rebind-supplement-ledger", "/private/tmp/moesekai-rebind-options-test/supplement/ledger",
		"-rebind-supplement-acquisition-set", "/private/tmp/moesekai-rebind-options-test/supplement/acquisition-set.json",
	)
	supplemented, err := parseOptions(supplementArguments)
	if err != nil || supplemented.rebindSupplementLedgerPath == "" ||
		supplemented.rebindSupplementAcquisitionSetPath == "" {
		t.Fatalf("parsed supplemented rebind options=%+v err=%v", supplemented, err)
	}
	if _, err := parseOptions(removeOptionPair(
		append([]string(nil), supplementArguments...), "-rebind-supplement-acquisition-set",
	)); err == nil {
		t.Fatal("partial rebind supplement was accepted")
	}

	for name, mutate := range map[string]func([]string) []string{
		"missing source set": func(values []string) []string {
			return removeOptionPair(values, "-rebind-source-acquisition-set")
		},
		"source aliases output": func(values []string) []string {
			return replaceOptionValue(values, "-rebind-source-ledger", optionValue(values, "-ledger"))
		},
		"sources outside rebind": func(values []string) []string {
			return replaceOptionValue(values, "-mode", "check")
		},
		"supplement outside rebind": func(values []string) []string {
			values = replaceOptionValue(values, "-mode", "check")
			return append(values,
				"-rebind-supplement-ledger", "/private/tmp/moesekai-rebind-options-test/supplement/ledger",
				"-rebind-supplement-acquisition-set", "/private/tmp/moesekai-rebind-options-test/supplement/acquisition-set.json",
			)
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseOptions(mutate(append([]string(nil), arguments...))); err == nil {
				t.Fatal("invalid offline rebind options were accepted")
			}
		})
	}
}

func rebindOptionArguments(t *testing.T) []string {
	t.Helper()
	root := filepath.Join("/private/tmp", "moesekai-rebind-options-test")
	outputs := filepath.Join(root, "outputs")
	return []string{
		"-mode", "rebind",
		"-plan", filepath.Join(root, "plan.json"),
		"-expected-plan-sha256", strings.Repeat("a", 64),
		"-source-root", filepath.Join(root, "source"),
		"-catalog", filepath.Join(root, "catalog.db"),
		"-expected-catalog-sha256", strings.Repeat("b", 64),
		"-expected-catalog-count", "698",
		"-expected-catalog-music-ids-sha256", strings.Repeat("c", 64),
		"-ledger", filepath.Join(outputs, "ledger"),
		"-acquisition-set", filepath.Join(outputs, "acquisition-set.json"),
		"-provider-outcomes", filepath.Join(outputs, "provider-outcomes"),
		"-song-results", filepath.Join(outputs, "song-results"),
		"-evidence-pack", filepath.Join(outputs, "evidence-pack"),
		"-root-manifest", filepath.Join(outputs, "root.json"),
		"-rebind-source-ledger", filepath.Join(root, "historical", "ledger"),
		"-rebind-source-acquisition-set", filepath.Join(root, "historical", "acquisition-set.json"),
		"-sekaipedia-list-replay-ledger", filepath.Join(root, "list", "ledger"),
		"-sekaipedia-list-replay-acquisition-id", strings.Repeat("d", 64),
	}
}

func optionValue(arguments []string, name string) string {
	for index := 0; index+1 < len(arguments); index += 2 {
		if arguments[index] == name {
			return arguments[index+1]
		}
	}
	return ""
}

func replaceOptionValue(arguments []string, name, value string) []string {
	for index := 0; index+1 < len(arguments); index += 2 {
		if arguments[index] == name {
			arguments[index+1] = value
			return arguments
		}
	}
	return arguments
}

func removeOptionPair(arguments []string, name string) []string {
	for index := 0; index+1 < len(arguments); index += 2 {
		if arguments[index] == name {
			return append(arguments[:index], arguments[index+2:]...)
		}
	}
	return arguments
}
