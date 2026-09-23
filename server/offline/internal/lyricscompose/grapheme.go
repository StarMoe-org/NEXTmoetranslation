package lyricscompose

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// splitGraphemes implements the Unicode grapheme rules needed by source text:
// controls/CRLF, combining and spacing marks, Hangul syllables, prepend marks,
// emoji modifiers and ZWJ sequences, and regional-indicator pairs. Composition
// never normalizes text; every returned cluster preserves its exact bytes.
func splitGraphemes(value string) []string {
	if value == "" {
		return nil
	}
	runes := []rune(value)
	byteOffsets := make([]int, len(runes)+1)
	byteOffset := 0
	for index, current := range runes {
		byteOffsets[index] = byteOffset
		byteOffset += utf8.RuneLen(current)
	}
	byteOffsets[len(runes)] = len(value)

	boundaries := []int{0}
	for index := 1; index < len(runes); index++ {
		if graphemeBreak(runes, index) {
			boundaries = append(boundaries, byteOffsets[index])
		}
	}
	boundaries = append(boundaries, len(value))

	clusters := make([]string, 0, len(boundaries)-1)
	for index := 0; index+1 < len(boundaries); index++ {
		clusters = append(clusters, value[boundaries[index]:boundaries[index+1]])
	}
	return clusters
}

func graphemeBreak(runes []rune, index int) bool {
	left, right := runes[index-1], runes[index]

	// GB3.
	if left == '\r' && right == '\n' {
		return false
	}
	// GB4/GB5.
	if isGraphemeControl(left) || isGraphemeControl(right) {
		return true
	}
	// GB6-GB8.
	leftHangul, rightHangul := hangulClass(left), hangulClass(right)
	if leftHangul == hangulL && (rightHangul == hangulL || rightHangul == hangulV || rightHangul == hangulLV || rightHangul == hangulLVT) {
		return false
	}
	if (leftHangul == hangulLV || leftHangul == hangulV) && (rightHangul == hangulV || rightHangul == hangulT) {
		return false
	}
	if (leftHangul == hangulLVT || leftHangul == hangulT) && rightHangul == hangulT {
		return false
	}
	// GB9/GB9a/GB9b.
	if isGraphemeExtend(right) || right == '\u200d' || unicode.Is(unicode.Mc, right) {
		return false
	}
	if isGraphemePrepend(left) {
		return false
	}
	// GB11: Extended_Pictographic Extend* ZWJ × Extended_Pictographic.
	if isExtendedPictographic(right) && left == '\u200d' {
		cursor := index - 2
		for cursor >= 0 && isGraphemeExtend(runes[cursor]) {
			cursor--
		}
		if cursor >= 0 && isExtendedPictographic(runes[cursor]) {
			return false
		}
	}
	// GB12/GB13: pair regional indicators from the start of the run.
	if isRegionalIndicator(left) && isRegionalIndicator(right) {
		preceding := 0
		for cursor := index - 1; cursor >= 0 && isRegionalIndicator(runes[cursor]); cursor-- {
			preceding++
		}
		return preceding%2 == 0
	}
	return true
}

type hangulGraphemeClass uint8

const (
	hangulOther hangulGraphemeClass = iota
	hangulL
	hangulV
	hangulT
	hangulLV
	hangulLVT
)

func hangulClass(current rune) hangulGraphemeClass {
	switch {
	case current >= 0x1100 && current <= 0x115f || current >= 0xa960 && current <= 0xa97c:
		return hangulL
	case current >= 0x1160 && current <= 0x11a7 || current >= 0xd7b0 && current <= 0xd7c6:
		return hangulV
	case current >= 0x11a8 && current <= 0x11ff || current >= 0xd7cb && current <= 0xd7fb:
		return hangulT
	case current >= 0xac00 && current <= 0xd7a3:
		if (current-0xac00)%28 == 0 {
			return hangulLV
		}
		return hangulLVT
	default:
		return hangulOther
	}
}

func isGraphemeControl(current rune) bool {
	if current == '\u200c' || current == '\u200d' || current >= 0xe0020 && current <= 0xe007f {
		return false
	}
	return unicode.IsControl(current) || unicode.In(current, unicode.Cf, unicode.Zl, unicode.Zp)
}

func isGraphemeExtend(current rune) bool {
	return unicode.In(current, unicode.Mn, unicode.Me) || current == '\u200c' ||
		current >= 0xfe00 && current <= 0xfe0f || current >= 0xe0100 && current <= 0xe01ef ||
		current >= 0x1f3fb && current <= 0x1f3ff || current >= 0xe0020 && current <= 0xe007f
}

func isGraphemePrepend(current rune) bool {
	switch {
	case current >= 0x0600 && current <= 0x0605,
		current == 0x06dd,
		current == 0x070f,
		current == 0x0890 || current == 0x0891,
		current == 0x08e2,
		current == 0x0d4e,
		current == 0x110bd || current == 0x110cd,
		current >= 0x111c2 && current <= 0x111c3,
		current == 0x1193f || current == 0x11941,
		current == 0x11a3a,
		current >= 0x11a84 && current <= 0x11a89,
		current == 0x11d46:
		return true
	default:
		return false
	}
}

func isExtendedPictographic(current rune) bool {
	return current == 0x00a9 || current == 0x00ae || current == 0x203c || current == 0x2049 ||
		current == 0x2122 || current == 0x2139 || current >= 0x2194 && current <= 0x2199 ||
		current >= 0x21a9 && current <= 0x21aa || current >= 0x231a && current <= 0x231b ||
		current == 0x2328 || current == 0x23cf || current >= 0x23e9 && current <= 0x23f3 ||
		current >= 0x23f8 && current <= 0x23fa || current == 0x24c2 ||
		current >= 0x25aa && current <= 0x25ab || current == 0x25b6 || current == 0x25c0 ||
		current >= 0x25fb && current <= 0x25fe || current >= 0x2600 && current <= 0x27bf ||
		current >= 0x2934 && current <= 0x2935 || current >= 0x2b05 && current <= 0x2b07 ||
		current >= 0x2b1b && current <= 0x2b1c || current == 0x2b50 || current == 0x2b55 ||
		current >= 0x3030 && current <= 0x303d || current == 0x3297 || current == 0x3299 ||
		current >= 0x1f000 && current <= 0x1faff
}

func isRegionalIndicator(current rune) bool {
	return current >= 0x1f1e6 && current <= 0x1f1ff
}

func reconstructsByGrapheme(parent string, parts []string) bool {
	parentClusters := splitGraphemes(parent)
	cursor := 0
	for _, part := range parts {
		if part == "" {
			return false
		}
		clusters := splitGraphemes(part)
		if cursor+len(clusters) > len(parentClusters) {
			return false
		}
		for index, cluster := range clusters {
			if parentClusters[cursor+index] != cluster {
				return false
			}
		}
		cursor += len(clusters)
	}
	return cursor == len(parentClusters)
}

func joinClusters(clusters []string) string {
	return strings.Join(clusters, "")
}
