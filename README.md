# reverse_node

## 自动发布 Windows / Linux amd64

[GitHub Actions: Release Windows and Linux AMD64](https://github.com/qqq694637644/reverse_node/actions/workflows/release.yml) 使用 Ubuntu Runner 交叉编译 `reverse_node-windows-amd64.exe` 和 `reverse_node-linux-amd64`，另发布 `SHA256SUMS` 校验文件。

- **不填版本**：没有稳定 Release 时，从 `0.0.1` 开始；如果最新版为 `0.0.1`，自动发布 `0.0.2`；例如 `0.9.9` 升级为 `0.9.10`。依据已发布的稳定 Release 的最大语义版本，忽略草稿、预发布及非版本标签。
- **指定版本**：在 workflow 的可选 `version` 参数填 `0.1.0`；已存在的 tag / Release 不会被手动流程覆盖。
- **Tag 触发**：推送 `0.1.0` 或 `v0.1.0` Git tag 也会触发构建与发布。
- **测试与发布**：先运行 Go 测试、`go vet` 和版本规则测试，再构建 Windows/Linux amd64、生成 SHA-256 校验值并创建 GitHub Release。二进制的 `reverse_node version` 命令会显示发布版本。

GitHub CLI 不指定版本直接触发：

```powershell
gh workflow run release.yml --repo qqq694637644/reverse_node --ref main
```

需要指定版本时：

```powershell
gh workflow run release.yml --repo qqq694637644/reverse_node --ref main -f version=0.0.3
```

### 下载 Windows 最新版

在 **Windows PowerShell** 中执行：

```powershell
Invoke-WebRequest -Uri "https://github.com/qqq694637644/reverse_node/releases/latest/download/reverse_node-windows-amd64.exe" -OutFile ".\reverse_node.exe"
.\reverse_node.exe version
```

以上下载的是 reverse_node 启动器，不含 Xray 内核；请另外准备兼容的 Xray-core 26.3.27，并通过 `--xray-path` 指向其 `xray.exe` 文件。

Windows 境内节点：使用 **Xray-core 26.3.27 原生 Reverse Portal + Hysteria2 / QUIC Brutal**，替换旧 nat_node 中的 Python RSX3 数据面。该程序不实现第二套隧道协议，只负责生成配置、验证并管理 `xray.exe` 进程。

## 连接方向（重要）

本项目对应的是“境外主动连接境内，业务请求反向穿过隧道”的架构：

```text
境内 Windows（Portal）
浏览器 / 应用
    │ SOCKS5 TCP + UDP
    ▼
127.0.0.1:1080
    │ Xray reverse.portals
    ▼
Hysteria2 / QUIC / TLS / Brutal
[DDNS AAAA]:18443 / UDP
    ▲
    │ 境外 Bridge 主动建立 Hysteria2 连接
    │
境外 Linux VPS（Bridge，现有 x-ui / Xray）
Xray reverse.bridges
    │ 通过现有 routing 选择 WARP / freedom / 其他出口
    ▼
互联网目标
```

- **境外 Bridge 主动拨号到境内 DDNS 的 UDP 端口**，不是境内主动连接 VPS。
- 业务请求从境内 SOCKS5 进入 Portal，经反向隧道交给境外 Bridge，再由 VPS 的 Xray 路由访问目标。
- 境内 SOCKS5 固定绑定回环地址，不会暴露到公网。
- Hysteria2 / QUIC Brutal 由 Xray 内核实现；本项目不依赖 FRPC、FRPS、STUN、XTCP、nat_node 或自定义 RSX3。
- 原 nat_node 仓库不会被修改。它可以保留作为回退方案。

## 兼容要求

使用支持以下功能的 **Xray-core 26.3.27**：

- 原生 `reverse.portals` / `reverse.bridges`
- 原生 `protocol: hysteria`（Hysteria2）
- `finalmask.quicParams.congestion = force-brutal`

当前仓库 CI 使用 `qqq694637644/Xray-core` 固定提交 `d2758a023cd7f4174a5a5fa4ff66e487d4342ba0` 编译测试内核。`reverse_node` 会检查版本，并执行 `xray run -test`；如果使用的 `xray.exe` 不包含所需 Hysteria2 / Brutal 功能，初始化会失败，而不会静默退回 BBR。

## Windows 境内部署

### 1. 准备 DDNS 和 IPv6 入站

1. 将 DDNS 的 **AAAA 记录**更新为境内 Windows 当前可从公网访问的全局 IPv6 地址。
2. 在 Windows 防火墙和光猫的 IPv6 入站过滤中允许 **UDP 18443**（或你选择的端口）。IPv6 通常不需要传统 IPv4 NAT 端口映射，但光猫的 IPv6 防火墙仍可能拦截入站。
3. 不要向公网开放 SOCKS5 `1080`。只开放 Hysteria2 的 UDP 端口。
4. 确保该 DDNS 名称能从境外解析到正确的 IPv6 地址。

### 2. 初始化 Portal

将 `reverse_node.exe` 与 `xray.exe` 放在合适的位置，然后在 PowerShell 7 中运行：

```powershell
./reverse_node.exe init --xray-path "C:/xray/xray.exe" --public-host "your-ddns.example.net" --state-dir "$env:APPDATA/reverse_node" --listen "::" --port 18443 --socks-listen "127.0.0.1" --socks-port 1080 --brutal-up 16mbps --brutal-down 24mbps
```

`--xray-path` 是实际 `xray.exe` 的路径参数，不依赖系统 `PATH`。

默认情况下，程序会：

- 生成 32 字节随机 Hysteria2 认证密钥。
- 生成本地自签名 TLS 证书与私钥，并在境外 Bridge 配置中固定证书 SHA-256 指纹。
- 生成 `xray-portal.json`、`overseas-bridge.fragment.json` 和 `node.json`。
- 使用指定的 Xray 内核运行 `run -test`；只有配置通过校验才会完成初始化。

如果你已有受信任 CA 签发的证书，可同时指定 `--cert` 和 `--key`。程序会验证证书链；如果证书不受系统信任，则自动启用证书固定。需要强制固定已有证书时，可增加 `--pin-cert`。

生成的 `node.json`、`tls-key.pem` 和 `overseas-bridge.fragment.json` 都是敏感文件。尤其 Bridge fragment 内含共享认证密钥；不要提交 Git、公开上传或发到公开工单。

### 3. 在境内启动 Xray Portal

```powershell
./reverse_node.exe check --xray-path "C:/xray/xray.exe" --state-dir "$env:APPDATA/reverse_node"
./reverse_node.exe run --xray-path "C:/xray/xray.exe" --state-dir "$env:APPDATA/reverse_node"
```

`run` 会监控 Xray 进程；异常退出时按指数退避重启，按 Ctrl+C 停止。需要调试时，初始化加 `--log-level debug`；默认 `warning`，避免打印过多会话日志。

## 境外 VPS：配置 Xray Bridge

初始化生成的 `overseas-bridge.fragment.json` 是**配置片段**，不是用于覆盖 VPS 整份 Xray 配置的文件。将它合并到现有 x-ui / Xray 配置中：

1. 将 `reverse.bridges` 项追加到现有 `reverse.bridges`，不要覆盖已经存在的 `reverse.portals`。
2. 将 `outbounds` 中的 `reverse-node-hysteria` 出站追加到现有 `outbounds`。
3. 将 `routing.rules` 的 **`full:reverse-node.internal` 注册规则放到靠前位置**，确保 Bridge 建立注册连接时选中 `reverse-node-hysteria`，不会被通用规则抢先匹配。
4. 将 `inboundTag: ["reverse-node-bridge"]` 的出口规则放在现有域名分流规则之后，作为 fallback。这样现有 WARP 域名规则仍有机会先匹配。
5. 保存并重启 VPS 的 Xray / x-ui 受管内核。确认该内核也为支持 Hysteria2 和 `force-brutal` 的 26.3.27 版本。

默认 `--bridge-egress freedom` 表示未命中已有域名规则时从 VPS 的 freedom 出口访问目标。若希望 fallback 使用现有 WARP 出站，可在境内初始化时使用 `--bridge-egress warp`（替换为实际 outbound tag）。不要把两个 `routing.rules` 项盲目整体追加到通用规则之后：注册规则需要靠前，fallback 规则需要靠后。

Bridge fragment 中的 Hysteria2 出站会主动连接：

- `settings.address`：境内 DDNS 主机名
- `settings.port`：境内 UDP 端口
- `hysteriaSettings.auth`：与境内 Portal 一致的随机认证密钥
- `tlsSettings.serverName`：境内 DDNS 主机名
- `pinnedPeerCertSha256`：默认自签名证书的 SHA-256 pin
- `finalmask.quicParams.congestion`：固定为 `force-brutal`

`brutalUp` / `brutalDown` 按各端自身的上传/下载视角反向对应，因此生成的 Bridge 片段会交换境内两项速率，保证方向含义一致。

## 端口与流量安全

- 公网监听：Hysteria2 **UDP 18443**（可通过 `--port` 修改）。
- 本地 SOCKS5：`127.0.0.1:1080`，TCP + UDP；只允许回环绑定。
- 新架构不再使用旧 nat_node / frpc 的本地 `9312/9313/9314` 通道或路由器 UDP `39999` 打洞映射；只需 DDNS AAAA 与可达的境内 Hysteria2 UDP 端口。旧方案文件仍保留，回退时再按旧文档恢复。
- Portal 将 SOCKS5 入站直接路由到 `reverse-node-portal`。Bridge 不在线时请求会失败，不会自动改走境内直连，避免流量意外泄漏。
- 如果更换了 `--auth`、自签名证书或 DDNS 名称，必须重新生成并同步境外 Bridge fragment。
- `--force` 会轮换认证/证书材料并覆盖本地生成文件；只有准备好同步更新境外配置时才使用。

## CLI

```text
reverse_node init --xray-path <path> --public-host <ddns-host> [options]
reverse_node check --state-dir <path> [--xray-path <path>]
reverse_node run --state-dir <path> [--xray-path <path>] [--restart=false]
reverse_node print-bridge --state-dir <path>
reverse_node version
```

主要初始化选项：

- `--xray-path`：必需，实际 `xray.exe` 路径。
- `--public-host`：必需，境内 DDNS 主机名或公网 IP。
- `--listen` / `--port`：Portal 的公网监听地址和 Hysteria2 UDP 端口。
- `--socks-listen` / `--socks-port`：本地 SOCKS5；listen 必须是回环 IP。
- `--auth`：可选的自定义共享认证；省略时使用安全随机值。
- `--domain`：两端 Xray reverse 的内部匹配域名，默认 `reverse-node.internal`，不需要公开 DNS 记录。
- `--brutal-up` / `--brutal-down`：境内端上传和下载目标，默认 16/24 Mbps。
- `--bridge-egress`：境外 Bridge 的 fallback 出站 tag，默认 `freedom`。
- `--cert` / `--key`：可选的 TLS 证书与私钥 PEM 路径。
- `--state-dir`：本地配置目录，默认系统用户配置目录下的 `reverse_node`。
- `--log-level`：`error`、`warning`、`info` 或 `debug`。

## 测试

```powershell
go test ./...
go vet ./...
go build -o reverse_node.exe .
python scripts/smoke_reverse.py --xray C:/xray/xray.exe --reverse-node ./reverse_node.exe
```

Smoke test 启动两套真实 Xray 进程，验证境内 Portal 到境外 Bridge 的 TCP、SOCKS5 UDP、TLS pin 与两端 Brutal 激活。环回测试通过不代表真实跨境链路一定可达；最终仍需在实际 DDNS、IPv6、防火墙与 VPS 路由环境中联调。
