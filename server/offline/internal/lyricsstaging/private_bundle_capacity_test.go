package lyricsstaging

import (
	"bytes"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"moesekai/server/internal/lyricssource"
	"moesekai/server/internal/model"
)

func TestNewPrivateEvidenceReceiptRawCapacityBoundary(t *testing.T) {
	if lyricssource.MaxIndexEvidenceRawBytes != 2<<20 || MaxPrivateEvidenceReceiptRawBytes != 32<<20 ||
		MaxPrivateEvidenceReceiptBytes != 64<<20 {
		t.Fatalf(
			"reviewed evidence limits changed: perEvidence=%d rawReceipt=%d encodedReceipt=%d",
			lyricssource.MaxIndexEvidenceRawBytes,
			MaxPrivateEvidenceReceiptRawBytes,
			MaxPrivateEvidenceReceiptBytes,
		)
	}

	maxRaw := bytes.Repeat([]byte("a"), lyricssource.MaxIndexEvidenceRawBytes)
	exact := make([]lyricssource.IndexEvidence, MaxPrivateEvidenceReceiptRawBytes/len(maxRaw))
	for index := range exact {
		exact[index] = privateCapacityRevisionEvidence(index+1, maxRaw, time.Unix(100+int64(index), 0).UTC())
	}

	t.Run("exact 32 MiB", func(t *testing.T) {
		receipt, err := NewPrivateEvidenceReceipt(exact)
		if err != nil {
			t.Fatalf("exact aggregate raw bound: %v", err)
		}
		if got := privateEvidenceReceiptRawBytes(receipt.IndexEvidence); got != MaxPrivateEvidenceReceiptRawBytes {
			t.Fatalf("aggregate raw bytes=%d, want %d", got, MaxPrivateEvidenceReceiptRawBytes)
		}
		body, err := MarshalPrivateEvidenceReceipt(receipt)
		if err != nil {
			t.Fatalf("marshal exact aggregate raw bound: %v", err)
		}
		if len(body) > MaxPrivateEvidenceReceiptBytes {
			t.Fatalf("encoded receipt bytes=%d, limit=%d", len(body), MaxPrivateEvidenceReceiptBytes)
		}
	})

	t.Run("plus one byte", func(t *testing.T) {
		plusOne := append([]lyricssource.IndexEvidence{}, exact...)
		plusOne = append(plusOne, privateCapacityRevisionEvidence(1000, []byte("b"), time.Unix(1000, 0).UTC()))
		runtime.GC()
		var before runtime.MemStats
		runtime.ReadMemStats(&before)
		_, err := NewPrivateEvidenceReceipt(plusOne)
		var after runtime.MemStats
		runtime.ReadMemStats(&after)
		requirePrivateEvidenceCapacityError(
			t,
			err,
			len(plusOne),
			MaxPrivateEvidenceReceiptRawBytes+1,
		)
		if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 4<<20 {
			t.Fatalf("aggregate raw overflow allocated %d bytes before rejection", allocated)
		}
		runtime.KeepAlive(plusOne)
	})
}

func TestNewPrivateEvidenceReceiptChargesCanonicalAcquisitions(t *testing.T) {
	maxRaw := bytes.Repeat([]byte("a"), lyricssource.MaxIndexEvidenceRawBytes)
	exact := make([]lyricssource.IndexEvidence, MaxPrivateEvidenceReceiptRawBytes/len(maxRaw))
	for index := range exact {
		exact[index] = privateCapacityRevisionEvidence(index+1, maxRaw, time.Unix(200+int64(index), 0).UTC())
	}

	t.Run("exact duplicate single charge", func(t *testing.T) {
		withDuplicate := append([]lyricssource.IndexEvidence{}, exact...)
		withDuplicate = append(withDuplicate, clonePrivateIndexEvidence(exact[0]))
		receipt, err := NewPrivateEvidenceReceipt(withDuplicate)
		if err != nil {
			t.Fatalf("exact duplicate at aggregate bound: %v", err)
		}
		if len(receipt.IndexEvidence) != len(exact) ||
			privateEvidenceReceiptRawBytes(receipt.IndexEvidence) != MaxPrivateEvidenceReceiptRawBytes {
			t.Fatalf(
				"exact duplicate charged more than once: items=%d rawBytes=%d",
				len(receipt.IndexEvidence),
				privateEvidenceReceiptRawBytes(receipt.IndexEvidence),
			)
		}
	})

	t.Run("distinct acquisition double charge", func(t *testing.T) {
		reacquired := clonePrivateIndexEvidence(exact[0])
		reacquired.FetchedAt = time.Unix(200, 1).UTC().Format(time.RFC3339Nano)
		reacquired.EvidenceID = lyricssource.MediaWikiRevisionAcquisitionEvidenceID(
			reacquired.Provider,
			"fetch:vocaloid-fandom:"+strconv.Itoa(reacquired.PageID),
			reacquired.FetchedAt,
			reacquired.RawSHA256,
		)
		withReacquisition := append([]lyricssource.IndexEvidence{}, exact...)
		withReacquisition = append(withReacquisition, reacquired)
		_, err := NewPrivateEvidenceReceipt(withReacquisition)
		requirePrivateEvidenceCapacityError(
			t,
			err,
			len(withReacquisition),
			MaxPrivateEvidenceReceiptRawBytes+lyricssource.MaxIndexEvidenceRawBytes,
		)
	})
}

func TestNewPrivateEvidenceReceiptChecksCanonicalItemCapacityBeforeDigest(t *testing.T) {
	evidence := make([]lyricssource.IndexEvidence, MaxPrivateEvidenceReceiptItems+1)
	for index := range evidence {
		evidence[index].EvidenceID = fmt.Sprintf("capacity:%05d", index)
	}
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	_, err := NewPrivateEvidenceReceipt(evidence)
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	requirePrivateEvidenceCapacityError(t, err, len(evidence), 0)
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 2<<20 {
		t.Fatalf("item-count overflow allocated %d bytes before rejection", allocated)
	}
	runtime.KeepAlive(evidence)
}

