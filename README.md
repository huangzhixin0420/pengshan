# 蓬山 / Pengshan

> 蓬山此去无多路，青鸟殷勤为探看。

蓬山是 [青鸟（Qingniao）](https://github.com/huangzhixin0420/qingniao) iOS 客户端的配套守护进程：安装在你运行 [Hermes Agent](https://github.com/NousResearch/hermes-agent) 的电脑上，为青鸟提供安全接入——设备配对、本地 `hermes serve` 桥接、与 relay 的端到端加密通道。

**Pengshan is the companion daemon for the Qingniao iOS client. Installed on the machine running your Hermes Agent, it provides secure access for the app: device pairing, local bridging to `hermes serve`, and an end-to-end-encrypted channel through the relay.**

## 设计原则

- **E2E 加密**：relay 只转发密文，服务端不接触对话明文
- **零网络配置**：daemon 出站反连 relay，无需公网 IP / 端口映射
- **最小依赖**：基于官方 `hermes serve` JSON-RPC 面（~180 方法、三消费者共用的稳定协议），不侵入 hermes-agent 本体；隧道内字节透传，零协议翻译

## 架构

```
iOS 青鸟 ──① LAN 直连（优先）──→ hermes serve（本机 127.0.0.1:9121）
   │
   └──② WSS 隧道（零配置回退）──→ pengshan-relay（出境单节点，只转发密文）
                                     ↑ 出站反连（daemon 主动连出）
                              pengshan daemon（Go 单二进制，launchd 常驻）
                                     │ 帧解密 → 明文字节流（反向代理）
                                     └──→ hermes serve 127.0.0.1:<port>
```

隧道内是 **E2E 加密的字节管道**：app 的 HTTP 前奏（认证）与 WS 流式全经 ChaCha20-Poly1305 加密帧传输，daemon 反向代理到本地 serve，不做任何协议解析。协议细节见 [docs/PROTOCOL.md](docs/PROTOCOL.md)。

## 安装

```bash
curl -fsSL https://raw.githubusercontent.com/huangzhixin0420/pengshan/main/scripts/install.sh | bash
```

装到 `~/.pengshan/bin/`，可选自启 `HERMES_PENGSHAN_AUTOSTART=1`。

## 快速开始

```bash
pengshan config set serve_addr 127.0.0.1:9121     # 你的 hermes serve 地址
pengshan config set relay_urls '["ws://你的relay:9400"]'
pengshan pair                                       # 生成二维码给青鸟扫
pengshan autostart on                               # 登录即常驻
pengshan doctor                                     # 八项体检
```

## 命令

| 命令 | 说明 |
|---|---|
| `pair` | 生成配对码 + 终端 QR，10 分钟窗口等 app claim |
| `devices list / revoke <id>` | 已配对设备管理（吊销即时断连） |
| `unpair` | 全量吊销重配 |
| `run` | 前台运行 daemon（调试；`--relay/--serve` 覆盖 config） |
| `config set/get/path` | 读写 `~/.pengshan/config.json` |
| `autostart on/off/status` | 开机自启（登录即起、stop 不自拉起） |
| `update [-v 版本] [--mirror URL]` | 自更新（sha256 校验后替换 + launchd 重启） |
| `doctor` | 版本/权限/配置/serve/relay/自启体检 |
| `logs`（计划中）/ `version` | — |

## 安全模型（摘要）

- 长期身份：X25519 密钥对（daemon 存 `~/.pengshan/identity.key` 0600；app 存 iOS Keychain）
- 每隧道 3-DH 握手（长期+临时混合）→ HKDF 派生方向密钥 → XChaCha20-Poly1305 帧加密，单调序号防重放
- 配对授权：8 位一次性配对码（10 分钟、原子 claim），QR 含 daemon 公钥供 app 验 transcript
- relay 无秘密：不持密钥、不落盘、不解密；完整协议与复算向量见 [PROTOCOL.md](docs/PROTOCOL.md)

## 开发

```bash
go test ./...                  # 全量测试（含 -race 变体）
go test ./internal/relayctl/ -run TestRelayStorm -v   # 100 并发风暴压测
PENGSHAN_UPDATE_VECTOR=1 go test ./internal/e2e/      # 重生成握手测试向量
bash scripts/build.sh vX.Y.Z   # 四平台交叉编译 → dist/ + checksums.txt
```

## 状态

Phase 1 主体完成（M1 协议骨架 / M2 配对 / M3 桥接全链 / M4 分发 / M5 打磨）。relay 生产部署与青鸟 app 侧隧道 transport 接入进行中。

## License

MIT
