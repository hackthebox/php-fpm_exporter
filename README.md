# php-fpm_exporter

[![Test](https://github.com/hackthebox/php-fpm_exporter/actions/workflows/test.yml/badge.svg)](https://github.com/hackthebox/php-fpm_exporter/actions/workflows/test.yml)
[![Go Report Card](https://goreportcard.com/badge/github.com/hackthebox/php-fpm_exporter)](https://goreportcard.com/report/github.com/hackthebox/php-fpm_exporter)
[![Go Reference](https://pkg.go.dev/badge/github.com/hackthebox/php-fpm_exporter.svg)](https://pkg.go.dev/github.com/hackthebox/php-fpm_exporter)
[![All Contributors](https://img.shields.io/badge/all_contributors-14-orange.svg?style=flat-square)](#contributors)

> ### This is a fork
>
> Hack The Box's fork of [hipages/php-fpm_exporter](https://github.com/hipages/php-fpm_exporter),
> originally created by Enrico Stahn and hipages, and used here under the Apache-2.0 licence.
> Thank you for the original project.
>
> **We maintain this for Hack The Box's needs. Use at your own risk. Pull requests are welcome;
> no support is promised.** If you need a supported product, this is not one.
>
> Upstream's last release was v2.2.0 in May 2022. This fork carries fixes for crashes and
> incorrect metrics that upstream still has open. See [Differences from upstream](#differences-from-upstream).
>
> Images are published to **GHCR only**, not Docker Hub: `ghcr.io/hackthebox/php-fpm_exporter`.

A [prometheus](https://prometheus.io/) exporter for PHP-FPM.
The exporter connects directly to PHP-FPM and exports the metrics via HTTP.

A webserver such as NGINX or Apache is **NOT** needed!

## Table of Contents

<!-- toc -->

- [Differences from upstream](#differences-from-upstream)
- [Features](#features)
- [Usage](#usage)
  * [Options and defaults](#options-and-defaults)
  * [Why `--phpfpm.fix-process-count`?](#why---phpfpmfix-process-count)
  * [CLI Examples](#cli-examples)
  * [Docker Examples](#docker-examples)
  * [Kubernetes Example](#kubernetes-example)
- [Metrics collected](#metrics-collected)
- [Grafana Dasbhoard for Kubernetes](#grafana-dasbhoard-for-kubernetes)
- [FAQ](#faq)
- [Development](#development)
  * [E2E Tests](#e2e-tests)
- [Contributing](#contributing)
- [Contributors](#contributors)
- [Alternatives](#alternatives)

<!-- tocstop -->

## Differences from upstream

Upstream's last release was [v2.2.0](https://github.com/hipages/php-fpm_exporter/releases) in May 2022.
Everything below is fixed here and, at the time of writing, not upstream.

**Crashes and hangs**

* A malformed `--phpfpm.scrape-uri` no longer panics the process. `url.Parse` returns a nil URL
  alongside its error and the result was dereferenced anyway, inside the scrape goroutine, so a
  single typo took the exporter down. Present upstream since 2018.
* `/metrics` no longer hangs and leaks a goroutine and socket per scrape when a PHP-FPM `/status`
  stalls, for example during a graceful reload. Every request now runs under a connection deadline
  ([upstream #253](https://github.com/hipages/php-fpm_exporter/issues/253), open since 2022).
* Removing a pool no longer races its own loop. Under Kubernetes pod deletion this could index past
  the end of the slice and panic.

**Metrics correctness**

* Every process stage PHP-FPM reports is counted. `Finishing`, `Ending` and `Getting request
  information` sat in empty `switch` cases and vanished from the totals, so `phpfpm_total_processes`
  drifted below the real process count under load
  ([upstream #322](https://github.com/hipages/php-fpm_exporter/issues/322), open since 2023).
* The `Creating` stage is recognised, instead of logging `Unknown process state 'Creating'` on every
  scrape ([upstream #419](https://github.com/hipages/php-fpm_exporter/issues/419)).
* A failed scrape is logged once per pool, not three times.

**CLI behaviour**

* `get --out json` emits valid JSON. `request duration` was rendered as a pointer address, so the
  whole document failed to parse.
* `get` exits non-zero when a target cannot be scraped, and rejects an unknown `--out` value.
* An invalid `--log.level` falls back to `info` with a warning instead of killing the process.

**Additions**

* Kubernetes pod auto-tracking (`--k8s.autotracking`): discovers PHP-FPM pods by label and adds or
  removes them as they come and go, adding a `phpfpm_pod` label. See
  [Kubernetes Example](#kubernetes-example).

## Features

* Export single or multiple pools
* Export to CLI as text or JSON
* Connects directly to PHP-FPM via TCP or Socket
* Maps environment variables to CLI options
* Fix for PHP-FPM metrics oddities
* [Grafana Dashboard](https://grafana.com/dashboards/4912) for Kubernetes

## Usage

`php-fpm_exporter` is released as a [binary](https://github.com/hackthebox/php-fpm_exporter/releases) and a [container image](https://github.com/hackthebox/php-fpm_exporter/pkgs/container/php-fpm_exporter) on GHCR.
It uses sensible defaults which usually avoids the need to use command parameters or environment variables.

`php-fpm_exporter` supports 2 commands, `get` and `server`.
The `get` command allows to retrieve information from PHP-FPM without running as a server and exposing an endpoint.
It prints whatever it managed to collect and exits non-zero if any target could not be scraped, so it can be used in scripts and health checks.
The `server` command runs the server required for prometheus to retrieve the statistics.

### Options and defaults

| Option                 | Description                                           | Environment variable         | Default value   |
|------------------------|-------------------------------------------------------|------------------------------|-----------------|
| `--web.listen-address` | Address on which to expose metrics and web interface. | `PHP_FPM_WEB_LISTEN_ADDRESS` | [`:9253`](https://github.com/prometheus/prometheus/wiki/Default-port-allocations)         |
| `--web.telemetry-path` | Path under which to expose metrics.                   | `PHP_FPM_WEB_TELEMETRY_PATH` | `/metrics`      |
| `--phpfpm.scrape-uri`  | FastCGI address, e.g. unix:///tmp/php.sock;/status or tcp://127.0.0.1:9000/status | `PHP_FPM_SCRAPE_URI` | `tcp://127.0.0.1:9000/status` |
| `--phpfpm.fix-process-count`  | Enable to calculate process numbers via php-fpm_exporter since PHP-FPM sporadically reports wrong active/idle/total process numbers. | `PHP_FPM_FIX_PROCESS_COUNT`| `false` |
| `--log.level`          | Only log messages with the given severity or above. Valid levels: [debug, info, warn, error, fatal] (default "error") | `PHP_FPM_LOG_LEVEL` | info |

### Why `--phpfpm.fix-process-count`?

`php-fpm_exporter` implements an option to "fix" the reported metrics based on the provided processes list by PHP-FPM.

We have seen PHP-FPM provide metrics (e.g. active processes) which don't match reality.
Specially `active processes` being larger than `max_children` and the actual number of running processes on the host.
Looking briefly at the source code of PHP-FPM it appears a scoreboard is being kept and the values are increased/decreased once an action is executed.
The metric `active processes` is also an accumulation of multiple states (e.g. Reading headers, Getting request information, Running).
Which shouldn't matter and `active processes` should still be equal or lower to `max_children`.

`--phpfpm.fix-process-count` will emulate PHP-FPMs implementation including the accumulation of multiple states.
PHP-FPM treats a child as idle only while it is accepting; every other stage (`Creating`, `Reading headers`,
`Getting request information`, `Running`, `Finishing`, `Ending`) counts as active. `total processes` is therefore
always the number of processes PHP-FPM reported.

If you like to have a more granular reporting please use `phpfpm_process_state`.

* https://bugs.php.net/bug.php?id=76003
* https://stackoverflow.com/questions/48961556/can-active-processes-be-larger-than-max-children-for-php-fpm

### CLI Examples

* Retrieve information from PHP-FPM running on `127.0.0.1:9000` with status endpoint being `/status`
  ```
  php-fpm_exporter get
  ```

* Retrieve information from PHP-FPM running on `127.0.0.1:9000` and `127.0.0.1:9001`
  ```
  php-fpm_exporter get --phpfpm.scrape-uri tcp://127.0.0.1:9000/status,tcp://127.0.0.1:9001/status
  ```

* Run as server with 2 pools:
  ```
  php-fpm_exporter server --phpfpm.scrape-uri tcp://127.0.0.1:9000/status,tcp://127.0.0.1:9001/status
  ```

* Run as server and enable process count fix via environment variable:
  ```
  PHP_FPM_FIX_PROCESS_COUNT=1 go run main.go server --web.listen-address ":12345" --log.level=debug
  ```

### Docker Examples

* Run docker manually
  ```
  docker pull ghcr.io/hackthebox/php-fpm_exporter
  docker run -it --rm -e PHP_FPM_SCRAPE_URI="tcp://127.0.0.1:9000/status,tcp://127.0.0.1:9001/status" ghcr.io/hackthebox/php-fpm_exporter
  ```

* Run the docker-compose example
  ```
  git clone git@github.com:hackthebox/php-fpm_exporter.git
  cd php-fpm_exporter/test
  docker-compose -p php-fpm_exporter up
  ```
  You can now access the following links:

  * Prometheus: http://127.0.0.1:9090/
  * php-fpm_exporter metrics: http://127.0.0.1:9253/metrics

  [![asciicast](https://asciinema.org/a/1msR8nqAsFdHzROosUb7PiHvf.png)](https://asciinema.org/a/1msR8nqAsFdHzROosUb7PiHvf)

### Kubernetes Example

Run the exporter with Kubernetes discovery enabled:

```bash
php-fpm_exporter server --k8s.autotracking --k8s.namespace my-namespace --k8s.pod-labels 'php-fpm-exporter/collect=true'
```

When Kubernetes auto-tracking is enabled, every emitted metric includes a `phpfpm_pod` label populated with the discovered PHP-FPM pod name. Static `--phpfpm.scrape-uri` targets have no pod name, so they do not carry the label at all.

## Metrics collected

```
# HELP phpfpm_accepted_connections The number of requests accepted by the pool.
# TYPE phpfpm_accepted_connections counter
# HELP phpfpm_active_processes The number of active processes.
# TYPE phpfpm_active_processes gauge
# HELP phpfpm_idle_processes The number of idle processes.
# TYPE phpfpm_idle_processes gauge
# HELP phpfpm_listen_queue The number of requests in the queue of pending connections.
# TYPE phpfpm_listen_queue gauge
# HELP phpfpm_listen_queue_length The size of the socket queue of pending connections.
# TYPE phpfpm_listen_queue_length gauge
# HELP phpfpm_max_active_processes The maximum number of active processes since FPM has started.
# TYPE phpfpm_max_active_processes counter
# HELP phpfpm_max_children_reached The number of times, the process limit has been reached, when pm tries to start more children (works only for pm 'dynamic' and 'ondemand').
# TYPE phpfpm_max_children_reached counter
# HELP phpfpm_max_listen_queue The maximum number of requests in the queue of pending connections since FPM has started.
# TYPE phpfpm_max_listen_queue counter
# HELP phpfpm_process_last_request_cpu The %cpu the last request consumed.
# TYPE phpfpm_process_last_request_cpu gauge
# HELP phpfpm_process_last_request_memory The max amount of memory the last request consumed.
# TYPE phpfpm_process_last_request_memory gauge
# HELP phpfpm_process_request_duration The duration in microseconds of the requests.
# TYPE phpfpm_process_request_duration gauge
# HELP phpfpm_process_requests The number of requests the process has served.
# TYPE phpfpm_process_requests counter
# HELP phpfpm_process_state The state of the process (Idle, Running, ...).
# TYPE phpfpm_process_state gauge
# HELP phpfpm_scrape_failures The number of failures scraping from PHP-FPM.
# TYPE phpfpm_scrape_failures counter
# HELP phpfpm_slow_requests The number of requests that exceeded your 'request_slowlog_timeout' value.
# TYPE phpfpm_slow_requests counter
# HELP phpfpm_start_since The number of seconds since FPM has started.
# TYPE phpfpm_start_since counter
# HELP phpfpm_total_processes The number of idle + active processes.
# TYPE phpfpm_total_processes gauge
# HELP phpfpm_up Could PHP-FPM be reached?
# TYPE phpfpm_up gauge
```

## Grafana Dasbhoard for Kubernetes

The Grafana dashboard can be found [here](https://grafana.com/dashboards/4912).
There is also a more generic version [here](./grafana/kubernetes-php-fpm.json).

<img src="https://grafana.com/api/dashboards/4912/images/3079/image" width="600">

## FAQ

* **How to update "Metrics collected"?**

  Copy&paste the output from:
  ```
  curl http://127.0.0.1:12345/metrics | grep phpfpm | grep "#"
  ```

## Development

### E2E Tests

The E2E tests are based on docker-compose and bats-core. Install the required components, e.g. via brew on MacOS:

```bash
brew tap kaos/shell
brew install docker-compose bats-core kaos/shell/bats-assert kaos/shell/bats-support
```

After the components are installed run the E2E tests:

```bash
make test-e2e
```

## Contributing

Contributions are greatly appreciated.
The maintainers actively manage the issues list, and try to highlight issues suitable for newcomers.
The project follows the typical GitHub pull request model.
See " [How to Contribute to Open Source](https://opensource.guide/how-to-contribute/) " for more details.
Before starting any work, please either comment on an existing issue, or file a new one.

## Contributors

Thanks goes to these wonderful people ([emoji key](https://github.com/all-contributors/all-contributors#emoji-key)).

Most of this exporter was written before Hack The Box ever forked it. The people below built it, and the
fork stands on their work.

<!-- ALL-CONTRIBUTORS-LIST:START - Do not remove or modify this section -->
<!-- prettier-ignore-start -->
<!-- markdownlint-disable -->
<table>
  <tr>
    <td align="center"><a href="http://enricostahn.com"><img src="https://avatars3.githubusercontent.com/u/362174?v=4?s=100" width="100px;" alt=""/><br /><sub><b>Enrico Stahn</b></sub></a><br /><a href="#question-estahn" title="Answering Questions">💬</a> <a href="https://github.com/hipages/php-fpm_exporter/commits?author=estahn" title="Code">💻</a> <a href="https://github.com/hipages/php-fpm_exporter/commits?author=estahn" title="Documentation">📖</a> <a href="#maintenance-estahn" title="Maintenance">🚧</a> <a href="https://github.com/hipages/php-fpm_exporter/commits?author=estahn" title="Tests">⚠️</a></td>
    <td align="center"><a href="https://github.com/XooR"><img src="https://avatars2.githubusercontent.com/u/120429?v=4?s=100" width="100px;" alt=""/><br /><sub><b>Stanislav Antic</b></sub></a><br /><a href="https://github.com/hipages/php-fpm_exporter/commits?author=XooR" title="Code">💻</a></td>
    <td align="center"><a href="http://herb123456.blogspot.com/"><img src="https://avatars1.githubusercontent.com/u/1568165?v=4?s=100" width="100px;" alt=""/><br /><sub><b>herb</b></sub></a><br /><a href="https://github.com/hipages/php-fpm_exporter/commits?author=herb123456" title="Code">💻</a></td>
    <td align="center"><a href="https://github.com/Nyoroon"><img src="https://avatars1.githubusercontent.com/u/182203?v=4?s=100" width="100px;" alt=""/><br /><sub><b>Smoked Cheese</b></sub></a><br /><a href="https://github.com/hipages/php-fpm_exporter/issues?q=author%3ANyoroon" title="Bug reports">🐛</a> <a href="https://github.com/hipages/php-fpm_exporter/commits?author=Nyoroon" title="Code">💻</a></td>
    <td align="center"><a href="https://www.old-games.ru"><img src="https://avatars3.githubusercontent.com/u/7388179?v=4?s=100" width="100px;" alt=""/><br /><sub><b>Alexander</b></sub></a><br /><a href="https://github.com/hipages/php-fpm_exporter/commits?author=sas1024" title="Code">💻</a></td>
    <td align="center"><a href="https://github.com/stanxing"><img src="https://avatars2.githubusercontent.com/u/23288646?v=4?s=100" width="100px;" alt=""/><br /><sub><b>Stan Xing</b></sub></a><br /><a href="https://github.com/hipages/php-fpm_exporter/commits?author=stanxing" title="Code">💻</a></td>
    <td align="center"><a href="https://github.com/itcsoft54"><img src="https://avatars2.githubusercontent.com/u/22459145?v=4?s=100" width="100px;" alt=""/><br /><sub><b>itcsoft54</b></sub></a><br /><a href="https://github.com/hipages/php-fpm_exporter/commits?author=itcsoft54" title="Code">💻</a></td>
  </tr>
  <tr>
    <td align="center"><a href="http://128.io"><img src="https://avatars0.githubusercontent.com/u/44527?v=4?s=100" width="100px;" alt=""/><br /><sub><b>John S Long</b></sub></a><br /><a href="#infra-adduc" title="Infrastructure (Hosting, Build-Tools, etc)">🚇</a></td>
    <td align="center"><a href="https://github.com/danielocallaghan"><img src="https://avatars.githubusercontent.com/u/62488?v=4?s=100" width="100px;" alt=""/><br /><sub><b>Daniel O'Callaghan</b></sub></a><br /><a href="https://github.com/hipages/php-fpm_exporter/commits?author=danielocallaghan" title="Code">💻</a></td>
    <td align="center"><a href="https://github.com/stchr"><img src="https://avatars.githubusercontent.com/u/166079?v=4?s=100" width="100px;" alt=""/><br /><sub><b>Simon Stücher</b></sub></a><br /><a href="https://github.com/hipages/php-fpm_exporter/issues?q=author%3Astchr" title="Bug reports">🐛</a></td>
    <td align="center"><a href="https://sterba.dev"><img src="https://avatars.githubusercontent.com/u/48120735?v=4?s=100" width="100px;" alt=""/><br /><sub><b>André Sterba</b></sub></a><br /><a href="https://github.com/hipages/php-fpm_exporter/commits?author=andresterba" title="Code">💻</a></td>
  </tr>
  <tr>
    <td align="center"><a href="https://github.com/ClementineM12"><img src="https://avatars.githubusercontent.com/u/106354411?v=4?s=100" width="100px;" alt=""/><br /><sub><b>Christina Moraiti</b></sub></a><br /><a href="https://github.com/hackthebox/php-fpm_exporter/commits?author=ClementineM12" title="Code">💻</a> <a href="https://github.com/hackthebox/php-fpm_exporter/commits?author=ClementineM12" title="Documentation">📖</a></td>
    <td align="center"><a href="https://github.com/vlasopoulos"><img src="https://avatars.githubusercontent.com/u/1096466?v=4?s=100" width="100px;" alt=""/><br /><sub><b>Vasilis Vlasopoulos</b></sub></a><br /><a href="https://github.com/hackthebox/php-fpm_exporter/commits?author=vlasopoulos" title="Code">💻</a></td>
    <td align="center"><a href="https://www.schizas.me/"><img src="https://avatars.githubusercontent.com/u/13113025?v=4?s=100" width="100px;" alt=""/><br /><sub><b>Dimosthenis Schizas</b></sub></a><br /><a href="https://github.com/hackthebox/php-fpm_exporter/commits?author=dimoschi" title="Code">💻</a> <a href="https://github.com/hackthebox/php-fpm_exporter/commits?author=dimoschi" title="Documentation">📖</a> <a href="#infra-dimoschi" title="Infrastructure (Hosting, Build-Tools, etc)">🚇</a> <a href="#maintenance-dimoschi" title="Maintenance">🚧</a> <a href="https://github.com/hackthebox/php-fpm_exporter/commits?author=dimoschi" title="Tests">⚠️</a></td>
  </tr>
</table>

<!-- markdownlint-restore -->
<!-- prettier-ignore-end -->

<!-- ALL-CONTRIBUTORS-LIST:END -->

This project follows the [all-contributors](https://github.com/all-contributors/all-contributors) specification. Contributions of any kind welcome!

## Alternatives

* [bakins/php-fpm-exporter](https://github.com/bakins/php-fpm-exporter)
* [peakgames/php-fpm-prometheus](https://github.com/peakgames/php-fpm-prometheus)
* [craigmj/phpfpm_exporter](https://github.com/craigmj/phpfpm_exporter)