func TestPrivateEvidenceReceiptRejectsSameIDEnvelopeConflict(t *testing.T) {
	first := privateCapacityRevisionEvidence(1, []byte("first immutable acquisition"), time.Unix(300, 0).UTC())
	conflicting := clonePrivateIndexEvidence(first)
	conflicting.FetchedAt = time.Unix(301, 0).UTC().Format(time.RFC3339Nano)

	if _, err := NewPrivateEvidenceReceipt([]lyricssource.IndexEvidence{first, conflicting}); err == nil ||
		err.Error() != "private evidence ID has conflicting exact resolutions" {
		t.Fatalf("same-ID envelope conflict error=%v", err)
	}
}

func TestPrivateEvidenceReceiptDigestIsDeterministic(t *testing.T) {
	first := privateCapacityRevisionEvidence(1, []byte("first"), time.Unix(400, 0).UTC())
	second := privateCapacityRevisionEvidence(2, []byte("second"), time.Unix(401, 0).UTC())
	third := privateCapacityRevisionEvidence(3, []byte("third"), time.Unix(402, 0).UTC())

	forward, err := NewPrivateEvidenceReceipt([]lyricssource.IndexEvidence{third, first, second, first})
	if err != nil {
		t.Fatal(err)
	}
	reverse, err := NewPrivateEvidenceReceipt([]lyricssource.IndexEvidence{second, first, third})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(forward, reverse) || forward.ReceiptSHA256 == "" {
		t.Fatalf("canonical receipt digest changed with input order:\nforward=%+v\nreverse=%+v", forward, reverse)
	}
}

func TestPrivateEvidenceResolverProjectsDeterministicManifestReachableUnion(t *testing.T) {
	first := privateCapacityRevisionEvidence(1, []byte("first"), time.Unix(410, 0).UTC())
	second := privateCapacityRevisionEvidence(2, []byte("second"), time.Unix(411, 0).UTC())
	orphan := privateCapacityRevisionEvidence(3, []byte("non-manifest"), time.Unix(412, 0).UTC())
	firstCandidate := privateCapacityCandidate(first)
	secondCandidate := privateCapacityCandidate(second)
	orphanCandidate := privateCapacityCandidate(orphan)
	receipt, err := NewPrivateEvidenceReceipt([]lyricssource.IndexEvidence{orphan, second, first})
	if err != nil {
		t.Fatal(err)
	}
	resolver, err := NewPrivateEvidenceResolver(receipt)
	if err != nil {
		t.Fatal(err)
	}
	if err := resolver.ValidateCandidates([]CandidateIdentity{firstCandidate, secondCandidate, orphanCandidate}); err != nil {
		t.Fatalf("full report receipt validation: %v", err)
	}

	projected, err := resolver.ProjectReceipt([]CandidateIdentity{secondCandidate, firstCandidate, firstCandidate})
	if err != nil {
		t.Fatal(err)
	}
	reverse, err := resolver.ProjectReceipt([]CandidateIdentity{firstCandidate, secondCandidate})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(projected, reverse) || len(projected.IndexEvidence) != 2 ||
		projected.IndexEvidence[0].EvidenceID != first.EvidenceID || projected.IndexEvidence[1].EvidenceID != second.EvidenceID ||
		projected.ReceiptSHA256 == receipt.ReceiptSHA256 {
		t.Fatalf("projected receipt is not the deterministic exact subset:\nfull=%+v\nprojected=%+v\nreverse=%+v", receipt, projected, reverse)
	}
	if err := ValidatePrivateEvidenceReceiptForCandidates(projected, []CandidateIdentity{firstCandidate, secondCandidate}); err != nil {
		t.Fatalf("projected exact-union validation: %v", err)
	}

	projected.IndexEvidence[0].Raw[0] ^= 1
	again, err := resolver.ProjectReceipt([]CandidateIdentity{firstCandidate, secondCandidate})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(projected.IndexEvidence[0].Raw, again.IndexEvidence[0].Raw) {
		t.Fatal("projected receipt mutation reached the immutable resolver")
	}
}

func TestPrivateEvidenceResolverProjectionRejectsMissingDuplicateAndConflictingReferences(t *testing.T) {
	evidence := privateCapacityRevisionEvidence(1, []byte("exact"), time.Unix(420, 0).UTC())
	candidate := privateCapacityCandidate(evidence)
	receipt, err := NewPrivateEvidenceReceipt([]lyricssource.IndexEvidence{evidence})
	if err != nil {
		t.Fatal(err)
	}
	resolver, err := NewPrivateEvidenceResolver(receipt)
	if err != nil {
		t.Fatal(err)
	}

	for name, mutate := range map[string]func(*CandidateIdentity){
		"missing ID": func(value *CandidateIdentity) {
			value.IndexEvidenceRefs[0].EvidenceID = strings.Repeat("f", 64)
		},
		"conflicting digest": func(value *CandidateIdentity) {
			value.IndexEvidenceRefs[0].SHA256 = strings.Repeat("e", 64)
		},
		"duplicate ID": func(value *CandidateIdentity) {
			value.IndexEvidenceRefs = append(value.IndexEvidenceRefs, value.IndexEvidenceRefs[0])
		},
	} {
		t.Run(name, func(t *testing.T) {
			mutated := candidate
			mutated.IndexEvidenceRefs = append([]model.LyricsSourceIndexEvidenceRef(nil), candidate.IndexEvidenceRefs...)
			mutate(&mutated)
			if _, err := resolver.ProjectReceipt([]CandidateIdentity{mutated}); err == nil {
				t.Fatal("invalid projected reference was accepted")
			}
		})
	}
}

