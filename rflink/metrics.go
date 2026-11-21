/* SPDX-License-Identifier: AGPL-3.0-or-later
 * Copyright (c) 2022-2026 MERKATOR <merkator@merkator.pro>
 * @AGPL-3.0-or-later <https://spdx.org/licenses/AGPL-3.0-or-later.html>
 */

package rflink

import (
	"fmt"
	"strings"
)

// MetricsText is the Prometheus text exposition of process counters and gauges.
// Sensor names are not labels: a scrape should not list the radio neighbourhood.
func (p *Publisher) MetricsText() string {
	if p == nil {
		return ""
	}
	var b strings.Builder
	gauge := func(name, help string, v float64) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s gauge\n%s %g\n", name, help, name, name, v)
	}
	counter := func(name, help string, v uint64) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s counter\n%s %d\n", name, help, name, name, v)
	}
	counter("gorflink_published_total", "Sensor messages published to MQTT.", p.published.Load())
	counter("gorflink_publish_failures_total", "Sensor publishes that failed.", p.publishFailed.Load())
	gauge("gorflink_ping_fails", "Worst consecutive missed RFLink PONGs across gateways. Resets when that gateway answers.", float64(p.worstPingFails()))
	fmt.Fprintf(&b, "# HELP gorflink_dropped_total Messages dropped before publish. serial means that gateway's own queue was full.\n# TYPE gorflink_dropped_total counter\n")
	fmt.Fprintf(&b, "gorflink_dropped_total{reason=\"dedup\"} %d\n", p.dedupDropped.Load())
	fmt.Fprintf(&b, "gorflink_dropped_total{reason=\"throttle\"} %d\n", p.throttleDropped.Load())
	fmt.Fprintf(&b, "gorflink_dropped_total{reason=\"serial\"} %d\n", p.serialDropped.Load())
	fmt.Fprintf(&b, "gorflink_dropped_total{reason=\"pending\"} %d\n", p.pendingDropped.Load())
	ports := p.serialPortSnapshot()
	wrotePortDrops := false
	for _, port := range ports {
		if port.Dropped == 0 {
			continue
		}
		if !wrotePortDrops {
			fmt.Fprintf(&b, "# HELP gorflink_serial_queue_dropped_total Lines discarded because that gateway queue was full.\n# TYPE gorflink_serial_queue_dropped_total counter\n")
			wrotePortDrops = true
		}
		fmt.Fprintf(&b, "gorflink_serial_queue_dropped_total{device=%s} %d\n", promLabel(port.Device), port.Dropped)
	}
	gauge("gorflink_mqtt_connected", "1 when the MQTT session is up.", boolGauge(p.mqttUp.Load()))
	gauge("gorflink_serial_connected", "1 when every configured serial port is open.", boolGauge(p.serialUp.Load()))
	gauge("gorflink_rflink_unresponsive", "1 after the PING failure threshold on any gateway.", boolGauge(p.anyUnresponsive()))
	return b.String()
}

func promLabel(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\n':
			b.WriteString(`\n`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

func boolGauge(v bool) float64 {
	if v {
		return 1
	}
	return 0
}
