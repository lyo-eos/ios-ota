# iOS OTA Architecture

Document revision: `1.4.1`

Revised: `2026-09-17`

## Decision

iOS OTA is one unprivileged, launchd-managed Go daemon on macOS. It owns one
verified RemotePairing control connection, one TLS-PSK data connection, one
userspace RSD topology, and serialized app lifecycle requests for one
configured iPhone. Tailscale supplies stable unicast reachability but is not
pairing, session, installation, or removal authority.

The CLI is a short-lived client. It communicates only through a profile-scoped,
owner-only Unix socket. An explicitly configured location-only HTTPS endpoint
accepts a device-scoped credential on the host Tailnet address. It cannot
install/remove apps, read files, or invoke arbitrary developer services.

## iOS OTA location extension and naming

The installed Mac service's canonical display name is **Lyo Nodus iOS OTA**,
with technical slug `lyo-nodus-ios-ota`. Its executable, LaunchAgent label,
plist basename and log basenames use that exact slug. The repository remains
`ios-ota`; Link Core retains its upstream identity. The MacBook's separately
persisted pairing name remains **iOS OTA** and its identity is not rotated.
The Mac mini has its own identity with the owner-selected phone pairing name
**iOS OTA Mac Mini**. These are per-host pairing labels, not alternate service
names or executable aliases.

The daemon owns one requested location and one LocationSimulation connection over
its existing paired tunnel. PUT and DELETE return HTTP 202 with the accepted
state immediately; GET remains responsive during service discovery. A single
worker applies the latest request, refreshes an active coordinate every five
seconds and reopens a failed inner service while retaining the requested target.
Tunnel loss marks the target recovering; a replacement authenticated OTA tunnel
reapplies it. The target lasts until explicit Restore or daemon shutdown, not
until the phone HTTP request, foreground session or one refresh ends. Nothing is
persisted across daemon restarts.

The Instruments socket belongs to the retained location driver. Opening uses
a bounded cancellation link which is detached after successful negotiation;
finishing a command never cancels that socket lifetime. Driver Close ends it.
Each worker attempt has one 60-second context covering fresh RSD identity,
Instruments channel negotiation and the command acknowledgement. Link Core
propagates that context through channel creation and method calls, including
connection cancellation; its old implicit five-second channel deadline is not
used for this operation. A failed attempt reports recovering with its failed
stage, never active. A current successful command is required for active/idle;
fresh phone CoreLocation is still required for system verification.

Restore immediately removes the requested target, cancels an obsolete in-flight
set and makes the worker send stop until acknowledged. Revision checks prevent
late replies from resurrecting an older target. Update supersedes the old
coordinate the same way. Only the worker owns and closes the inner connection;
HTTP/state locks never cover network I/O. Shutdown cancels the worker, which makes a bounded best-effort clear. The daemon
joins it before closing the outer tunnel.

The location listener uses TLS 1.3 and only a local Tailnet IPv4 address. A protected
credential file supplies a certificate and a random device-scoped bearer token.
`configure-location` exports URL, certificate SHA-256 and token for App import.
Only GET/PUT/DELETE `/v1/location` are accepted. Credentials and coordinates are
not logged. Existing profile pairing is reused; no new iPhone VPN is created.

`refresh-location --profile ... --output ...` reissues the location certificate
for the profile's current listener IP and exports a new private connection
document. It preserves the existing private key, bearer token, pairing and
profile. Stop the daemon before refreshing, then restart and import the new
document on the phone. Startup rejects a certificate whose IP or validity does
not match the configured listener, instead of presenting an unusable HTTPS
endpoint as ready. Location errors are English. Certificate renewal and an
authenticated state read are separate from a real location set/restore test.

Set acknowledgement, fresh simulated CoreLocation, persistence with another
app foreground, and fresh real location after clear are distinct evidence.
WeChat and Xiaohongshu acceptance must be separately observed. No universal
app acceptance or bypass of simulated-location detection is promised.

The component name is also the runtime identity:

- executable and process: `lyo-nodus-ios-ota`;
- per-user LaunchAgent and plist basename: `lyo-nodus-ios-ota`;
- default runtime root: `~/Library/Application Support/Lyo Nodus iOS OTA/`;
- explicit runtime-root override: `LYO_NODUS_IOS_OTA_RUNTIME_ROOT`;
- installed executable: `~/.local/bin/lyo-nodus-ios-ota`.

Follow the existing Lyo Nodus App Server Override pattern: one ordinary signed
executable, one same-slug plist/Label, and the canonical human name in service
status output and logs. Do not create an app bundle, association wrapper,
second daemon, helper UI or invented LaunchAgent `DisplayName` key. macOS may
show the technical executable name just as it does for Override; do not claim
a separate system display-name override. Installation does not activate the
service. `RunAtLoad` and `KeepAlive` start
the job at bootstrap; no subsequent force-kickstart restarts it a second time.

Activation is a single service release: first verify the installed identity,
current source ownership, no in-progress install/capture/location operation,
and a reachable Wi-Fi RemotePairing listener. Build and test first, unload the
exact old `ios-ota` job, activate `lyo-nodus-ios-ota`, then remove only the
superseded plist and executable after the new job is authenticated. Preserve
profile/socket paths, phone pairing, location certificate/token and the Core
process. An existing explicitly selected runtime/build root may be reused;
do not duplicate its dependency cache merely to rename a service. No old-name
alias or compatibility job remains. Record actual paths and both old/new
installed versions in release acceptance.

The previously proposed app-bundle association is withdrawn before
implementation at the owner's request; no such bundle was installed.

The superseded executable and LaunchAgent are removed at activation. There is
no command alias or parallel daemon. Profiles are explicit owner-selected
absolute paths; their adjacent control socket is independent of the binary
installation directory. Reuse an existing profile in place when other local
consumers already monitor its socket. This is one configured socket, not an alias.

Renaming the service does not rotate its RemotePairing identity. A local record
passing validation and an open listener are prerequisites, not authentication
proof. On `pair_verify_failed`, use the explicitly authorized device-initiated
bootstrap documented in README. Use the selected host's pairing label and a
host identifier independent of the Mac hostname and Xcode pairing. The
upstream exchange persists that identifier, IRK and keypair together; normal
acquisition reuses the record without changing identity. Preserve the selected
phone's configured profile and location credentials. Pairing acceptance requires
the phone to retain the component in its paired list and an authenticated active
session to survive bootstrap exit. Pairing, Xcode reachability, installation
readback and capture acceptance remain separate observations.

## Build and install ownership

### Independent Mac bridges

MacBook Air and Mac mini may each install the existing 1.3.2 bridge binary as
their own per-user service. Each host owns a separate profile, control socket,
RemotePairing identity and key material. Pair the Mac mini independently;
do not copy the MacBook's pairing record or alter its trusted identity.
Installing a binary does not establish a second authenticated bridge.

Mac mini is intended as the persistent host while MacBook Air remains available
for development and OS reinstallation. Select the host explicitly through its
local CLI, using SSH when operating the other Mac. There is no automatic host
discovery, failover or shared operation lock between the two daemons. Send app
lifecycle and location operations through one selected host at a time; concurrent
phone operations from separate bridges are not an accepted capability.

Each host needs its own first authenticated session while the iPhone exposes
RemotePairing on Wi-Fi. A second bridge does not provide cold acquisition on
pure cellular networking or preserve a session interrupted by replacement of
the phone's own VPN app. Lyo Proxy currently stores one Location host connection;
changing that host requires an explicit profile import after the target bridge
has been paired and its Location endpoint configured.

Mac mini deployment acceptance must separately record binary installation,
independent pairing, LaunchAgent activation and an authenticated active session.
Do not disturb the MacBook bridge to validate installation on the Mac mini.