func TestPrivateEvidenceReceiptStreamingJSONPreservesEncodingJSONContract(t *testing.T) {
	invalidUTF8 := string([]byte{'x', 0xff, 'y'})
	receipt := PrivateEvidenceReceipt{
		SchemaVersion: PrivateEvidenceReceiptSchemaVersion,
		IndexEvidence: []lyricssource.IndexEvidence{
			{
				EvidenceID: invalidUTF8 + `<&>"\\\n\u2028\u2029`,
				SHA256:     "",
				Kind:       lyricssource.IndexEvidenceKind("<kind>"),
				Provider:   model.LyricsSourceProvider("provider&"),
				Origin:     "origin\t",
				Categories: nil,
				Raw:        nil,
			},
			{
				EvidenceID: "second",
				PageID:     1,
				RevisionID: 2,
				Categories: []string{},
				Raw:        []byte{},
			},
		},
		ReceiptSHA256: "",
	}

	standardCompact, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	var streamedCompact bytes.Buffer
	if err := writePrivateEvidenceReceiptJSON(&streamedCompact, receipt, false, false); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(streamedCompact.Bytes(), standardCompact) {
		t.Fatalf("streamed compact JSON drifted from encoding/json:\nstreamed=%s\nstandard=%s", streamedCompact.Bytes(), standardCompact)
	}
	expectedDigest := sha256.Sum256(standardCompact)
	gotDigest, err := privateEvidenceReceiptDigest(receipt)
	if err != nil {
		t.Fatal(err)
	}
	if gotDigest != hex.EncodeToString(expectedDigest[:]) {
		t.Fatalf("streamed digest=%s, want encoding/json digest=%x", gotDigest, expectedDigest)
	}

	standardPretty, err := json.MarshalIndent(receipt, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	standardPretty = append(standardPretty, '\n')
	var streamedPretty bytes.Buffer
	if err := writePrivateEvidenceReceiptJSON(&streamedPretty, receipt, true, true); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(streamedPretty.Bytes(), standardPretty) {
		t.Fatalf("streamed pretty JSON drifted from encoding/json:\nstreamed=%s\nstandard=%s", streamedPretty.Bytes(), standardPretty)
	}
}

func TestValidateAndDecodePrivateEvidenceReceiptRejectMalformedExactEnvelopeWithoutCandidates(t *testing.T) {
	valid := privateCapacityRevisionEvidence(1, []byte("immutable exact evidence"), time.Unix(450, 0).UTC())
	tests := []struct {
		name string
		want string
		item func() lyricssource.IndexEvidence
	}{
		{
			name: "raw digest drift",
			want: "digest does not bind exact raw bytes",
			item: func() lyricssource.IndexEvidence {
				item := clonePrivateIndexEvidence(valid)
				item.Raw[0] ^= 1
				return item
			},
		},
		{
			name: "acquisition identity drift",
			want: "acquisition identity is invalid",
			item: func() lyricssource.IndexEvidence {
				item := clonePrivateIndexEvidence(valid)
				item.FetchedAt = time.Unix(451, 0).UTC().Format(time.RFC3339Nano)
				return item
			},
		},
		{
			name: "per evidence raw overflow",
			want: "bytes or digest are invalid",
			item: func() lyricssource.IndexEvidence {
				return privateCapacityRevisionEvidence(
					2,
					bytes.Repeat([]byte("x"), lyricssource.MaxIndexEvidenceRawBytes+1),
					time.Unix(452, 0).UTC(),
				)
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			item := test.item()
			if _, err := NewPrivateEvidenceReceipt([]lyricssource.IndexEvidence{item}); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("construction error=%v, want substring %q", err, test.want)
			}
			receipt := PrivateEvidenceReceipt{
				SchemaVersion: PrivateEvidenceReceiptSchemaVersion,
				IndexEvidence: []lyricssource.IndexEvidence{item},
				ReceiptSHA256: strings.Repeat("0", 64),
			}
			var err error
			receipt.ReceiptSHA256, err = privateEvidenceReceiptDigest(receipt)
			if err != nil {
				t.Fatal(err)
			}
			if err := ValidatePrivateEvidenceReceipt(receipt); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("validation error=%v, want substring %q", err, test.want)
			}
			body, err := json.Marshal(receipt)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := DecodePrivateEvidenceReceipt(body); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("decode error=%v, want substring %q", err, test.want)
			}
		})
	}
}

func TestPrivateEvidenceReceiptEncodedOverflowIsRejectedWithoutFullBodyAllocation(t *testing.T) {
	item := privateCapacityRevisionEvidence(1, []byte("small"), time.Unix(460, 0).UTC())
	oversizedTitle := strings.Repeat("x", MaxPrivateEvidenceReceiptBytes)
	item.Title = oversizedTitle
	receipt := PrivateEvidenceReceipt{
		SchemaVersion: PrivateEvidenceReceiptSchemaVersion,
		IndexEvidence: []lyricssource.IndexEvidence{item},
		ReceiptSHA256: strings.Repeat("0", 64),
	}

	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	err := ValidatePrivateEvidenceReceipt(receipt)
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	runtime.KeepAlive(oversizedTitle)
	if !errors.Is(err, errPrivateEvidenceReceiptEncodedLimit) {
		t.Fatalf("encoded overflow error=%v", err)
	}
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 4<<20 {
		t.Fatalf("encoded overflow validation allocated %d bytes; want bounded streaming rejection", allocated)
	}

	input := []lyricssource.IndexEvidence{item}
	runtime.GC()
	var beforeConstruction runtime.MemStats
	runtime.ReadMemStats(&beforeConstruction)
	_, constructionErr := NewPrivateEvidenceReceipt(input)
	var afterConstruction runtime.MemStats
	runtime.ReadMemStats(&afterConstruction)
	if !errors.Is(constructionErr, errPrivateEvidenceReceiptEncodedLimit) {
		t.Fatalf("encoded overflow construction error=%v", constructionErr)
	}
	if allocated := afterConstruction.TotalAlloc - beforeConstruction.TotalAlloc; allocated > 4<<20 {
		t.Fatalf("encoded overflow construction allocated %d bytes before rejection", allocated)
	}
	runtime.KeepAlive(input)
}

