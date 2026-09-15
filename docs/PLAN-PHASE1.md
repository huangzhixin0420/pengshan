# 蓬山 Phase 1 实现规划

> 状态：待审（拍板点见 §7，拍板前不动码）
> 输入拍板：daemon 路线 = **fork hermes-link 思路自研**（2026-09-15）
> 参考实现：github.com/HangbinYang/hermes-link（Python/FastAPI，MIT，~8.9k 行，已通读）

## 1. 定位与边界

蓬山 = 跑在用户 Mac 上的青鸟配套守护进程：设备配对、relay 反连、E2E 加密通道、本地 `hermes serve` 桥接。普通用户装完即用，无需公网 IP / cloudflared / 端口映射。

**做**：配对与设备管理、出站反连 relay、app↔daemon E2E 加密通道（X25519 + ChaCha20-Poly1305）、WS 透传到本地 serve、CLI + 安装脚本 + 自启 + 更新。

**不做（Phase 1 明确裁剪）**：

- REST 控制面。hermes-link 的 `api.py`（1.1k 行）+ `hermes_adapter.py`（1.6k 行）+ `execution.py`（0.6k 行）是给 REST/SSE 型客户端准备的 hermes 控制代理；青鸟的客户端已经完整讲 serve 的 WS JSON-RPC 协议（~180 方法、流式、补拉），蓬山只做**字节级透传**，不翻译协议。
- 限流（`rate_limit.py`）。v1 单用户设备数 ≤  handful，serve 自身的 auth gate 已在。
- 多节点聚合、账户/计费（Phase 2 商业化再说）。
- Gateway 拉起（npm 版 link 管 `hermes gateway run`；蓬山不管，serve 由用户/hermes 自己拉起，蓬山启动时探测本地 serve 端口）。

## 2. 总体架构

```
iOS 青鸟 ──① LAN 直连（优先）──→ hermes serve（本机 127.0.0.1:9121）
   │
   └──② WSS 隧道（零配置回退）──→ relay（出境单节点，只转发密文）
                                     ↑ 出站反连（daemon 主动连出）
                              蓬山 daemon（Go 单二进制，launchd 常驻）
                                     │ 帧解密 → 明文 WS
                                     └──→ hermes serve 127.0.0.1:<port>/api/ws
```

青鸟侧改动极小：GatewayClient 的"地址择优"从「LAN + 隧道」两地址扩成「LAN + 隧道 + relay 隧道」三地址，relay 隧道里跑的仍是同一套 serve JSON-RPC（含 ticket 认证、流式、events.since 补拉）。

### 蓬山内部模块

```
cmd/pengshan/            CLI 入口（pair/start/stop/status/logs/doctor/update）
cmd/pengshan-relay/      relay 服务端入口（同一仓，见 §7-P2）
internal/
  identity/              daemon 长期密钥对（X25519，首次启动生成，钥匙串/0600 文件）
  pairing/               配对码、QR 载荷、设备注册/吊销、SQLite 仓储、审计
  tunnel/                E2E 帧层：握手（混合 ECDH）、ChaCha20-Poly1305 seal/open、分帧/重组
  relayclient/           出站 control 连接、心跳、重连退避、拓扑上报
  bridge/                tunnel ↔ 本地 serve WS 双向透传（帧解密后原样转发）
  serveprobe/            本地 serve 端口探测/健康检查（供 status/doctor 与择优）
  autostart/             launchd（Mac）/ systemd（Linux）plist/unit 安装卸载
  update/                GH Release 检查、自替换、重启
```

估算 2–3k 行 Go（stdlib `crypto/ecdh` + `golang.org/x/crypto/{chacha20poly1305,hkdf}` + `gorilla/websocket` 或 `coder/websocket` + `modernc.org/sqlite` 纯 Go SQLite 免 cgo）。

## 3. 从 hermes-link 拿什么 / 不拿什么

| hermes-link 模块 | 行数 | 蓬山取舍 |
|---|---|---|
| `relay.py` | 643 | **改造拿拓扑**：bootstrap（refresh/access token）→ control WS → 心跳 20s → 重连退避 3→60s → 状态上报。弃其 HTTP-over-WS 代理帧（`http.request/response.*`），换成 E2E 隧道帧（§4.3） |
| `security.py` | 333 | **拿模型拿简**：pairing code + access/refresh token + 设备吊销 + 审计的结构照抄，scope 体系简化（v1 全部设备同权，不做 admin/细分） |
| `cli.py` | 1086 | **拿命令面**：pair / start / stop / status / logs / doctor / autostart / unpair / devices，中英 i18n 同款骨架 |
| `storage.py` | 919 | **拿表结构**：devices / pairing_sessions / tokens / audit_events 四表，SQLite，token 只存 sha256 |
| `network.py` | 354 | **拿思路**：LAN IP 检测 + topology 快照上报，给 relay 侧做「LAN 优先、relay 回退」的择优提示 |
| `autostart.py` | 180 | **拿双平台**：launchd + systemd 模板 |
| `i18n.py` | 755 | **拿骨架**（文案量按实际命令面缩） |
| `api.py` / `hermes_adapter.py` / `execution.py` / `service.py` / `control_plane.py` / `rate_limit.py` | ~3.9k | **不拿**：REST 控制面与 hermes 控制代理整体裁掉（§1） |

