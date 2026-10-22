// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The MDTI Authors

package lattice

import (
	Lattice "github.com/anduril/lattice-sdk-go/v5"
)

// midNationality maps AIS Maritime Identification Digits (ITU MID, the first
// three MMSI digits) to Lattice nationality literals. Only literals verified to
// exist in SDK v5.2.0 are listed; unknown MIDs are omitted, never guessed
// (SPEC §3.4.3). Flags without a Lattice literal (e.g. Marshall Islands,
// Bahamas, Vanuatu, Hong Kong) are intentionally absent.
var midNationality = map[string]Lattice.MilViewNationality{}

func init() {
	add := func(n Lattice.MilViewNationality, mids ...string) {
		for _, m := range mids {
			midNationality[m] = n
		}
	}
	add("NATIONALITY_ARGENTINA", "701")
	add("NATIONALITY_URUGUAY", "770")
	add("NATIONALITY_BRAZIL", "710")
	add("NATIONALITY_CHILE", "725")
	add("NATIONALITY_PARAGUAY", "755")
	add("NATIONALITY_CHINA", "412", "413", "414")
	add("NATIONALITY_TAIWAN", "416")
	add("NATIONALITY_REPUBLIC_OF_KOREA", "440", "441")
	add("NATIONALITY_JAPAN", "431", "432")
	add("NATIONALITY_SPAIN", "224", "225")
	add("NATIONALITY_PORTUGAL", "263")
	add("NATIONALITY_ITALY", "247")
	add("NATIONALITY_GERMANY", "211", "218")
	add("NATIONALITY_NETHERLANDS", "244", "245", "246")
	add("NATIONALITY_UNITED_KINGDOM", "232", "233", "234", "235")
	add("NATIONALITY_NORWAY", "257", "258", "259")
	add("NATIONALITY_GREECE", "237", "239", "240", "241")
	add("NATIONALITY_RUSSIA", "273")
	add("NATIONALITY_MALTA", "215", "229", "248", "249", "256")
	add("NATIONALITY_UNITED_STATES_OF_AMERICA", "338", "366", "367", "368", "369")
	add("NATIONALITY_PANAMA", "351", "352", "353", "354", "355", "356", "357", "370", "371", "372", "373", "374")
	add("NATIONALITY_LIBERIA", "636", "637")
	add("NATIONALITY_SINGAPORE", "563", "564", "565", "566")
}

// NationalityFromMMSI returns the flag-state literal for a ship-station MMSI
// (9 digits, MID first), or false when unknown.
func NationalityFromMMSI(mmsi string) (Lattice.MilViewNationality, bool) {
	if len(mmsi) != 9 || mmsi[0] < '2' || mmsi[0] > '7' {
		return "", false // not a ship station (coast stations, SAR, AtoN, …)
	}
	n, ok := midNationality[mmsi[:3]]
	return n, ok
}