func TestNewPrivateEvidenceReceiptRejectsCanonicalPrettyEncodingOverflowBeforeDigest(t *testing.T) {
	item := privateCapacityRevisionEvidence(1, []byte("small"), time.Unix(465, 0).UTC())
	probe := PrivateEvidenceReceipt{
		SchemaVersion: PrivateEvidenceReceiptSchemaVersion,
		IndexEvidence: []lyricssource.IndexEvidence{item},
		ReceiptSHA256: strings.Repeat("0", 64),
	}
	compactBase, err := privateEvidenceReceiptJSONSize(probe, false, false)
	if err != nil {
		t.Fatal(err)
	}
	prettyBase, err := privateEvidenceReceiptJSONSize(probe, true, true)
	if err != nil {
		t.Fatal(err)
	}
	categoryBytes := len(item.Categories[0]) + MaxPrivateEvidenceReceiptBytes + 1 - prettyBase
	if categoryBytes <= 0 || prettyBase-compactBase <= 1 {
		t.Fatalf("unexpected canonical encoding overhead: compact=%d pretty=%d", compactBase, prettyBase)
	}
	oversizedCategory := strings.Repeat("x", categoryBytes)
	item.Categories = []string{oversizedCategory}
	probe.IndexEvidence[0] = item
	if _, err := privateEvidenceReceiptJSONSize(probe, false, false); err != nil {
		t.Fatalf("compact receipt should remain within the limit: %v", err)
	}
	if _, err := privateEvidenceReceiptJSONSize(probe, true, true); !errors.Is(err, errPrivateEvidenceReceiptEncodedLimit) {
		t.Fatalf("canonical pretty receipt overflow error=%v", err)
	}

	input := []lyricssource.IndexEvidence{item}
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	_, err = NewPrivateEvidenceReceipt(input)
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	if !errors.Is(err, errPrivateEvidenceReceiptEncodedLimit) {
		t.Fatalf("canonical pretty overflow construction error=%v", err)
	}
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 4<<20 {
		t.Fatalf("canonical pretty overflow allocated %d bytes before rejection", allocated)
	}
	runtime.KeepAlive(oversizedCategory)
	runtime.KeepAlive(input)
}

func TestNewPrivateEvidenceReceiptClonesCanonicalDuplicateRawOnce(t *testing.T) {
	raw := bytes.Repeat([]byte("a"), lyricssource.MaxIndexEvidenceRawBytes)
	item := privateCapacityRevisionEvidence(1, raw, time.Unix(470, 0).UTC())
	duplicates := make([]lyricssource.IndexEvidence, 12)
	for index := range duplicates {
		duplicates[index] = item
	}

	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	receipt, err := NewPrivateEvidenceReceipt(duplicates)
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	if err != nil {
		t.Fatal(err)
	}
	if len(receipt.IndexEvidence) != 1 {
		t.Fatalf("canonical evidence count=%d, want 1", len(receipt.IndexEvidence))
	}
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 8<<20 {
		t.Fatalf("canonical duplicate construction allocated %d bytes; want one bounded raw clone", allocated)
	}
	raw[0] = 'b'
	if receipt.IndexEvidence[0].Raw[0] != 'a' {
		t.Fatal("receipt did not retain one defensive canonical raw clone")
	}
	runtime.KeepAlive(duplicates)
}

func TestPrivateEvidenceResolverRejectsResolutionDriftAndOrphans(t *testing.T) {
	first := privateCapacityRevisionEvidence(1, []byte("first"), time.Unix(500, 0).UTC())
	orphan := privateCapacityRevisionEvidence(2, []byte("orphan"), time.Unix(501, 0).UTC())
	candidate := privateCapacityCandidate(first)

	t.Run("orphan", func(t *testing.T) {
		receipt, err := NewPrivateEvidenceReceipt([]lyricssource.IndexEvidence{first, orphan})
		if err != nil {
			t.Fatal(err)
		}
		if err := ValidatePrivateEvidenceReceiptForCandidates(receipt, []CandidateIdentity{candidate}); err == nil ||
			err.Error() != "private evidence receipt contains orphan evidence" {
			t.Fatalf("orphan evidence error=%v", err)
		}
	})

	t.Run("missing", func(t *testing.T) {
		receipt, err := NewPrivateEvidenceReceipt([]lyricssource.IndexEvidence{first})
		if err != nil {
			t.Fatal(err)
		}
		missing := candidate
		missing.IndexEvidenceRefs = append([]model.LyricsSourceIndexEvidenceRef{}, candidate.IndexEvidenceRefs...)
		missing.IndexEvidenceRefs[0].EvidenceID = strings.Repeat("a", 64)
		if _, err := receipt.HydrateCandidate(missing); err == nil ||
			err.Error() != "candidate reference is unresolved by the private evidence receipt" {
			t.Fatalf("missing evidence error=%v", err)
		}
	})

	t.Run("duplicate reference", func(t *testing.T) {
		receipt, err := NewPrivateEvidenceReceipt([]lyricssource.IndexEvidence{first})
		if err != nil {
			t.Fatal(err)
		}
		duplicated := candidate
		duplicated.IndexEvidenceRefs = append(
			append([]model.LyricsSourceIndexEvidenceRef{}, candidate.IndexEvidenceRefs...),
			candidate.IndexEvidenceRefs[0],
		)
		if _, err := receipt.HydrateCandidate(duplicated); err == nil {
			t.Fatal("duplicate candidate evidence reference was accepted")
		}
	})

	t.Run("provider mismatch", func(t *testing.T) {
		receipt, err := NewPrivateEvidenceReceipt([]lyricssource.IndexEvidence{first})
		if err != nil {
			t.Fatal(err)
		}
		mismatched := candidate
		mismatched.Provider = model.LyricsSourceProviderMoegirl
		mismatched.Origin = model.LyricsSourceOriginMoegirl
		if _, err := receipt.HydrateCandidate(mismatched); err == nil {
			t.Fatal("provider-mismatched candidate evidence was accepted")
		}
	})

	t.Run("raw SHA drift", func(t *testing.T) {
		receipt, err := NewPrivateEvidenceReceipt([]lyricssource.IndexEvidence{first})
		if err != nil {
			t.Fatal(err)
		}
		receipt.IndexEvidence[0].Raw = append([]byte{}, receipt.IndexEvidence[0].Raw...)
		receipt.IndexEvidence[0].Raw[0] ^= 1
		receipt.ReceiptSHA256, err = privateEvidenceReceiptDigest(receipt)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := receipt.HydrateCandidate(candidate); err == nil {
			t.Fatal("raw bytes detached from their exact SHA-256 were accepted")
		}
	})

	t.Run("acquisition identity drift", func(t *testing.T) {
		receipt, err := NewPrivateEvidenceReceipt([]lyricssource.IndexEvidence{first})
		if err != nil {
			t.Fatal(err)
		}
		receipt.IndexEvidence[0].FetchedAt = time.Unix(502, 0).UTC().Format(time.RFC3339Nano)
		receipt.ReceiptSHA256, err = privateEvidenceReceiptDigest(receipt)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := receipt.HydrateCandidate(candidate); err == nil {
			t.Fatal("evidence envelope detached from its acquisition ID was accepted")
		}
	})

	t.Run("receipt ordering and digest", func(t *testing.T) {
		receipt, err := NewPrivateEvidenceReceipt([]lyricssource.IndexEvidence{first, orphan})
		if err != nil {
			t.Fatal(err)
		}
		receipt.IndexEvidence[0], receipt.IndexEvidence[1] = receipt.IndexEvidence[1], receipt.IndexEvidence[0]
		if _, err := receipt.HydrateCandidate(candidate); err == nil ||
			err.Error() != "private evidence receipt is not uniquely ordered by evidence ID" {
			t.Fatalf("unordered receipt error=%v", err)
		}
		receipt.IndexEvidence[0], receipt.IndexEvidence[1] = receipt.IndexEvidence[1], receipt.IndexEvidence[0]
		receipt.ReceiptSHA256 = strings.Repeat("0", 64)
		if _, err := receipt.HydrateCandidate(candidate); err == nil ||
			err.Error() != "private evidence receipt digest does not match" {
			t.Fatalf("receipt digest error=%v", err)
		}
	})
}

