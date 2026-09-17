# iOS OTA

Mac background service: **Lyo Nodus iOS OTA** (`lyo-nodus-ios-ota`).
The iPhone pairing entry remains exactly **iOS OTA**.

[English](#english) · [中文](#中文)

## English

Install, capture, run native tests and control system location beyond the local network. iOS OTA keeps one
authenticated Apple RemotePairing tunnel open over Tailscale, so a signed iOS
app built on a Mac can be installed on a paired iPhone after the phone roams
from Wi-Fi to 5G.

> [!IMPORTANT]
> Establish the iOS OTA bridge first. Xcode builds and signs the
> app; iOS OTA then installs Xcode's `.app` output through that
> bridge. This project does not make the iPhone appear as a native remote Xcode
> Run Destination.

```text
Xcode build + signing
        |
        v
signed .app -> iOS OTA CLI -> owner-only Unix socket
                                      |
                                      v
                            persistent bridge on macOS
                                      |
                                      v
                              Tailscale -> iPhone
```

### Current network boundary

- The Mac and iPhone may be on different physical networks.
- Both devices must be online in the same Tailscale tailnet.
- The bridge must first become `active` while iOS exposes RemotePairing. Any
  Wi-Fi connection can open that listener; it does not have to be the Mac's
  LAN.
- After the bridge is active, the same session can survive a Wi-Fi-to-5G roam
  and serve repeated installs.
- A cold start on pure 5G is not supported. If the outer session is lost, join
  any Wi-Fi so the daemon can reacquire the listener.

### Requirements

- Apple-silicon Mac with Xcode Command Line Tools;
- iPhone with Developer Mode enabled;
- Tailscale signed into the same tailnet on the Mac and iPhone;
- an Apple RemotePairing record trusted by the iPhone;
- an Xcode project configured with valid iOS development signing.

### 1. Bootstrap the pairing identity

Reuse an existing trusted, owner-only RemotePairing record. Only when creating
or explicitly replacing the component pairing, use an isolated environment with
`pymobiledevice3 11.12.4` and its upstream device-initiated pairing API. Keep the
phone unlocked on the same LAN for this bootstrap. Preserve the normal Xcode
pairing; the component must have its own identity and the exact name **iOS OTA**.

```sh
python3 - <<'PY'
import asyncio, os, subprocess, uuid
from pymobiledevice3.remote.tunnel_service import PairableHostInfo, serve_pairable_host

os.umask(0o077)
async def main():
    info = PairableHostInfo(
        name="iOS OTA",
        model=subprocess.check_output(["sysctl", "-n", "hw.model"], text=True).strip(),
        identifier=str(uuid.uuid4()).upper(),
    )
    print("Select iOS OTA in the iPhone's Paired Macs settings.", flush=True)
    result = await serve_pairable_host(
        info, timeout=240,
        pin_callback=lambda pin: print("Pairing code:", pin, flush=True),
    )
    print("Phone:", result.peer_device.name, result.peer_device.udid)
    print("Record:", result.record_path)
asyncio.run(main())
PY
```

Select **iOS OTA** on the intended phone and enter the short-lived code. Confirm
the returned device identity before using its record. Do not retain the code.
The API persists the new identity and keys together; subsequent daemon
connections reuse them. Do not run this command for normal reconnection.

`remote pair-host --name` alone still uses a hostname-derived identity in this
upstream release. It can collide with a previous component pairing under a
different name. Host-initiated `lockdown remotepairing --pair` also emits a
three-field record without the paired `host_identifier` and `host_alt_irk`
required by this reader. Do not invent those fields or reset all device trust.

The record is normally written under `~/.pymobiledevice3/`. It contains private
key material and must remain readable only by its owner. iOS OTA
reads it in place and never copies it into the repository or profile.

The rename to iOS OTA does not itself invalidate pairing. If `status` reports
`pair_verify_failed`, restore trusted phone connectivity and repeat the bootstrap
for that iPhone. `doctor` checks local prerequisites, not successful pair
verification. Acceptance requires **iOS OTA** to remain in the phone's paired
list and the daemon to remain `active` after the bootstrap exits, not only a
success receipt or momentary connection. Preserve an existing profile, its adjacent socket and any location
configuration; do not recreate the profile just to change its directory name.

### 2. Install the bridge

```sh
git clone https://github.com/zjz-connect/ios-ota.git
cd ios-ota
./scripts/install.sh
```

The deterministic installer downloads the pinned Go toolchain and pinned Link
Core source, applies the repository patch, and installs the binary under:

```text
~/.local/bin/lyo-nodus-ios-ota
```

It does not install Go globally. As with Lyo Nodus App Server Override, the
ordinary executable and LaunchAgent use `lyo-nodus-ios-ota`, with human-facing
service name **Lyo Nodus iOS OTA** in status output and logs. There is no app
bundle or wrapper process. `LYO_NODUS_IOS_OTA_RUNTIME_ROOT` selects an explicit
existing runtime/build root when required; profiles and credentials stay put.

### 3. Configure and start the bridge

Keep the iPhone on any Wi-Fi for this first acquisition. Replace the values
below with the RemotePairing identifier, the iPhone's Tailscale IP, and the
absolute path to its pairing record:

```sh
ios_ota="$HOME/.local/bin/lyo-nodus-ios-ota"
profile="$HOME/Library/Application Support/Lyo Nodus iOS OTA/iphone.json"

"$ios_ota" configure \
  --profile "$profile" \
  --device-label "development iphone" \
  --remote-identifier "REMOTE-PAIRING-IDENTIFIER" \
  --target-tailnet-ip "TAILSCALE-IP" \
  --pair-record "$HOME/.pymobiledevice3/remote_REMOTE-PAIRING-IDENTIFIER.plist"

"$ios_ota" doctor --profile "$profile"
"$ios_ota" launch-agent install --profile "$profile"
"$ios_ota" status --profile "$profile"
```

Do not switch the iPhone to 5G until `status` reports `active`. The default
RemotePairing port is `49152`; pass `--remote-pairing-port` to `configure` only
when the discovered listener uses a different port.

### 4. Build and sign with Xcode

Build a device app with your normal Apple development team and provisioning
profile. For example:

```sh
xcodebuild \
  -project YourApp.xcodeproj \
  -scheme YourApp \
  -configuration Debug \
  -destination 'generic/platform=iOS' \
  -derivedDataPath "$PWD/.build/iOSOTADerivedData" \
  build
```

For a workspace, replace `-project YourApp.xcodeproj` with
`-workspace YourApp.xcworkspace`. A successful device build produces a signed
`.app`, usually under
`.build/iOSOTADerivedData/Build/Products/Debug-iphoneos/`.

### 5. Install Xcode's output through the bridge

Once the bridge is active, the iPhone may roam to 5G. Install the signed result:

```sh
"$ios_ota" install \
  --profile "$profile" \
  "$PWD/.build/iOSOTADerivedData/Build/Products/Debug-iphoneos/YourApp.app"
```

The short-lived CLI submits the request to the persistent daemon. Success is
reported only after streaming zip conduit completes and InstallationProxy reads
the installed bundle back from the iPhone. Later builds reuse the same warm
generation.

For an explicit cold-install test, remove exactly one bundle through the same
warm bridge before reinstalling it:

```sh
"$ios_ota" uninstall \
  --profile "$profile" \
  com.example.YourApp
```

`uninstall` is destructive: iOS removes both the selected app and its local
data container. The command accepts one exact bundle identifier, is serialized
with installs, and succeeds only after InstallationProxy confirms that bundle
is absent. It never selects an app implicitly or accepts a wildcard.

Useful commands:

```sh
"$ios_ota" status --profile "$profile"
"$ios_ota" watch --profile "$profile"
"$ios_ota" stop --profile "$profile"
"$ios_ota" launch-agent remove --profile "$profile"
```

### Development

```sh
./scripts/test.sh
```

The test entry point verifies the pinned downstream patch, affected Link Core
packages, iOS OTA unit tests, race tests, and `go vet`. See
[`docs/architecture.md`](docs/architecture.md) for ownership and failure
boundaries.

iOS OTA is released under the [MIT License](LICENSE). Link Core is
derived from [`danielpaulus/go-ios`](https://github.com/danielpaulus/go-ios);
see [`THIRD_PARTY_NOTICES.md`](THIRD_PARTY_NOTICES.md).

---

## 中文

iOS OTA 用于在局域网之外安装由 Xcode 构建的 App。它通过 Tailscale
持续保有一条经过认证的 Apple RemotePairing 隧道，使 Mac 上由 Xcode 构建并
签名的 iOS App，可以在已配对的 iPhone 从 Wi-Fi 切换到 5G 后继续安装。

> [!IMPORTANT]
> 必须先建立 iOS OTA bridge。Xcode 负责构建和签名，iOS OTA
> 再通过 bridge 安装 Xcode 生成的 `.app`。本项目不会让 iPhone
> 变成 Xcode 原生的远程 Run Destination。

```text
Xcode 构建和签名
        |
        v
已签名 .app -> iOS OTA CLI -> 仅限当前用户的 Unix socket
                                      |
                                      v
                              macOS 常驻 bridge
                                      |
                                      v
                               Tailscale -> iPhone
```

### 当前网络边界

- Mac 和 iPhone 可以位于不同的物理网络。
- 两台设备必须在线并加入同一个 Tailscale tailnet。
- iOS 暴露 RemotePairing listener 时，bridge 必须先进入 `active`。任意 Wi-Fi
  都可以打开该 listener，不要求与 Mac 位于同一个局域网。
- bridge 激活后，同一条 session 可以跨越 Wi-Fi 到 5G 的切换，并用于多次安装。
- 暂不支持纯 5G 冷启动。外层 session 丢失后，需要让 iPhone 加入任意 Wi-Fi，
  daemon 才能重新获取 listener。

### 前置条件

- Apple 芯片 Mac，并安装 Xcode Command Line Tools；
- iPhone 已开启 Developer Mode；
- Mac 和 iPhone 已登录同一个 Tailscale tailnet；
- iPhone 已信任的 Apple RemotePairing record；
- 已正确配置 iOS 开发签名的 Xcode 项目。

### 1. 首次建立配对身份

如果已有可信且仅限当前用户访问的 RemotePairing record，直接复用。
只有首次创建或明确重建组件配对时，才执行上方英文第 1 节的 upstream API
命令：在隔离环境使用 `pymobiledevice3 11.12.4`，广播名称为 **iOS OTA**，
并创建独立的组件身份。手机保持解锁、与 Mac 在同一局域网，在已配对 Mac
设置中选择 **iOS OTA** 并输入临时验证码。保留原有 **MacBook Air** Xcode
配对，正常重连不重新生成身份，不重置全部信任。

仅设置 `pair-host --name` 仍会复用由主机名推导的身份，不等于独立配对。
host-initiated 路径输出的三字段 record 也不满足当前 reader 契约；不要伪造
缺失的身份或密钥字段。核对命令返回的手机身份，验证码不要保存。

record 通常位于 `~/.pymobiledevice3/`。其中包含私钥，只能由当前用户读取。
iOS OTA 会在原路径读取它，不会将其复制到仓库或 profile。

更名本身不会使配对失效。如果 `status` 返回 `pair_verify_failed`，先恢复
可信的手机连接，再对该 iPhone 重做上述配对。`doctor` 通过只代表前置检查
通过，不代表配对认证成功。需确认配对程序退出后，手机已配对列表保留
**iOS OTA**，且 daemon 持续 `active`；短暂连上不算验收。现有 profile、相邻 socket 和 location 配置保留
原位，不要仅为更改目录名而重新运行 `configure`。

### 2. 安装 bridge

```sh
git clone https://github.com/zjz-connect/ios-ota.git
cd ios-ota
./scripts/install.sh
```

可复现安装脚本会下载固定版本的 Go 工具链和 Link Core 源码，应用仓库内的固定
patch，并将二进制文件安装到：

```text
~/.local/bin/lyo-nodus-ios-ota
```

它不会在系统中全局安装 Go。沿用 App Server Override 模式：可执行文件、
Label、plist 统一为 `lyo-nodus-ios-ota`，状态输出和日志使用友好名称
**Lyo Nodus iOS OTA**，不创建应用包或额外服务。macOS 自身可能显示技术名，
不另做系统显示名覆盖。可用 `LYO_NODUS_IOS_OTA_RUNTIME_ROOT` 明确复用已有
runtime/build 目录；不迁移现有 profile、配对或定位凭证。

### 3. 配置并启动 bridge

首次连接时先让 iPhone 保持在任意 Wi-Fi。将下列值替换为 RemotePairing
identifier、iPhone 的 Tailscale IP，以及 pairing record 的绝对路径：

```sh
ios_ota="$HOME/.local/bin/lyo-nodus-ios-ota"
profile="$HOME/Library/Application Support/Lyo Nodus iOS OTA/iphone.json"

"$ios_ota" configure \
  --profile "$profile" \
  --device-label "development iphone" \
  --remote-identifier "REMOTE-PAIRING-IDENTIFIER" \
  --target-tailnet-ip "TAILSCALE-IP" \
  --pair-record "$HOME/.pymobiledevice3/remote_REMOTE-PAIRING-IDENTIFIER.plist"

"$ios_ota" doctor --profile "$profile"
"$ios_ota" launch-agent install --profile "$profile"
"$ios_ota" status --profile "$profile"
```

必须等 `status` 显示 `active` 后，才能把 iPhone 切换到 5G。默认 RemotePairing
端口是 `49152`；只有实际发现的 listener 使用其他端口时，才需要在 `configure`
中传入 `--remote-pairing-port`。

### 4. 使用 Xcode 构建并签名

使用正常的 Apple 开发团队与 provisioning profile 构建设备 App。例如：

```sh
xcodebuild \
  -project YourApp.xcodeproj \
  -scheme YourApp \
  -configuration Debug \
  -destination 'generic/platform=iOS' \
  -derivedDataPath "$PWD/.build/iOSOTADerivedData" \
  build
```

如果使用 workspace，将 `-project YourApp.xcodeproj` 替换为
`-workspace YourApp.xcworkspace`。成功的设备构建会生成已签名 `.app`，通常位于
`.build/iOSOTADerivedData/Build/Products/Debug-iphoneos/`。

### 5. 通过 bridge 安装 Xcode 输出

bridge 激活后，iPhone 可以切换到 5G。安装已签名的构建结果：

```sh
"$ios_ota" install \
  --profile "$profile" \
  "$PWD/.build/iOSOTADerivedData/Build/Products/Debug-iphoneos/YourApp.app"
```

短生命周期 CLI 会把请求交给常驻 daemon。只有 streaming zip conduit 完成，且
InstallationProxy 从 iPhone 回读到该 bundle 后，命令才会报告成功。之后的构建
会复用同一个 warm generation。

如需进行明确的冷安装测试，可先通过同一条 warm bridge 删除一个精确 bundle，
再重新安装：

```sh
"$ios_ota" uninstall \
  --profile "$profile" \
  com.example.YourApp
```

`uninstall` 是破坏性操作：iOS 会同时删除指定 App 及其本地数据容器。命令只接受
一个精确 bundle identifier，与安装操作串行执行，并且只有 InstallationProxy
确认该 bundle 已不存在时才会成功。它不会隐式选择 App，也不接受通配符。

常用命令：

```sh
"$ios_ota" status --profile "$profile"
"$ios_ota" watch --profile "$profile"
"$ios_ota" stop --profile "$profile"
"$ios_ota" launch-agent remove --profile "$profile"
```

### 开发与验证

```sh
./scripts/test.sh
```

测试入口会验证固定的下游 patch、受影响的 Link Core package、iOS OTA
单元测试、race test 和 `go vet`。所有权与失败边界见
[`docs/architecture.md`](docs/architecture.md)。

iOS OTA 使用 [MIT License](LICENSE) 发布。Link Core 衍生自
[`danielpaulus/go-ios`](https://github.com/danielpaulus/go-ios)，完整归属见
[`THIRD_PARTY_NOTICES.md`](THIRD_PARTY_NOTICES.md)。

## Remote capture preview

Capture uses the existing go-ios dependency and pairing identity. Preview 6
is active with native test execution and original screenshot attachments.
Physical tap, swipe and scroll checks passed over cellular with six full-frame
PNGs. Standalone Instruments screenshot has both successful captures and
intermittent timeouts; the final three repeat samples passed, but the earlier
timeout cause remains unexplained;
see [Remote Capture](docs/remote-capture.md) for the separate operation and
network evidence.

The capture candidate uses raw PNG bodies after a bounded JSON result header.
Build and tests are now separate from installation: `scripts/build.sh` writes
only the local `.build/ios-ota`; `scripts/test.sh` never installs it.
The dynamic acquisition route is native XCTest, with WebDriverAgent excluded.
Preview 6 adds `run-tests` for selected signed XCTest methods, full-frame PNG
attachments and explicit completion checks over the existing Tailnet session.
It does not depend on an Xcode Run Destination. See
[Remote Capture](docs/remote-capture.md) revision 1.2.0-design.6 for the command,
network boundary and separately recorded physical acceptance.

The prepared `1.2.0-capture-preview.8` candidate supports a screenshot-only endpoint during `run-tests --capture-bind-address TAILNET_IP`, allowing native in-flight XCTest captures without blocking screenshots behind the gesture. It is not activated by building or testing. See [Remote Capture](docs/remote-capture.md) for its scope and image verification.

## System location

Version `1.3.2` adds `refresh-location --profile "$profile" --output
"/absolute/private/connection.json"` for a changed host Tailnet IP or certificate
renewal. It preserves the existing key, token and phone pairing. Stop the daemon,
refresh, restart it, and import the new document on the phone. Startup validates
the certificate against the configured listener IP.

Version `1.3.0-location-preview.1` adds one host-owned Instruments location
session over the existing paired tunnel. Lyo Proxy controls it using TLS 1.3,
a pinned certificate and a phone-scoped credential. No new phone VPN is created.

Before service activation, provision the connection document:

```sh
"$ios_ota" configure-location --profile "$profile" \
  --listen "HOST_TAILNET_IPV4:61443" --output "/absolute/private/connection.json"
"$ios_ota" launch-agent install --profile "$profile"
```

Import the owner-only document in Lyo Proxy's 主机连接 sheet. Keep it out of Git
and logs. GET/PUT/DELETE `/v1/location` expose only state, coordinate set/update
and clear. The host retains the session when the phone app is suspended. A
command reply is not proof of system readback or acceptance by another app.
Profiles may retain their explicit existing paths so their one control socket
remains available to local consumers; the installed binary and LaunchAgent are
`ios-ota`.
