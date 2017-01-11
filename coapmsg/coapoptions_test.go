package coapmsg

import (
	"fmt"
	"testing"
)

var numbers = []struct {
	Num        OptionId
	Critical   bool
	Unsafe     bool
	NoCahceKey bool
}{
	{1, true, false, false},
	{3, true, true, false},
	{4, false, false, false},
	{5, true, false, false},
	{7, true, true, true},
	{8, false, false, false},
	{11, true, true, true},

	{12, false, false, false},
	{14, false, true, true},
	{15, true, true, true},
	{17, true, false, false},
	{20, false, false, false},
	{35, true, true, true},
	{39, true, true, true},
	{60, false, false, true},

	// Custom options by Lobaro
	{3000, false, false, false},
	{3008, false, false, false},
	{3012, false, false, false},
	{3016, false, false, false},
	{3020, false, false, false},
}

func TestNumbers(t *testing.T) {
	for _, n := range numbers {
		def := OptionDef{
			Number: n.Num,
		}

		if n.Critical != def.Critical() {
			t.Error(fmt.Sprint("Option ", n.Num, " Critical does not match, should be ", n.Critical))
		}
		if n.Unsafe != def.UnSafe() {
			t.Error(fmt.Sprint("Option ", n.Num, " UnSafe does not match, should be ", n.Unsafe))
		}
		// NoCacheKey only has a meaning for options that are Safe-to-Forward
		if !def.UnSafe() && n.NoCahceKey != def.NoCacheKey() {
			t.Error(fmt.Sprint("Option ", n.Num, " NoCacheKey does not match, should be ", n.NoCahceKey))
		}
	}
}

func _TestFindNumbers(t *testing.T) {
	for i := 3000; i < 3200; i++ {
		def := OptionDef{
			Number: OptionId(i),
		}
		if def.Critical() {
			continue
		}
		if def.UnSafe() {
			continue
		}

		if def.NoCacheKey() {
			continue
		}

		t.Log(fmt.Sprint(def.Number, ": ", def.Critical(), "\t", def.UnSafe(), "\t", def.NoCacheKey()))
	}
}