func TestReceiptCandidateValidationAvoidsRetainedRawResolverCopies(t *testing.T) {
	raw := bytes.Repeat([]byte("a"), lyricssource.MaxIndexEvidenceRawBytes)
	evidence := privateCapacityRevisionEvidence(1, raw, time.Unix(489, 0).UTC())
	receipt, err := NewPrivateEvidenceReceipt([]lyricssource.IndexEvidence{evidence})
	if err != nil {
		t.Fatal(err)
	}
	candidate := privateCapacityCandidate(evidence)

	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	err = ValidatePrivateEvidenceReceiptForCandidates(receipt, []CandidateIdentity{candidate})
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	if err != nil {
		t.Fatal(err)
	}
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 3<<20 {
		t.Fatalf("one-shot receipt validation allocated %d bytes; raw evidence was retained in a defensive resolver clone", allocated)
	}
	runtime.KeepAlive(receipt)
	runtime.KeepAlive(candidate)
	runtime.KeepAlive(raw)
}

func TestFixedArtifactEvidenceValidationAvoidsDuplicateRawResolverCopies(t *testing.T) {
	raw := bytes.Repeat([]byte("a"), lyricssource.MaxIndexEvidenceRawBytes)
	evidence := privateCapacityRevisionEvidence(1, raw, time.Unix(490, 0).UTC())
	receipt, err := NewPrivateEvidenceReceipt([]lyricssource.IndexEvidence{evidence})
	if err != nil {
		t.Fatal(err)
	}
	artifact := FixedArtifact{
		Candidate: privateCapacityCandidate(evidence),
		Fixed: lyricssource.FixedRevision{
			IndexEvidence: []lyricssource.IndexEvidence{clonePrivateIndexEvidence(evidence)},
		},
	}

	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	err = validatePrivateEvidenceReceiptForFixedArtifacts(receipt, []FixedArtifact{artifact})
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	if err != nil {
		t.Fatal(err)
	}
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 3<<20 {
		t.Fatalf("fixed-artifact receipt validation allocated %d bytes; raw receipt was cloned into a duplicate resolver", allocated)
	}
	runtime.KeepAlive(receipt)
	runtime.KeepAlive(artifact)
	runtime.KeepAlive(raw)
}

