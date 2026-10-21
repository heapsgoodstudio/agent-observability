package main

import "testing"

func TestCreditPriceAcceptsOnlyAPositiveNumber(t *testing.T) {
	for in, want := range map[string]float64{"0.04": 0.04, " 0.045 ": 0.045, "": 0, "0": 0, "-1": 0, "$0.04": 0, "NaN": 0, "Inf": 0} {
		if got := creditPrice(in); got != want {
			t.Errorf("creditPrice(%q) = %v, want %v", in, got, want)
		}
	}
}
