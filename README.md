# unifi-ups-exporter

A deliberately minimal Prometheus exporter for the embedded NUT server on
Ubiquiti UniFi UPS devices.

## Why this exists

As of July 2026, the NUT server on UniFi UPS devices doesn't fully comply with the NUT specification
(see threads [here](https://community.ui.com/questions/UniFi-UPS-sends-incorrect-responses-in-NUT-protocol/7f862573-eda4-4180-a987-0aca1233ee8a)
and [here](https://community.ui.com/questions/UI-UPS-full-NUT-support/af7afa20-bd55-4e07-920e-212ae1c4ed09)).

General-purpose NUT exporters (such as DRuggeri's [`nut_exporter`](https://github.com/DRuggeri/nut_exporter),
HON95's [Rust exporter](https://github.com/HON95/prometheus-nut-exporter),
Telegraf's [`upsd` input](https://github.com/influxdata/telegraf/tree/master/plugins/inputs/upsd))
all fail against the UniFi UPS as a result of this.

## Usage

Multi-target exporter pattern (like blackbox/snmp_exporter):

```
GET /metrics?target=<host[:port]>
```

`:3493` is assumed if no port is given. A single instance scrapes many UPSs;
fan out targets in Prometheus/Alloy relabelling.

### Flags

| Flag                    | Default   | Description                              |
|-------------------------|-----------|------------------------------------------|
| `-web.listen-address`   | `:9199`   | Listen address.                          |
| `-web.telemetry-path`   | `/metrics`| Path for UPS metrics (expects `?target=`)|
| `-nut.default-port`     | `3493`    | Port used when a target omits one.       |
| `-nut.timeout`          | `5s`      | Overall timeout per target scrape.       |

## Metrics

Numeric NUT variables are emitted as gauges under the `network_ups_tools_`
namespace with an `ups` label, matching the naming used by existing NUT
Grafana dashboards (such as [this](https://github.com/DRuggeri/nut_exporter/blob/master/dashboard/dashboard.json)):

```
network_ups_tools_battery_charge{ups="argon"} 100
network_ups_tools_input_voltage{ups="argon"} 243
network_ups_tools_ups_load{ups="argon"} 12
```

`ups.status` is fanned out to one series per active flag:

```
network_ups_tools_ups_status{ups="argon",flag="OL"} 1
network_ups_tools_ups_status{ups="argon",flag="CHRG"} 1
```

Selected string variables become labels on an info metric:

```
network_ups_tools_device_info{ups="argon",manufacturer="Ubiquiti",model="UniFi UPS"} 1
```

Per-scrape health (an unreachable UPS yields `scrape_success 0` with HTTP 200,
so one dead unit never fails the others):

```
network_ups_tools_scrape_success 1
network_ups_tools_scrape_duration_seconds 0.0007
```

Non-numeric variables that aren't in the info set are skipped.

## Deploy

### Swarm stack

```yaml
services:
  nut-exporter:
    image: ghcr.io/mattdy/unifi-ups-exporter:latest
    # Optional flags (defaults shown):
    # command:
    #   - "-nut.timeout=5s"
    #   - "-nut.default-port=3493"
    #   - "-web.listen-address=:9199"
    networks:
      - monitoring
    deploy:
      replicas: 1
      restart_policy:
        condition: any
    # Publish only if something outside the overlay network needs to reach it.
    # ports:
    #   - "9199:9199"

networks:
  monitoring:
    external: true
```

### Alloy scrape configuration

```river
// Scrape all three UniFi UPS units through one exporter instance.
// The exporter fans out via the ?target= query parameter, so a single
// prometheus.scrape drives all three.

discovery.relabel "unifi_ups" {
  targets = [
    { "__address__" = "192.168.1.200" },
    { "__address__" = "192.168.1.201" },
    { "__address__" = "192.168.1.202" },
  ]

  // exporter expects ?target=<host>; it appends :3493 if no port is given
  rule {
    source_labels = ["__address__"]
    target_label  = "__param_target"
  }

  // keep the UPS IP as the instance label
  rule {
    source_labels = ["__param_target"]
    target_label  = "instance"
  }

  // scrape the exporter, not the UPS
  rule {
    target_label = "__address__"
    replacement  = "nut-exporter:9199"
  }
}

prometheus.scrape "unifi_ups" {
  targets         = discovery.relabel.unifi_ups.output
  forward_to      = [prometheus.remote_write.default.receiver]
  job_name        = "unifi-ups"
  metrics_path    = "/metrics"
  scrape_interval = "30s"
}
```

## Scope / notes

- v1 has no TLS or auth on the HTTP side.
- The exporter's own process metrics (Go runtime) are not exposed; Alloy's
  `up` for the scrape covers liveness.
- No support for username/password at this time
