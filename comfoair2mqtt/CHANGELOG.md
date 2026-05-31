# Changelog

## 0.1.4
- **Self-healing supervisor (no more manual restarts):**
  - The add-on entrypoint now runs `ca350.py` inside a restart-supervisor loop instead of as a one-shot command
  - If the script ever exits — e.g. an MQTT failure or an unhandled serial exception terminates it — it is automatically restarted after 10s, so the add-on recovers without a manual restart
  - Clean stops from Home Assistant (SIGTERM/SIGINT) break the loop instead of restarting (proper `trap` handling, no orphaned child process)
  - **Exponential backoff (10s → 300s)** on restarts: a permanently missing serial device no longer causes a 10s restart storm. A healthy run (≥60s) resets the backoff so ordinary crashes still recover quickly
  - The upstream [hacomfoairmqtt](https://github.com/adorobis/hacomfoairmqtt) code is left completely untouched — this is a wrapper-level change only

## 0.1.3 and earlier
- Wrapper around [hacomfoairmqtt](https://github.com/adorobis/hacomfoairmqtt): exposes its configuration in Home Assistant, provides a working Python environment, and installs/runs the upstream `ca350.py`.
