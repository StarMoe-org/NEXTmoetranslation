package translator

import (
	"fmt"

	"moesekai/server/internal/scenariolang"
)

// errNotJapaneseScenario tells why a scenario fetched as JP is not Japanese,
// or is nil (see package scenariolang for the rule).
func errNotJapaneseScenario(value any) error {
	if counts := scenariolang.CountScenario(value); !counts.Japanese() {
		return fmt.Errorf("not Japanese text (%v)", counts)
	}
	return nil
}

// errNotChineseScenario does the same for a scenario fetched as official CN.
// The CN mirrors serve the Japanese text of scenarios CN has listed but not
// yet translated.
func errNotChineseScenario(value any) error {
	if counts := scenariolang.CountScenario(value); !counts.Chinese() {
		return fmt.Errorf("not Chinese text (%v)", counts)
	}
	return nil
}
