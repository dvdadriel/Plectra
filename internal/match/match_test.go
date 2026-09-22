package match

import "testing"

func TestKeyIgnoresSpellingNoise(t *testing.T) {
	same := [][4]string{
		{"Massive Attack", "Teardrop", "massive attack", "Teardrop (Remastered 2011)"},
		{"Queen", "Under Pressure", "QUEEN", "Under Pressure - Live"},
		{"Sigur Rós", "Hoppípolla", "Sigur Ros", "Hoppipolla"},
		{"AC/DC", "Back In Black", "ACDC", "Back in Black [Bonus Track]"},
		{"Beyoncé", "Halo", "beyonce", "Halo"},
	}
	for _, c := range same {
		if a, b := Key(c[0], c[1]), Key(c[2], c[3]); a != b {
			t.Errorf("%q/%q and %q/%q differ: %q vs %q", c[0], c[1], c[2], c[3], a, b)
		}
	}
	// Different songs must not collide.
	if Key("Queen", "Bohemian Rhapsody") == Key("Queen", "Under Pressure") {
		t.Error("different titles produced the same key")
	}
	// Different artists must not collide either.
	if Key("Queen", "Halo") == Key("Beyoncé", "Halo") {
		t.Error("different artists produced the same key")
	}
}