func TestPrivateEvidenceReceiptSupports704CandidatesWithSharedFixedAuthority(t *testing.T) {
	const candidateCount = 704
	receipt, candidates := private704SharedAuthorityFixture(t)
	rawBytes := privateEvidenceReceiptRawBytes(receipt.IndexEvidence)
	if len(receipt.IndexEvidence) != candidateCount+1 || rawBytes >= MaxPrivateEvidenceReceiptRawBytes {
		t.Fatalf(
			"704-candidate receipt capacity: items=%d rawBytes=%d rawLimit=%d",
			len(receipt.IndexEvidence),
			rawBytes,
			MaxPrivateEvidenceReceiptRawBytes,
		)
	}
	runtime.GC()
	var beforeOneShot runtime.MemStats
	runtime.ReadMemStats(&beforeOneShot)
	if err := ValidatePrivateEvidenceReceiptForCandidates(receipt, candidates); err != nil {
		t.Fatalf("704-candidate one-shot validation: %v", err)
	}
	var afterOneShot runtime.MemStats
	runtime.ReadMemStats(&afterOneShot)
	if allocated := afterOneShot.TotalAlloc - beforeOneShot.TotalAlloc; allocated > 16<<20 {
		t.Fatalf("704-candidate one-shot validation allocated %d bytes", allocated)
	}
	resolver, err := NewPrivateEvidenceResolver(receipt)
	if err != nil {
		t.Fatalf("704-candidate resolver construction: %v", err)
	}
	runtime.GC()
	var beforeValidation runtime.MemStats
	runtime.ReadMemStats(&beforeValidation)
	if err := resolver.ValidateCandidates(candidates); err != nil {
		t.Fatalf("704-candidate shared-authority resolution: %v", err)
	}
	var afterValidation runtime.MemStats
	runtime.ReadMemStats(&afterValidation)
	if allocated := afterValidation.TotalAlloc - beforeValidation.TotalAlloc; allocated > 16<<20 {
		t.Fatalf("704-candidate validation allocated %d bytes; raw evidence was likely hydrated and retained", allocated)
	}
	runtime.GC()
	var beforeProjection runtime.MemStats
	runtime.ReadMemStats(&beforeProjection)
	projected, err := resolver.ProjectReceipt(candidates)
	var afterProjection runtime.MemStats
	runtime.ReadMemStats(&afterProjection)
	if err != nil {
		t.Fatalf("704-candidate projected receipt: %v", err)
	}
	if allocated := afterProjection.TotalAlloc - beforeProjection.TotalAlloc; allocated > 16<<20 {
		t.Fatalf("704-candidate receipt projection allocated %d bytes", allocated)
	}
	if len(projected.IndexEvidence) != candidateCount+1 {
		t.Fatalf("704-candidate projected evidence=%d, want %d", len(projected.IndexEvidence), candidateCount+1)
	}
	listCount := 0
	for _, evidence := range projected.IndexEvidence {
		if _, err := lyricssource.SekaipediaAuthorityFromIndexEvidence(evidence); err == nil {
			listCount++
		}
	}
	if listCount != 1 {
		t.Fatalf("projected shared List authority count=%d, want 1", listCount)
	}
	if err := ValidatePrivateEvidenceReceiptForCandidates(projected, candidates); err != nil {
		t.Fatalf("704-candidate projected exact union: %v", err)
	}
	runtime.GC()
	var beforeHydration runtime.MemStats
	runtime.ReadMemStats(&beforeHydration)
	hydrated, err := resolver.HydrateCandidates(candidates)
	var afterHydration runtime.MemStats
	runtime.ReadMemStats(&afterHydration)
	if err != nil || len(hydrated) != candidateCount {
		t.Fatalf("704-candidate linear hydration count=%d err=%v", len(hydrated), err)
	}
	if allocated := afterHydration.TotalAlloc - beforeHydration.TotalAlloc; allocated > 16<<20 {
		t.Fatalf("704-candidate hydration allocated %d bytes; shared raw evidence was cloned per candidate", allocated)
	}
	if &hydrated[0].IndexEvidence[0].Raw[0] != &hydrated[1].IndexEvidence[0].Raw[0] {
		t.Fatal("shared immutable authority was not represented by one batch-local clone")
	}
	body, err := MarshalPrivateEvidenceReceipt(receipt)
	if err != nil {
		t.Fatal(err)
	}
	if len(body) >= MaxPrivateEvidenceReceiptBytes {
		t.Fatalf("704-candidate encoded receipt bytes=%d, limit=%d", len(body), MaxPrivateEvidenceReceiptBytes)
	}
	originalAuthorityByte := receipt.IndexEvidence[0].Raw[0]
	receipt.IndexEvidence[0].Raw[0] ^= 1
	immutable, err := resolver.HydrateCandidate(candidates[0])
	if err != nil {
		t.Fatalf("hydrate after caller receipt mutation: %v", err)
	}
	if immutable.IndexEvidence[0].Raw[0] != originalAuthorityByte {
		t.Fatal("resolver retained a mutable alias to caller-owned receipt evidence")
	}
}

func TestPrivateEvidenceResolverRejectsInvalid704SharedAuthorityReferences(t *testing.T) {
	receipt, candidates := private704SharedAuthorityFixture(t)
	resolver, err := NewPrivateEvidenceResolver(receipt)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name   string
		want   string
		mutate func(*CandidateIdentity)
	}{
		{
			name: "malformed reference",
			want: "candidate reference is unresolved by the private evidence receipt",
			mutate: func(candidate *CandidateIdentity) {
				candidate.IndexEvidenceRefs[0].EvidenceID = " malformed"
			},
		},
		{
			name: "duplicate reference",
			want: "candidate index evidence reference is duplicated",
			mutate: func(candidate *CandidateIdentity) {
				candidate.IndexEvidenceRefs = append(candidate.IndexEvidenceRefs, candidate.IndexEvidenceRefs[0])
			},
		},
		{
			name: "missing fixed authority reference",
			want: "Sekaipedia candidate requires fixed List and song revision evidence",
			mutate: func(candidate *CandidateIdentity) {
				candidate.IndexEvidenceRefs = candidate.IndexEvidenceRefs[1:]
			},
		},
		{
			name: "missing receipt reference",
			want: "candidate reference is unresolved by the private evidence receipt",
			mutate: func(candidate *CandidateIdentity) {
				candidate.IndexEvidenceRefs[1].EvidenceID = strings.Repeat("f", 64)
			},
		},
		{
			name: "conflicting reference digest",
			want: "candidate reference is unresolved by the private evidence receipt",
			mutate: func(candidate *CandidateIdentity) {
				candidate.IndexEvidenceRefs[1].SHA256 = candidate.IndexEvidenceRefs[0].SHA256
			},
		},
		{
			name: "conflicting song reference",
			want: "Sekaipedia revision evidence is neither the fixed List nor the candidate song",
			mutate: func(candidate *CandidateIdentity) {
				candidate.IndexEvidenceRefs[1] = candidates[1].IndexEvidenceRefs[1]
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := candidates[0]
			candidate.IndexEvidenceRefs = append([]model.LyricsSourceIndexEvidenceRef(nil), candidate.IndexEvidenceRefs...)
			test.mutate(&candidate)
			candidateSet := append([]CandidateIdentity(nil), candidates...)
			candidateSet[0] = candidate
			if err := resolver.ValidateCandidates(candidateSet); err == nil || err.Error() != test.want {
				t.Fatalf("invalid 704 shared-authority references error=%v, want %q", err, test.want)
			}
			if _, err := resolver.ProjectReceipt(candidateSet); err == nil || err.Error() != test.want {
				t.Fatalf("invalid projected 704 shared-authority references error=%v, want %q", err, test.want)
			}
		})
	}
}