On September 17, 2026, the existing 1.3.2 binary was installed on Mac mini.
Device-initiated pairing used pymobiledevice3 11.12.4 in an isolated environment,
a fresh host identifier and the requested **iOS OTA Mac Mini** name. The saved
record matched that identity and retained its host IRK and intended phone ID.
Its own profile and pairing record are mode 0600. The per-user LaunchAgent has
RunAtLoad and KeepAlive enabled and reported running. After the pairing process
exited, Mac mini reported `active`, generation 1, while MacBook Air remained
`active`, generation 3, with its previous counters unchanged. No app install,
location operation, VPN toggle or MacBook service restart was performed.

The owner reported that **iOS OTA Mac Mini** was absent from the phone's paired
Mac list after pairing. The outgoing handshake and signed pairing information
both use the requested name, but phone-side list retention is not accepted.
A refreshed list readback is pending; successful transport authentication must
not be reported as proof of the phone's display or attributed to an iOS display
bug without evidence. Keep both working identities intact during diagnosis.

| Stage | Authority |
| --- | --- |
| Source compilation | Xcode / `xcodebuild` |
| Development signing | Xcode and Apple's signing assets |
| Bridge reachability | Tailscale plus the configured iPhone Tailnet IP |
| Pair verification | The configured owner-only RemotePairing record |
| Warm session | The long-running iOS OTA daemon |
| App transfer | Streaming zip conduit over the live RSD session |
| App removal | InstallationProxy for one explicit bundle identifier |
| Success readback | InstallationProxy bundle presence or absence enumeration |

iOS OTA consumes an existing signed `.app`. It does not become an Xcode Run
Destination, select a signing team, modify a project, or weaken iOS signature
validation.

## Session lifecycle

```text
waiting_for_listener -> connecting -> active -> installing   -> active
                                      |    \-> uninstalling -> active
                              |       |            |
                              |       +------------+ service failure
                              |                    v
                              +--------------> recovering
                                                    |
                       later lifecycle operation ---+----> active

outer packet pump terminates -> session_lost -> waiting_for_listener
```

An InstallationProxy, ZipConduit, or RSD request failure does not by itself
destroy the outer warm tunnel. Only termination of the outer packet-dispatch
loop releases the session. This prevents a recoverable service error from being
misreported as successful connectivity while also avoiding self-inflicted cold
reacquisition.

## Network boundary

RemotePairing acquisition requires iOS to expose its listener. Any Wi-Fi
connection can activate it, and Tailscale lets the Mac reach the explicit
iPhone endpoint without Bonjour or a shared LAN. Once verified, both outer TCP
connections remain open while Tailscale changes underlay from Wi-Fi to cellular.

This is warm-session continuity, not pure-cellular discovery. If the outer
session terminates while the phone is cold on 5G, the daemon waits until Wi-Fi
causes iOS to expose RemotePairing again.

## Source and dependency boundary

- Public source mirror:
  `zjz-connect/ios-ota`;
- Git fetch authority: `/srv/contabo/git/ios-ota.git`;
- Go toolchain: `1.26.5` for macOS arm64;
- Link Core upstream: `danielpaulus/go-ios` commit
  `3ebc297691a9e364772aef027744ebc0c49421a5`;
- maintained downstream change: `patches/link-core-tailnet.patch`.

The installer verifies the Go archive checksum, verifies the exact upstream
commit, and fails if the patch no longer applies cleanly. Generated dependency
source, caches, profiles, pairing records, logs, and binaries remain outside
Git under the selected runtime root. Existing protected profile and credential
locations are explicit configuration paths, not naming aliases.

## Security invariants

- Pairing records and profiles must be owner-only regular files.
- Pairing private key material is never copied into a profile, log, or response.
- The RSD-reported identifier must match the configured RemotePairing identifier.
- Install and uninstall requests share one serialization boundary. Install
  accepts only an existing code-signed `.app`; uninstall accepts only one exact
  syntactically valid bundle identifier and never a wildcard.
- Success requires post-install presence readback or post-uninstall absence
  readback.
