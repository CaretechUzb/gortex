package localizationauth

import (
	"encoding/json"
	"reflect"
	"strings"
)

const (
	maxPrimaryIDs       = 5
	maxEvidenceIDs      = 16
	maxEvidenceIDSize   = 1024
	maxEvidenceIDsBytes = 8 << 10
)

// NormalizeEvidenceIDs validates and clones the bounded evidence authority
// carried by one terminal receipt. Primary IDs must be a unique subset of the
// complete digest IDs. Empty slices preserve compatibility with older receipts.
func NormalizeEvidenceIDs(primaryIDs, evidenceIDs []string) ([]string, []string, bool) {
	if len(primaryIDs) == 0 && len(evidenceIDs) == 0 {
		return nil, nil, true
	}
	if len(primaryIDs) > maxPrimaryIDs || len(evidenceIDs) > maxEvidenceIDs {
		return nil, nil, false
	}
	normalize := func(values []string) ([]string, map[string]struct{}, int, bool) {
		out := make([]string, 0, len(values))
		seen := make(map[string]struct{}, len(values))
		total := 0
		for _, raw := range values {
			id := strings.TrimSpace(raw)
			if id == "" || len(id) > maxEvidenceIDSize {
				return nil, nil, 0, false
			}
			if _, duplicate := seen[id]; duplicate {
				return nil, nil, 0, false
			}
			total += len(id)
			if total > maxEvidenceIDsBytes {
				return nil, nil, 0, false
			}
			seen[id] = struct{}{}
			out = append(out, id)
		}
		return out, seen, total, true
	}
	evidence, evidenceSet, evidenceBytes, ok := normalize(evidenceIDs)
	if !ok {
		return nil, nil, false
	}
	primary, _, primaryBytes, ok := normalize(primaryIDs)
	if !ok || primaryBytes+evidenceBytes > maxEvidenceIDsBytes {
		return nil, nil, false
	}
	for _, id := range primary {
		if _, present := evidenceSet[id]; !present {
			return nil, nil, false
		}
	}
	return primary, evidence, true
}

// MarshalJSON prevents a direct caller from publishing an oversized authority
// even though the terminal receipt's final-response validation predates these
// optional fields.
func (receipt Receipt) MarshalJSON() ([]byte, error) {
	type wire Receipt
	primary, evidence, ok := NormalizeEvidenceIDs(receipt.PrimaryIDs, receipt.EvidenceIDs)
	if !ok {
		return nil, &json.UnsupportedValueError{Value: reflectReceiptValue(receipt), Str: "invalid localization evidence authority"}
	}
	receipt.PrimaryIDs = primary
	receipt.EvidenceIDs = evidence
	return json.Marshal(wire(receipt))
}

func (receipt *Receipt) UnmarshalJSON(data []byte) error {
	type wire Receipt
	var decoded wire
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	primary, evidence, ok := NormalizeEvidenceIDs(decoded.PrimaryIDs, decoded.EvidenceIDs)
	if !ok {
		return &json.UnsupportedValueError{Value: reflectReceiptValue(Receipt(decoded)), Str: "invalid localization evidence authority"}
	}
	decoded.PrimaryIDs = primary
	decoded.EvidenceIDs = evidence
	*receipt = Receipt(decoded)
	return nil
}

// json.UnsupportedValueError requires a reflect.Value. Keeping its construction
// here avoids exporting another error type from this internal package.
func reflectReceiptValue(receipt Receipt) reflect.Value {
	return reflect.ValueOf(receipt)
}
