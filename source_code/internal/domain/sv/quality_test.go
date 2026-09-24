package sv

import (
	"reflect"
	"testing"
)

func TestQualityBitAssignments(t *testing.T) {
	tests := []struct {
		name string
		got  Quality
		want Quality
	}{
		{"reserved validity", QualityReserved, 1},
		{"invalid validity", QualityInvalid, 2},
		{"questionable validity", QualityQuestionable, 3},
		{"test", QualityTest, 1 << 11},
		{"operator blocked", QualityOperatorBlocked, 1 << 12},
		{"derived", QualityDerived, 1 << 13},
	}
	for _, tt := range tests {
		if tt.got != tt.want {
			t.Errorf("%s: got 0x%X, want 0x%X", tt.name, tt.got, tt.want)
		}
	}
}

func TestQualityFlags(t *testing.T) {
	if got := QualityReserved.Flags(); !reflect.DeepEqual(got, []string{"ReservedValidity"}) {
		t.Fatalf("reserved Flags: %v", got)
	}
	if got := QualityInvalid.Flags(); !reflect.DeepEqual(got, []string{"Invalid"}) {
		t.Fatalf("invalid Flags: %v", got)
	}
	quality := QualityQuestionable | QualityTest | QualityOperatorBlocked | QualityDerived
	want := []string{"Questionable", "Test", "OperatorBlocked", "Derived"}
	if got := quality.Flags(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Flags: got %v, want %v", got, want)
	}
}