- No command changes Tailscale ACLs, enables Developer Mode, edits Apple's USB
  pairing database, republishes Bonjour, or falls back to TestFlight.

## v1.1.0 cold-validation contract

The `uninstall --profile <profile> <bundle-id>` command removes one explicitly
named development app and its iOS data container through the daemon-owned warm
session. The short-lived CLI never opens a second RemotePairing tunnel. The
owner-only control request, InstallationProxy removal, and absence readback all
run under the same operation lock as installation. A service failure enters
`recovering` while retaining the outer tunnel; it is never reported as a
successful removal. The command owns no archive, backup, wildcard, bulk-delete,
or implicit current-app policy. Callers must treat successful removal as
irreversible local-data deletion and reinstall the desired signed app
explicitly.

Source commit `4aef0a7` implements this contract as iOS OTA 1.1.0.
Repository unit tests, race tests, `go vet`, the pinned Link Core `ios`,
`tunnel`, `installationproxy`, and `zipconduit` tests, and an independent
versioned binary build passed. At that checkpoint the installed LaunchAgent ran the 1.1.0 binary.
Its first physical cold-removal attempt remains pending because iOS had not
reopened the configured RemotePairing listener after the preceding warm
generation was intentionally closed; no app or data container was removed in
that waiting state.

## v1.0.0 runtime acceptance

The canonical identity cutover passed on 2026-08-31 from source commit
`2c3a1ad72747c56aa8d8d54c44a2f2296da31cd1`:

- the pinned Link Core package tests, repository unit tests, race tests and
  `go vet` passed;
- the installed `ios-ota` reported version `1.0.0`, the pinned Link
  Core commit and patch digest, and its binary SHA-256 was
  `21d34fd59e5cf8903c3017d67480e08a895b836ed8abc190c99fa5057834baa5`;
- the schema-4 profile was copied byte-for-byte with mode `0600`; `doctor`
  accepted build metadata, profile, pairing record, Tailnet peer and the live
  RemotePairing listener;
- `ios-ota` acquired generation 1 and became `active` as
  the only loaded deployment job and process;
- the new daemon reinstalled the exact signed app already present on the phone,
  reached `InstallComplete` and `DataComplete`, performed its own bundle
  readback and advanced `install_count` to 1;
- CoreDevice independently read back the same product, bundle, version and
  build, then completed a terminate-existing foreground launch;
- only after that acceptance, the superseded LaunchAgent plist and runtime
  root were removed. The pairing record remained at its external authority.

## v1.2.0 capture candidate, design revision 6

Capture reuses the existing verified Session and returns raw PNG bytes over the
local control socket. The generation is recorded while the operation lock is
held. Build/test artifacts and the Go workspace belong to the current checkout;
installation and service activation are separate operations. The native XCTest
route replaces the earlier optional WebDriverAgent direction. On 2026-09-08,
user-authorized activation replaced the installed executable with capture preview
3 and restarted the same LaunchAgent without changing its profile or pairing.
The deployment session recovered; physical screenshot acquisition timed out.
See Remote Capture for the activation evidence and outstanding capture checks.

Preview 6 adds native testmanagerd execution to the same serialized session.
The authenticated RSD handshake supplies the device OS version. Selected tests
use the RSD testmanagerd, appservice and openstdio services, without local Xcode
discovery or usbmuxd. A private result directory holds structured completion
results and original PNG attachments. Client disconnect and timeout cancel only
the inner operation. Passing requires each selected method to finish exactly
once and every required PNG to exist and decode completely.

## Build metadata authority

Module version and pinned Link Core commit are Go constants consumed directly by
the CLI. The build script reads the same pinned commit for dependency checkout,
and injects only the actual downstream patch digest. The version command runs
the shared metadata validator before returning success, closing the previous
gap between a passing build/test and a doctor-rejected version.

Capture discovery revision 1.2.0-design.7: screenshot and XCTest operations obtain an identity-verified fresh RSD snapshot over their existing tunnel before using developer services. The acquisition map is installation bootstrap data, not permanent capture readiness. See remote-capture.md.

