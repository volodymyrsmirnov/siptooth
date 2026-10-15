# Setup

Target: Raspberry Pi 5, Raspberry Pi OS Lite 64-bit (trixie), onboard Bluetooth, an Android phone that stays at home. siptooth is one self-configuring binary that runs as your normal user (usually `pi`) as a systemd user service. It never needs sudo; the only administrator step is adding you to group `bluetooth` once.

Everything lives in `~/.siptooth/`:

| Path | Contents |
| --- | --- |
| `~/.siptooth/config.yaml` | configuration (relative paths in it resolve against `~/.siptooth/`) |
| `~/.siptooth/env` | `SIPTOOTH_SIP_PASSWORD=...`, mode 0600 |
| `~/.siptooth/siptooth.sock` | control socket of the running daemon |

Every command defaults to `--config ~/.siptooth/config.yaml` (or `$SIPTOOTH_CONFIG`).

## Steps

1. **Check the prerequisites.** BlueZ (`bluetoothd`, `bluetoothctl`) is installed and running on Raspberry Pi OS trixie. No other audio stack may claim the hands-free profile (PipeWire, PulseAudio, bluealsa, ofono); `siptooth doctor` reports them as `competing_handlers`. Building on the Pi needs Go 1.24 or newer (the linux-arm64 tarball from https://go.dev/dl unpacked in your home directory). Copying a prebuilt binary needs no Go.

2. **Add your user to group `bluetooth`** (administrator, once):

   ```sh
   sudo usermod -aG bluetooth "$USER"
   ```

   A user service gets its groups when the `user@UID` manager starts, so logging out and in is not enough: reboot (or `sudo systemctl restart user@$(id -u)`). `siptooth doctor` reports `bluetooth_group` until the session has the group.

3. **Get the binary onto the Pi** at `~/.local/bin/siptooth` (on the default `PATH` after the next login once the directory exists). Either:

   - from the Mac, in the repository: `make deploy` (cross-builds linux/arm64, copies to `pi@rpi-pt:~/.local/bin/siptooth` and prints the version; override the host with `make deploy PI_HOST=user@host`), or
   - on the Pi: `git clone` the repository, then `make build && install -m 0755 siptooth ~/.local/bin/siptooth`.

4. **Create the config directory:** `siptooth doctor --fix`. It prints every action before doing it, never overwrites an existing file, creates `~/.siptooth/` (0700), a default `config.yaml` and an `env` file (0600), then runs the checks again. It refuses to run as root.

5. **Edit `~/.siptooth/config.yaml`.** Required: `phone.number` (the phone's own number in E.164, needed when `{number}` appears in `sip.target`), `sip.target`, and `bluetooth.phone_address` (steps 7 and 8). Provider-specific values are in `docs/providers.md`. Config rules worth knowing: `sip.local_port` is for `udp` only; credentials never go in a URI (`sip:user:pass@host` is rejected, use `SIPTOOTH_SIP_PASSWORD`); `sip.registrar` is `host` or `host:port`; `nat.rtp_bind` is an IPv4 address; with `sip.register: true` the address of record is `sip.from` when it has no `{placeholders}`, else `sip:<username>@<registrar host>`. Validate with `siptooth config check`, which prints the effective configuration with the password redacted.

6. **Put the SIP password in `~/.siptooth/env`** (skip for a target without digest authentication). Set `sip.username` in the config and:

   ```sh
   SIPTOOTH_SIP_PASSWORD=your-password
   ```

   Keep the file at mode 0600. The password is never logged and never passed to hooks.

7. **Reuse allochka's pairing (no re-pairing).** Copy `bluetooth.phone_address` from `~/.allochka/config.yaml` into `~/.siptooth/config.yaml`. The phone's bond lives in BlueZ, not in either program, so it carries over. Skip step 8.

8. **Or pair afresh with `siptooth pair`.** Remove an old bond first (`bluetoothctl remove AA:BB:CC:DD:EE:FF`; `pair` only detects a newly paired device), then run `siptooth pair`. On the phone open Bluetooth settings, pair with the Pi and confirm the passkey on both sides. The command marks the phone trusted and writes `bluetooth.phone_address` into the config (`--adapter` overrides `bluetooth.adapter`). Make sure the phone allows "Phone calls" / call audio for the device. `pair` refuses while the siptooth service runs.

9. **Stop and disable allochka.** Only one program may own the hands-free profile:

   ```sh
   systemctl --user disable --now allochka
   ```

   `siptooth doctor` warns `allochka_service` while it is still running.

10. **Run the checks:** `siptooth doctor`. Every check should be `ok` except those expected before the first start: `service_unit` ("not installed"), `linger`, `sco_routing` (the Pi's adapter reports manufacturer 305, which is not Broadcom, so expect `sco_routing: skip (manufacturer 305 is not Broadcom)`; on a Broadcom adapter it is a warning that routing cannot be verified without CAP_NET_RAW) and `adapter_powered` if the adapter is off (the service powers it). Network checks (`dns`, `sip_connect`, `sip_options`, `local_address`, `nat_mapping`) show how the target is reached. `--json` gives machine output. Checks: `config`, `env_file`, `bluetooth_group`, `bluetoothd`, `adapter`, `adapter_powered`, `phone_bonded`, `sco_routing`, `competing_handlers`, `allochka_service`, `control_socket`, `linger`, `service_unit`, `build`, `dns`, `network`, `local_address`, `nat_mapping`, `sip_connect`, `sip_options`, `sip_password`, plus `sms_hooks` and `sms_obexd` (reported as skipped while `sms.enabled` is false).

11. **Install and start the service:** `siptooth install`. It writes `~/.config/systemd/user/siptooth.service` and runs `systemctl --user daemon-reload`, enable and restart, then `loginctl enable-linger` so the service starts at boot without a login. `--dry-run` prints the unit and the commands without running them. It refuses as root and while the config does not load. If linger needs administrator rights over SSH, `install` warns and prints `sudo loginctl enable-linger <user>` for you to run. If `systemctl --user` has no user bus (after `su` or `sudo -u`), log in directly as the user and re-run.

12. **Check the daemon:** `siptooth status`. Expect `ready: yes`, `phone connected: yes`, `state: idle` and, when `sip.register` is on, `registration: registered=yes` with the Contact and expiry. `--json` gives machine output.

13. **Watch the logs and make a test call:** `journalctl --user-unit siptooth -f`, then call the phone. A finished call logs one `call ended` line (fields in `docs/operations.md`).

14. **Optional: forward text messages.** Set `sms.enabled: true` in the config, add a hook listening for `sms.received` (copy `config/hooks/sms-log.sh`, see `docs/hooks.md`), restart with `systemctl --user restart siptooth` and run `siptooth hooks test --event sms.received`. When the phone asks for message access, allow it. `siptooth status` should then show `sms:             ready, ...`; if it does not, see "SMS forwarding" in `docs/operations.md`.

## Day to day

- Restart after editing the config: `systemctl --user restart siptooth`.
- Replace the binary: `make deploy` (or `install -m 0755 siptooth ~/.local/bin/siptooth`), then `siptooth install` (refreshes the unit and restarts onto the new binary). `doctor` warns `service_unit` when the unit is stale.
- Debug logging: set `SIPTOOTH_LOG_LEVEL=debug` (values `debug`, `info`, `warn`, `error`) in the service environment.

## BlueZ D-Bus policy

If `doctor` or the daemon reports that BlueZ access is denied even though you are in group `bluetooth`, an administrator adds a D-Bus policy such as `/etc/dbus-1/system.d/siptooth.conf` and restarts D-Bus and BlueZ (or reboots):

```xml
<!DOCTYPE busconfig PUBLIC "-//freedesktop//DTD D-BUS Bus Configuration 1.0//EN"
 "http://www.freedesktop.org/standards/dbus/1.0/busconfig.dtd">
<busconfig>
  <policy group="bluetooth">
    <allow send_destination="org.bluez"/>
    <allow send_interface="org.bluez.Profile1"/>
    <allow send_interface="org.bluez.Agent1"/>
  </policy>
</busconfig>
```

## SCO routing

Broadcom controllers (the Pi's onboard one) carry call audio only when SCO is routed over HCI. Reading or changing that needs CAP_NET_RAW, which siptooth does not have. Raspberry Pi OS routes SCO over HCI at boot (its `bthelper` service), so when the routing cannot be read the service assumes it and answers. If calls are answered but silent, see `docs/operations.md`.