func private704SharedAuthorityFixture(t *testing.T) (PrivateEvidenceReceipt, []CandidateIdentity) {
	t.Helper()
	const candidateCount = 704
	listEvidence := privateSekaipediaListEvidence(t)
	evidence := make([]lyricssource.IndexEvidence, 1, candidateCount+1)
	evidence[0] = listEvidence
	candidates := make([]CandidateIdentity, candidateCount)
	for index := range candidates {
		candidate, songEvidence := privateSekaipediaCapacityCandidate(t, index, listEvidence)
		candidates[index] = candidate
		evidence = append(evidence, songEvidence)
	}
	receipt, err := NewPrivateEvidenceReceipt(evidence)
	if err != nil {
		t.Fatalf("704-candidate shared-authority receipt: %v", err)
	}
	return receipt, candidates
}

func privateCapacityRevisionEvidence(pageID int, raw []byte, fetchedAt time.Time) lyricssource.IndexEvidence {
	revisionID := 100000 + pageID
	title := fmt.Sprintf("Capacity-%d", pageID)
	rawSHA1 := sha1.Sum(raw)
	rawSHA256 := sha256.Sum256(raw)
	rawSHA256Text := hex.EncodeToString(rawSHA256[:])
	fetchedAtText := fetchedAt.Format(time.RFC3339Nano)
	return lyricssource.IndexEvidence{
		EvidenceID: lyricssource.MediaWikiRevisionAcquisitionEvidenceID(
			model.LyricsSourceProviderVocaloidFandom,
			"fetch:vocaloid-fandom:"+strconv.Itoa(pageID),
			fetchedAtText,
			rawSHA256Text,
		),
		SHA256:        rawSHA256Text,
		Kind:          lyricssource.IndexEvidenceKindMediaWikiRevision,
		Provider:      model.LyricsSourceProviderVocaloidFandom,
		Origin:        model.LyricsSourceOriginVocaloidFandom,
		PageID:        pageID,
		RevisionID:    revisionID,
		MediaWikiSHA1: hex.EncodeToString(rawSHA1[:]),
		Title:         title,
		CanonicalURL: fmt.Sprintf(
			"https://vocaloid.fandom.com/wiki/%s?oldid=%d",
			title,
			revisionID,
		),
		Categories: []string{"Songs"},
		FetchedAt:  fetchedAtText,
		Raw:        raw,
		RawSHA256:  rawSHA256Text,
	}
}

func privateCapacityCandidate(evidence lyricssource.IndexEvidence) CandidateIdentity {
	return CandidateIdentity{
		Provider:      evidence.Provider,
		Origin:        evidence.Origin,
		PageID:        evidence.PageID,
		RevisionID:    evidence.RevisionID,
		SHA1:          evidence.MediaWikiSHA1,
		Title:         evidence.Title,
		CanonicalURL:  evidence.CanonicalURL,
		Categories:    append([]string{}, evidence.Categories...),
		Section:       "Lyrics",
		RenditionKey:  "full-sekai",
		VersionReason: model.LyricsSourceVersionReasonUntaggedFullOnly,
		IndexEvidenceRefs: []model.LyricsSourceIndexEvidenceRef{{
			EvidenceID: evidence.EvidenceID,
			SHA256:     evidence.SHA256,
		}},
	}
}

func privateSekaipediaListEvidence(t *testing.T) lyricssource.IndexEvidence {
	t.Helper()
	raw, err := os.ReadFile("../../../internal/lyricssource/testdata/sekaipedia-list-335193.json")
	if err != nil {
		t.Fatal(err)
	}
	rawDigest := sha256.Sum256(raw)
	rawSHA256 := hex.EncodeToString(rawDigest[:])
	const fetchedAt = "2026-08-01T12:00:00Z"
	if rawSHA256 != "c21e31c36f8e7d7534af1617d5b737a1662decd40c34c9e7d4aab71b103ef8dd" {
		t.Fatalf("fixed Sekaipedia List fixture SHA-256=%s", rawSHA256)
	}
	return lyricssource.IndexEvidence{
		EvidenceID: lyricssource.MediaWikiRevisionAcquisitionEvidenceID(
			model.LyricsSourceProviderSekaipedia,
			"authority:sekaipedia:list-of-songs:335193",
			fetchedAt,
			rawSHA256,
		),
		SHA256:            rawSHA256,
		Kind:              lyricssource.IndexEvidenceKindMediaWikiRevision,
		Provider:          model.LyricsSourceProviderSekaipedia,
		Origin:            model.LyricsSourceOriginSekaipedia,
		PageID:            268,
		RevisionID:        335193,
		RevisionTimestamp: "2026-07-27T16:29:13Z",
		MediaWikiSHA1:     "b216a827f88c59f5e954a120027832fe9cd74413",
		Title:             "List of songs",
		CanonicalURL:      "https://www.sekaipedia.org/wiki/List_of_songs?oldid=335193",
		Categories:        []string{"Lists", "Project SEKAI"},
		FetchedAt:         fetchedAt,
		Raw:               raw,
		RawSHA256:         rawSHA256,
	}
}

