/* SPDX-License-Identifier: AGPL-3.0-or-later
 * Copyright (c) 2022-2026 MERKATOR <merkator@merkator.pro>
 * @AGPL-3.0-or-later <https://spdx.org/licenses/AGPL-3.0-or-later.html>
 */

package rflink

import "testing"

func FuzzSensorMessage(f *testing.F) {
	f.Add("20;01;EV1527;ID=0b25f2;SWITCH=04;CMD=ON;")
	f.Add("20;02;PONG;")
	f.Add("20;03;LacrosseV4;ID=0002;TEMP=00cd;HUM=42;BAT=OK;")
	f.Add("")
	f.Fuzz(func(t *testing.T, msg string) {
		_, _ = SensorDataFromMessage(msg)
		_, _ = ParseGatewayMessage(msg)
		_ = dedupKey(msg)
	})
}

func FuzzRFLinkCommand(f *testing.F) {
	f.Add("10;PING;")
	f.Add("10;NewKaku;0cac142;3;ON;")
	f.Add("11;20;01;LacrosseV4;ID=0002;TEMP=00cd;")
	f.Add("")
	f.Fuzz(func(t *testing.T, cmd string) {
		_ = ValidateRFLinkCommand(cmd)
		o := &Options{}
		o.Publish.CommandMaxLen = 256
		_, _ = o.NormalizeCommand(cmd)
	})
}