关键差异一句话：hermes-link 是「app 经 relay 发 HTTP → daemon 本地 REST 再调 hermes」，**REST 代理型**；蓬山是「app 经 relay 建 E2E 隧道 → daemon 透传本地 serve WS」，**WS 隧道型**。差异的根因是青鸟客户端已经讲 serve 协议，而 hermes-link 的客户端讲它自己的 REST。

## 4. 核心协议

### 4.1 E2E 通道（隧道内帧全部密文）

密钥学（Go `crypto/ecdh` X25519）：

- **长期身份密钥**：daemon 一对（首次启动生成，存 `~/.pengshan/identity.key`，0600）；每台已配对设备一对（app 侧生成，私钥留 iOS Keychain）。
- **配对时**：app 把设备公钥经配对通道交给 daemon（配对码授权），daemon 存设备公钥。
- **每次隧道建立**：双方各生成临时密钥对，按 3-DH 混合派生——`HKDF-SHA256(ECDH(dev长期, psn长期) ‖ ECDH(dev临时, psn长期) ‖ ECDH(dev长期, psn临时) ‖ transcript_hash)`——分出 rx/tx 两个 ChaCha20-Poly1305 密钥。临时密钥给前向安全；transcript hash（握手消息序列）防中间人把两条会话拼错对。

帧格式（隧道内）：

```json
{ "v": 1, "n": 42, "t": "data", "b": "<base64 ciphertext>" }
```

- `n` 单调计数器 = nonce，重放即拒；`t ∈ data|ping|pong|close|err`。
- 隧道外（app↔relay、daemon↔relay）relay 只路由不解密，帧里明文带路由元数据（见 §4.3）；payload 本身已是密文。
- 隧道内明文 = 完整的 serve JSON-RPC WS 消息流（app 的 ticket 认证、prompt.submit、message.delta 等原样搬运）。**青鸟协议零改动**。

### 4.2 配对

终端 `pengshan pair`：

1. daemon 生成一次性配对会话 `{session_id, code(8位), expires_at(10min), scopes}`，存 SQLite，打审计。
2. 终端显示 ASCII QR + 数字码。QR 载荷：`{v:1, kind:"pengshan-pair", relay_urls:[...], psn_id, psn_pk, session_id, code, exp}`——含 daemon 公钥（app 需要它来验握手 transcript）。
3. app 扫码：生成设备密钥对 → 经 relay 的配对端点（不经隧道，信封明文但含配对码签名）提交 `{session_id, code, device_id, device_pk, label, platform, 签名}`。
4. daemon 验码（未过期、未用过）→ 注册设备（公钥 + label）→ 双方各自具备 3-DH 全部输入。配对窗口关闭，审计 `pairing.claimed`。
5. 吊销：`pengshan devices revoke <id>` 删公钥即断该设备的隧道能力；`pengshan unpair` 全清重配。

### 4.3 relay 拓扑与帧

relay 是无秘密的转发器（公开仓、可自托管、可第三方审计）：

- **daemon → relay**：一条出站 control WSS（`Authorization: Bearer <access_token>`，bootstrap 流程照抄 hermes-link：`/bootstrap` 换 refresh token → refresh 换 access token）。多设备多隧道在这**一条 socket 上多路复用**，按 `channel_id` 路由：
  - `open {channel_id, device_id}` / `data {channel_id, b64}` / `close {channel_id}` / `ping/pong`
- **app → relay**：每条隧道一条独立 WSS `GET /tunnel/{channel_id}?token=<connect_token>`。connect token = daemon 代设备签的短期 HS256 JWT（TTL 30–60min，scoped 到 device_id），relay 只验签与有效期、不解密 payload——签发密钥（`connect_signing_secret`）由 relay 在 bootstrap 时下发的说法见 §7-P2 备注。
- **心跳/重连**：WS 层 ping 25s/timeout 20s；应用层 20s 心跳；重连指数退避 3→60s，重连成功后 daemon 重放 `open` 重建隧道。
- **拓扑上报**：daemon 每 20s 上报 `{lan_endpoints, serve_status, version}`，relay 存最新快照；app 连 relay 时先查快照——LAN 可达就走 LAN，不可达才开隧道。

### 4.4 本地桥接

- 隧道 `open` → daemon dial `ws://127.0.0.1:<port>/api/ws`（端口来自 `pengshan` 配置或自动探测常见端口），隧道帧解密后原样写入 serve socket，serve socket 帧读出后加密进隧道。纯字节搬运，不理解 JSON-RPC。
- 认证方式见 §7-P1（透传 vs 代铸），两种实现都在 bridge 层内，协议面不变。
- serve 不在线：bridge 拒 `open`（`err: serve_unavailable`），app 收到明确错误；`pengshan doctor` 本地报同一原因。

## 5. CLI 与安装分发

