package model

import "testing"

func TestGachaInfoIsSupportedRightAfterGacha(t *testing.T) {
	if !IsValidCategory("gachaInfo") {
		t.Fatal("gachaInfo is not a supported category")
	}
	for i, category := range SupportedCategories {
		if category == "gacha" {
			if i+1 >= len(SupportedCategories) || SupportedCategories[i+1] != "gachaInfo" {
				t.Fatalf("gachaInfo should be listed right after gacha: %v", SupportedCategories)
			}
			return
		}
	}
	t.Fatalf("gacha missing from %v", SupportedCategories)
}

func TestOnlyGachaInfoIsRestoreOptional(t *testing.T) {
	for _, category := range SupportedCategories {
		if got, want := IsRestoreOptionalCategory(category), category == "gachaInfo"; got != want {
			t.Fatalf("IsRestoreOptionalCategory(%q) = %v, want %v", category, got, want)
		}
	}
}