func privateSekaipediaCapacityCandidate(
	t *testing.T,
	index int,
	listEvidence lyricssource.IndexEvidence,
) (CandidateIdentity, lyricssource.IndexEvidence) {
	t.Helper()
	pageID := 1000 + index
	revisionID := 2000 + index
	title := fmt.Sprintf("Capacity-Song-%03d", index)
	content := fmt.Sprintf("容量試験歌詞-%03d", index)
	contentSHA1 := sha1.Sum([]byte(content))
	contentSHA1Text := hex.EncodeToString(contentSHA1[:])
	revisionTimestamp := time.Date(2026, time.August, 1, 0, 0, 0, 0, time.UTC).
		Add(time.Duration(index) * time.Second).
		Format(time.RFC3339Nano)
	const fetchedAt = "2026-08-02T00:00:00Z"
	categories := []string{"Lyrics", "Songs"}
	canonicalURL := fmt.Sprintf("https://www.sekaipedia.org/wiki/%s?oldid=%d", title, revisionID)
	raw, err := json.Marshal(map[string]any{
		"query": map[string]any{"pages": map[string]any{strconv.Itoa(pageID): map[string]any{
			"pageid": pageID,
			"title":  title,
			"categories": []map[string]string{
				{"title": "Category:Lyrics"},
				{"title": "Category:Songs"},
			},
			"revisions": []map[string]any{{
				"revid":     revisionID,
				"timestamp": revisionTimestamp,
				"sha1":      contentSHA1Text,
				"slots": map[string]any{
					"main": map[string]string{"content": content},
				},
			}},
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	rawDigest := sha256.Sum256(raw)
	rawSHA256 := hex.EncodeToString(rawDigest[:])
	evidenceID := lyricssource.MediaWikiRevisionAcquisitionEvidenceID(
		model.LyricsSourceProviderSekaipedia,
		fmt.Sprintf("revision:sekaipedia:%d:%d", pageID, revisionID),
		fetchedAt,
		rawSHA256,
	)
	songEvidence := lyricssource.IndexEvidence{
		EvidenceID:          evidenceID,
		SHA256:              rawSHA256,
		Kind:                lyricssource.IndexEvidenceKindMediaWikiRevision,
		Provider:            model.LyricsSourceProviderSekaipedia,
		Origin:              model.LyricsSourceOriginSekaipedia,
		PageID:              pageID,
		RevisionID:          revisionID,
		RevisionTimestamp:   revisionTimestamp,
		MediaWikiSHA1:       contentSHA1Text,
		Title:               title,
		CanonicalURL:        canonicalURL,
		Categories:          append([]string{}, categories...),
		CanonicalRequestURL: "",
		FetchedAt:           fetchedAt,
		Raw:                 raw,
		RawSHA256:           rawSHA256,
	}
	candidate := CandidateIdentity{
		Provider:          model.LyricsSourceProviderSekaipedia,
		Origin:            model.LyricsSourceOriginSekaipedia,
		PageID:            pageID,
		RevisionID:        revisionID,
		RevisionTimestamp: revisionTimestamp,
		SHA1:              contentSHA1Text,
		Title:             title,
		CanonicalURL:      canonicalURL,
		Categories:        append([]string{}, categories...),
		Section:           "Lyrics/Full Version",
		RenditionKey:      "full-sekai",
		VersionReason:     model.LyricsSourceVersionReasonUntaggedFullOnly,
		IndexEvidenceRefs: []model.LyricsSourceIndexEvidenceRef{
			{EvidenceID: listEvidence.EvidenceID, SHA256: listEvidence.SHA256},
			{EvidenceID: songEvidence.EvidenceID, SHA256: songEvidence.SHA256},
		},
	}
	return candidate, songEvidence
}

func privateEvidenceReceiptRawBytes(evidence []lyricssource.IndexEvidence) int {
	total := 0
	for _, item := range evidence {
		total += len(item.Raw)
	}
	return total
}

func requirePrivateEvidenceCapacityError(t *testing.T, err error, itemCount, rawBytes int) {
	t.Helper()
	if err == nil {
		t.Fatal("over-capacity private evidence receipt was accepted")
	}
	message := err.Error()
	for _, want := range []string{
		fmt.Sprintf("item count %d", itemCount),
		fmt.Sprintf("limit %d", MaxPrivateEvidenceReceiptItems),
		fmt.Sprintf("aggregate raw bytes %d", rawBytes),
		fmt.Sprintf("limit %d", MaxPrivateEvidenceReceiptRawBytes),
	} {
		if !strings.Contains(message, want) {
			t.Fatalf("capacity error %q does not contain %q", message, want)
		}
	}
	if strings.Contains(message, "Capacity-") || strings.Contains(message, "vocaloid.fandom.com") {
		t.Fatalf("capacity diagnostic leaked evidence content or identity: %q", message)
	}
}

func TestRequireCanonicalPreflightBytesStreamsLargeReceiptWithoutReportSizedClone(t *testing.T) {
	raw := bytes.Repeat([]byte("x"), 8<<20)
	report := PreflightReport{
		SchemaVersion:        PreflightSchemaVersion,
		GeneratedAt:          time.Unix(123, 0).UTC().Format(time.RFC3339Nano),
		CatalogSchemaVersion: CatalogSchemaVersion,
		CatalogCount:         0,
		Summary:              PreflightSummary{},
		EvidenceReceipt: &PrivateEvidenceReceipt{
			SchemaVersion: PrivateEvidenceReceiptSchemaVersion,
			IndexEvidence: []lyricssource.IndexEvidence{{
				EvidenceID: "streaming-capacity-fixture",
				Raw:        raw,
			}},
			ReceiptSHA256: strings.Repeat("0", 64),
		},
		CatalogReview: []PreflightItem{}, GameSizeEvidence: []PreflightItem{}, UniqueComplete: []PreflightItem{},
		Ambiguous: []PreflightItem{}, Missing: []PreflightItem{}, Incomplete: []PreflightItem{}, Error: []PreflightItem{},
	}
	body, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	body = append(body, '\n')

	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	err = RequireCanonicalPreflightBytes(body, report)
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	if err != nil {
		t.Fatal(err)
	}
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 4<<20 {
		t.Fatalf("canonical preflight verification allocated %d bytes; report-sized canonical clone was likely retained", allocated)
	}

	compact, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if err := RequireCanonicalPreflightBytes(compact, report); err == nil ||
		!strings.Contains(err.Error(), "canonical lyrics-preflight JSON serialization") {
		t.Fatalf("noncanonical preflight error=%v", err)
	}
	runtime.KeepAlive(raw)
	runtime.KeepAlive(body)
}
