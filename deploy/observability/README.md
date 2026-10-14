# Monitoring for lith apps

What every lith app on a host shares, installed by the host once, not by each app:

- `lith-apps.json`: the Grafana dashboard "lith apps". Pick an app's metric namespace (the one it gives `lith/prom`), then the app and the slot. It shows the requests by route, the live hub's ticks, renders and frames, the writer, the streams' memory budget, the statements and the process.
- `rules-lith-apps.yaml`: Grafana alert rules for any lith app, one alert per app and slot: a 5xx ratio above 2%, tick overruns, dropped frames, writer failures, refused streams. They land in the folder "lith apps".

Both read the metrics of the apps' own listeners, which the host's Prometheus scrapes from `/etc/prometheus/apps/<app>.yml` with the labels `app` and `slot`. Neither filters on the scrape job or on an app's name, so a new app needs no change here. The guide [Metrics and monitoring](https://lith.zweiundeins.gmbh/docs/guides/monitoring#drop-in-files-for-a-shared-host) has the whole contract between a host and its apps: the folders, the files each app installs, the labels.

## Installing

The host's Grafana needs a Prometheus data source with the uid `prometheus-ds`, and a dashboard provider of type `file`. Take both files from the lith release the apps run (a checkout of its tag), then, as root:

```sh
install -m 0644 lith-apps.json <the dashboard provider's path>/
install -m 0644 rules-lith-apps.yaml /etc/grafana/provisioning/alerting/
systemctl restart grafana-server
```

The provider rereads its folder every `updateIntervalSeconds`, so a new dashboard needs no restart; alert rules load when Grafana starts. A newer lith release may change both files: install them again with it.

`TestDashboard` in `site/internal/app` fails when the dashboard or the rules read a metric that lith's website does not export.
