# Prometheus metrics

[Back to the overview](../README.md)

`/metrics` serves all metrics in the Prometheus format. It needs no login by default, like
most exporters, and shows server names, subnets, and counts of devices and findings. To
protect it, set `METRICS_USER` and `METRICS_PASSWORD`: this login opens only this path, and
the login of the interface does not work there. `METRICS_ENABLED=false` removes the path altogether.

    scrape_configs:
      - job_name: kea-roost
        metrics_path: /metrics
        # only with METRICS_USER
        basic_auth:
          username: prometheus
          password_file: /etc/prometheus/kea-roost-password
        static_configs:
          - targets: ["kea-roost:8080"]

The Kea metrics keep the names of the common Kea exporters, so existing dashboards keep
working: `kea_dhcp4_packets_received_total`, `kea_dhcp4_addresses_assigned_total`,
`kea_dhcp4_global4_*`, `kea_dhcp4_pool_*` and so on. Each carries a `server` label, for
HA pairs. The subnet metrics carry the labels of both exporters, `subnet`, `subnet_id`,
`prefix`, `shared_network` and an empty `pool`. Pools are reported in their own
`kea_dhcp4_pool_*` metrics only, so a sum over subnets counts nothing twice.

Kea Roost adds what only it knows:

| Metric | Meaning |
|---|---|
| `roost_server_up`, `roost_server_uptime_seconds` | Each Kea server |
| `roost_ha_state`, `roost_ha_partner_in_touch`, `roost_ha_clock_skew_seconds` | The HA pair |
| `roost_config_differences` | Entries a peer holds differently from the primary |
| `roost_findings`, `roost_rule_findings`, `roost_findings_hidden`, `roost_rules_skipped` | The configuration review |
| `roost_health_checks` | Health checks per result |
| `roost_reservations`, `roost_leases_active` | Counts |
| `roost_devices_online`, `roost_devices_known`, `roost_arp_monitor_up` | From the ARP monitor |
| `roost_wifi_clients`, `roost_ap_dashboard_up` | From the AP dashboard |
| `roost_dns_hosts_file_ok`, `roost_info` | The hosts file and the version |

A scrape runs the same queries as the Health page, so an interval of a minute is enough.