```
pengshan pair [--scope ...]     生成配对码 + QR
pengshan devices list|revoke    设备管理
pengshan unpair                 全量吊销重配
pengshan start|stop|restart     前台/后台服务管理
pengshan status                 设备数 / relay 态 / serve 探测 / 拓扑
pengshan logs [-f] [-n N]       JSONL 日志
pengshan doctor                 装后自检（二进制/serve 可达性/自启态/relay 连通）
pengshan update                 检查 GH Release 自更新
pengshan autostart on|off       launchd / systemd
pengshan version
```

安装：`curl -fsSL https://pengshan.sh/install.sh | bash`（install.sh 在仓内 `scripts/`，从 GH Release 拉对应平台二进制 + sha256 校验；`pengshan.sh` 域名从 §7-P4）。自启默认安装时可选（`HERMES_PENGSHAN_AUTOSTART=1`），同 npm 版 link 语义：用户手动 stop 不自拉起，登录周期才自启。

## 6. 里程碑（每步可验证）

| 里程碑 | 内容 | 验证 |
|---|---|---|
| M1 骨架 | Go module；`cmd/pengshan-relay` 转发器（control 多路复用 + `/tunnel/{id}` 对接，明文 echo）；E2E 帧层（3-DH 握手 + seal/open + 重放拒绝）单测 | Go 测试；本机两进程 echo 往返 |
| M2 配对 | identity/pairing/storage；QR 载荷；claim；吊销；审计 | 单测 + CLI 手工配对流程走通 |
| M3 桥接 | bridge 透传 + serve 探测；青鸟经 relay 全链（ticket→WS→session.list→prompt 流式→events.since） | 模拟器 app 三地址择优接入，跑通真实回合 |
| M4 分发 | install.sh、launchd/systemd、`update`、GH Release 流水线 | 干净 Mac 用户视角演练（卸载态 → 一行安装 → 配对可用） |
| M5 打磨 | status/logs/doctor、拓扑上报择优、重连风暴防护、文档（README 用法 + 协议草案） | doctor 全绿；弱网重连测试 |

M1–M3 协议与加密相关，开发期 relay 用本机 `127.0.0.1`（或 txy 测试实例）即可，不依赖 §7-P3 生产选址。

## 7. 拍板点（规划定稿前需拍）

**P1 隧道内 serve 认证方式**（M3 前必须拍）：

- **A. 纯透传（推荐起步）**：app 的 hermes 密码经 E2E 通道直达 serve 做 password-login→ticket，daemon 全程不持凭证。与「最小依赖、不侵入」一致，密码明文只经过用户自己的两端；代价是用户体验与现状相同（换密码要重输），且 app 仍需持有密码（v1.5 才迁 Keychain）。
- B. daemon 代铸 ticket：配对时把 hermes 密码交给 daemon 存 macOS Keychain，之后 daemon 自动 login 铸 ticket，app 永不再输密码、可设备级吊销。体验最好，但密码多一个落点（虽然是用户本机 Keychain），且 daemon 要管 cookie 生命周期。
- C. daemon 存 refresh cookie：不存密码只存 serve 会话 cookie，到期重定向重输。折中，但 cookie 有效期语义未实证，可能悄悄过期变成隐性 B 的复杂度。

**P2 relay 代码归属**：推荐 **pengshan 仓 monorepo**（`cmd/pengshan` + `cmd/pengshan-relay` + `internal/` 共享帧层）。一个协议两个面，一起演进、一起发版；relay 无秘密（只转发密文 + 配对信封），公开可审计本身是 E2E 卖点的证据。备选：relay 单独闭源仓（商业托管用）。备注：connect token 签发密钥的托管（relay 下发 vs daemon 自管）随 P2 定，推荐 daemon 自管、relay 只验——relay 破防不解密、不越权。

**P3 relay 开发/测试节点**：先用本机或 txy（118.25.22.217）实例，生产选址（近岸 vs 境内）是商业拍板不阻塞开发。无争议即按此执行。

**P4 分发域名**：`pengshan.sh` 新注册 vs 挂现有 `hzxprzp.cn` 子域（如 `get.hzxprzp.cn`）。前者品牌独立、后者零成本。推荐后者起步（install.sh 亦可从 GH raw 拉，域名只是壳）。

## 8. 风险与开放问题

- **protocol drift**：serve WS 协议三消费者共用、稳定；蓬山透传不翻译，天然免疫。隧道帧版本 `v` 字段预留。
- **重连风暴**：app 弱网重连 + daemon 重连叠加可能雪崩——帧层 `n` 计数器 + 重连后显式 `open`（不复用旧 channel）+ 退避上限 60s；M5 压测。
- **iOS 侧改造量**：GatewayClient 多一条 transport（隧道 WS），认证流与流式逻辑不动；择优顺序（LAN → 隧道 → cloudflared 隧道）在 app 侧排。
- **connect token 被盗**：30–60min 短期 + 设备级吊销即时生效；隧道内还有 E2E 第二层（偷了 connect token 只拿到密文管道入口）。
- **未实证**：serve 门禁对 relay 来源 IP/Host 头无要求（桥接走 127.0.0.1，与来源无关，预期无问题，M3 首验）；app 经隧道跑 `session.events.since` 大帧补拉的吞吐（M3 用真实长会话验证）。