Capture preparation revision 1.2.0-design.8 revision 2 adds a run-scoped screenshot-only endpoint for native XCTest gestures. The test owner retains the operation lock; an independently opened Instruments service supplies original PNGs to the signed runner. Tailnet-only bind, random per-run token, exclusive request handling and context cancellation bound the endpoint. That preview8 checkpoint was preparation only; the later iOS OTA location release below is now active. See remote-capture.md for clock and acceptance rules.

## iOS OTA 1.3.0 location acceptance, 2026-09-12

The canonical `ios-ota` binary and LaunchAgent are active. The existing explicit
profile and adjacent owner-only socket were retained in place, preserving the
local Lyo Nodus status consumer; pairing was not repeated. The old executable
and LaunchAgent were removed. Both Git authorities now use `ios-ota.git`.

The full pinned dependency, local CLI/session/HTTPS tests, race checks and vet
passed. Lyo Proxy 0.3.0 (8) was installed before the acceptance run. Its native
XCTest then passed actual set, fresh simulated CoreLocation, app switching,
coordinate update, clear and fresh non-simulated CoreLocation. Receipt in the
Lyo Proxy checkout: `.build/location-host-acceptance-build8/result.json`, one
passing test and five screenshots, outer generation 1. Screenshots were visually
inspected. This is not a universal third-party-app acceptance claim.

The host accepts only pinned TLS 1.3 requests from the authenticated client.
The phone needs a narrowly scoped ATS exception for Tailnet IPv4 because iOS
17+ otherwise disallows relaxing IP-certificate trust to this private anchor.
No phone-side VPN configuration is created by location.

### Location renewal acceptance (2026-09-17)

`go test -race ./internal/linkcore ./cmd/lyo-nodus-ios-ota` passed. The service was upgraded to 1.3.2 and its existing certificate refreshed for 100.87.87.50:61443, preserving the key, token and device profile. Normal certificate/hostname verification and authenticated GET returned HTTP 200 with phase idle. The owner-only connection document was staged into the installed phone app Documents/lyo-proxy-location.json through its paired CoreDevice channel. Phone import and a fresh system-location mutation were not exercised.

### Persistent Location 1.4.0 verification (2026-09-17)

The previous read-only service probe required 11.874 seconds before any location
command, exceeding the old ten-second HTTP mutation budget. The fixed design
accepts intent immediately and gives the host worker one deadline through RSD,
channel negotiation and command reply. The complete scripts/test.sh passed
(upstream affected packages, service tests, race tests and vet). Additional DTX
race tests passed for caller-owned deadlines, blocked-write cancellation and
peer disconnect. Service tests cover refresh recovery, replacement tunnels,
Restore racing with a late Set, superseding slow discovery, pending failed
Restore and HTTP cancellation while discovery is blocked.

The inspected 1.4.0 binary was activated on MacBook using the existing profile,
LaunchAgent, certificate, token and pairing. The new daemon acquired generation
1. Live Restore and target-persistence acceptance are in progress; source tests
and service readiness do not establish physical CoreLocation acceptance.

The intermediate 1.4.0 live Restore was accepted in 0.046 seconds and reached
idle after 32.292 seconds during an OTA app transfer. Subsequent lifecycle review
found that Link Core's context-bound socket still inherited the attempt lifetime;
that ownership defect is removed in 1.4.1. A real TCP/DTX fixture must verify the
same socket accepts further commands after the opening and first-command
contexts are cancelled. This intermediate release is not persistent acceptance.

The 1.4.1 complete test entry passed, including the real TCP/DTX lifetime test.
The Instruments and DTX location cases also passed under the race detector.
The existing MacBook LaunchAgent now runs the inspected 1.4.1 binary. Activation
waited for the old job to finish removal before bootstrap; the profile, pairing
and TLS authority were preserved. The current target was captured immediately
before activation and submitted to the new daemon. Physical retention is being
observed separately.
