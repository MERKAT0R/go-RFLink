/* SPDX-License-Identifier: AGPL-3.0-or-later
 * Copyright (c) 2022-2026 MERKATOR <merkator@merkator.pro>
 * @AGPL-3.0-or-later <https://spdx.org/licenses/AGPL-3.0-or-later.html>
 */

package rflink

import "testing"

func TestTempCelsiusSignBit(t *testing.T) {
	if got := tempCelsiusFromRFLink(0x8066); got != -10.2 {
		t.Fatalf("TEMP=8066: got %v, want -10.2", got)
	}
	if got := tempCelsiusFromRFLink(0x00cd); got != 20.5 {
		t.Fatalf("TEMP=00cd: got %v, want 20.5", got)
	}

	sd, err := SensorDataFromMessage("20;01;Oregon;ID=0001;TEMP=8066;")
	if err != nil {
		t.Fatal(err)
	}
	if sd.Temperature == nil || *sd.Temperature != -10.2 {
		t.Fatalf("parsed temp %#v", sd.Temperature)
	}
	if got := tempCelsiusFromRFLink(0xFFF6); got != -1 {
		t.Fatalf("TEMP=fff6: got %v, want -1", got)
	}
	upm, err := SensorDataFromMessage("20;33;UPM/Esic;ID=0001;TEMP=fff6;HUM=33;BAT=OK;")
	if err != nil {
		t.Fatal(err)
	}
	if upm.Temperature == nil || *upm.Temperature != -1 {
		t.Fatalf("upm temp %#v", upm.Temperature)
	}
	if upm.Humidity == nil || *upm.Humidity != 33 {
		t.Fatalf("upm hum %#v", upm.Humidity)
	}
	f2, err := SensorDataFromMessage("20;01;UPM/Esic F2;ID=0001;HUM=32;")
	if err != nil {
		t.Fatal(err)
	}
	if f2.Humidity == nil || *f2.Humidity != 50 {
		t.Fatalf("f2 hum %#v", f2.Humidity)
	}
	lax, err := SensorDataFromMessage("20;01;LaCrosse;ID=0002;HUM=32;")
	if err != nil {
		t.Fatal(err)
	}
	if lax.Humidity == nil || *lax.Humidity != 50 {
		t.Fatalf("lacrosse hum %#v", lax.Humidity)
	}
}

func TestHumidityHexDoesNotDropFrame(t *testing.T) {
	sd, err := SensorDataFromMessage("20;01;LaCrosse;ID=0002;TEMP=00c8;HUM=4a;")
	if err != nil {
		t.Fatal(err)
	}
	if sd.Humidity == nil || *sd.Humidity != 0x4a {
		t.Fatalf("hex hum: %#v", sd.Humidity)
	}
	if sd.Temperature == nil || *sd.Temperature != 20 {
		t.Fatalf("temp dropped: %#v", sd.Temperature)
	}

	dec, err := SensorDataFromMessage("20;02;Oregon;ID=0003;HUM=42;")
	if err != nil {
		t.Fatal(err)
	}
	if dec.Humidity == nil || *dec.Humidity != 42 {
		t.Fatalf("decimal hum: %#v", dec.Humidity)
	}

	bad, err := SensorDataFromMessage("20;03;Oregon;ID=0004;TEMP=00c8;HUM=zz;")
	if err != nil {
		t.Fatal(err)
	}
	if bad.Humidity != nil {
		t.Fatalf("bad hum kept: %#v", bad.Humidity)
	}
	if bad.Temperature == nil {
		t.Fatal("temp dropped with bad humidity")
	}
}

func TestProtocolSlashBecomesTopicSafeHWID(t *testing.T) {
	sd, err := SensorDataFromMessage("20;2D;UPM/Esic;ID=0001;TEMP=00cf;")
	if err != nil {
		t.Fatal(err)
	}
	if sd.Model != "UPM_Esic" {
		t.Fatalf("model %q", sd.Model)
	}
	if sd.Hwid != "UPM_ESIC_0001" || sd.RawHWID != sd.Hwid {
		t.Fatalf("hwid %q raw %q", sd.Hwid, sd.RawHWID)
	}
	names := parseColonMap("UPM/Esic_0001:Garden")
	if names["UPM_ESIC_0001"] != "Garden" {
		t.Fatalf("map %#v", names)
	}
}

func TestFriendlyNameSurvivesCanonicalHWID(t *testing.T) {
	sd, err := SensorDataFromMessage("20;01;Oregon;ID=0acc;TEMP=00c8;")
	if err != nil {
		t.Fatal(err)
	}
	o := &Options{
		friendlyNameMap: parseColonMap("OREGON_0ACC:Living"),
		hwidMap:         parseColonMap("0ACC:KITCHEN"),
	}
	sd.ApplyCanonicalHWID(o)
	if sd.Hwid != "KITCHEN" || sd.RawHWID != "OREGON_0ACC" {
		t.Fatalf("hwid %q raw %q", sd.Hwid, sd.RawHWID)
	}
	p := &Publisher{opts: o}
	p.applyFriendlyName(sd)
	if sd.FriendlyName != "Living" {
		t.Fatalf("name %q", sd.FriendlyName)
	}
}

func TestCanonicalHWIDFindsSanitizedID(t *testing.T) {
	o := &Options{hwidMap: parseColonMap("ab/cd:SENSOR1")}
	if got := o.CanonicalHWID("ab/cd", "OREGON_AB_CD"); got != "SENSOR1" {
		t.Fatalf("got %q", got)
	}
}

func TestHADefaultEntityIDKeepsObjectID(t *testing.T) {
	if got := haDefaultEntityID("sensor", "LACROSSE_0001_temp"); got != "sensor.LACROSSE_0001_temp" {
		t.Fatal(got)
	}
}

func TestIgnoreListSpacesMatchModel(t *testing.T) {
	set := parseIgnoreList("Byron SX, Keeloq")
	if _, ok := set["BYRON_SX"]; !ok {
		t.Fatalf("missing underscored model: %#v", set)
	}
	if _, ok := set["KEELOQ"]; !ok {
		t.Fatalf("missing keeloq: %#v", set)
	}
}
